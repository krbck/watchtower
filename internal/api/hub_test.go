package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
	"github.com/krbck/watchtower/internal/topologymapper"
)

func testClient(objs ...client.Object) client.Client {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = appsv1.AddToScheme(sch)
	_ = networkingv1.AddToScheme(sch)
	_ = discoveryv1.AddToScheme(sch)
	_ = watchtowerv1alpha1.AddToScheme(sch)
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

// startStack runs a mapper (periodic fallback, fast debounce) and the server
// hub behind an httptest server. It returns the ws URL and a stop function.
func startStack(t *testing.T, c client.Client) (*Server, string, func()) {
	t.Helper()
	m := topologymapper.New(c, nil)
	m.QuietWindow, m.MaxWait = 10*time.Millisecond, 40*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	mapperDone := make(chan struct{})
	go func() {
		defer close(mapperDone)
		_ = m.Start(ctx)
	}()
	waitUntil(t, func() bool { return m.Version() >= 1 })

	s := &Server{Client: c, Mapper: m}
	s.hub = newHub(m, s.listChecks)
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		s.hub.run(ctx, true)
	}()
	ts := httptest.NewServer(s.routes())
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/stream"
	return s, url, func() {
		cancel()
		<-hubDone
		<-mapperDone
		ts.Close()
	}
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

type streamMsg struct {
	Type     string            `json:"type"`
	Checks   []json.RawMessage `json:"checks"`
	Topology *struct {
		Version uint64            `json:"version"`
		Nodes   []json.RawMessage `json:"nodes"`
		Edges   []json.RawMessage `json:"edges"`
	} `json:"topology"`
}

func readMsg(t *testing.T, conn *websocket.Conn) ([]byte, streamMsg) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m streamMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return raw, m
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestHubInitialMessagesAndFanOut(t *testing.T) {
	c := testClient(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&watchtowerv1alpha1.ConnectivityCheck{
			ObjectMeta: metav1.ObjectMeta{Name: "web-check", Namespace: "default"},
			Spec:       watchtowerv1alpha1.ConnectivityCheckSpec{Target: "web"},
		},
	)
	s, url, stop := startStack(t, c)
	defer stop()

	c1, c2 := dial(t, url), dial(t, url)
	defer c1.Close()
	defer c2.Close()

	// Every new client immediately gets the cached sync + topology messages,
	// in the original order and shape.
	for i, conn := range []*websocket.Conn{c1, c2} {
		_, m := readMsg(t, conn)
		if m.Type != "sync" || len(m.Checks) != 1 {
			t.Fatalf("client %d first message = %+v, want sync with 1 check", i, m)
		}
		_, m = readMsg(t, conn)
		if m.Type != "topology" || m.Topology == nil || len(m.Topology.Nodes) == 0 || m.Topology.Version < 1 {
			t.Fatalf("client %d second message = %+v, want topology", i, m)
		}
	}
	waitUntil(t, func() bool { return s.hub.clientCount() == 2 })
	before := s.hub.topoSerializations.Load()

	// A cluster change → one new version → serialized once, delivered to both.
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}); err != nil {
		t.Fatal(err)
	}
	raw1, m1 := readMsg(t, c1)
	raw2, _ := readMsg(t, c2)
	if m1.Type != "topology" || !bytes.Equal(raw1, raw2) {
		t.Fatalf("clients must receive the identical topology message:\n%s\n%s", raw1, raw2)
	}
	if !bytes.Contains(raw1, []byte(`"ns:shop"`)) {
		t.Fatalf("new namespace missing from pushed topology: %s", raw1)
	}
	if got := s.hub.topoSerializations.Load() - before; got != 1 {
		t.Fatalf("topology serialized %d times for one change, want 1", got)
	}

	// A check change → one sync message to both clients.
	if err := c.Create(context.Background(), &watchtowerv1alpha1.ConnectivityCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "api-check", Namespace: "default"},
		Spec:       watchtowerv1alpha1.ConnectivityCheckSpec{Target: "api"},
	}); err != nil {
		t.Fatal(err)
	}
	for i, conn := range []*websocket.Conn{c1, c2} {
		_, m := readMsg(t, conn)
		if m.Type != "sync" || len(m.Checks) != 2 {
			t.Fatalf("client %d: want sync with 2 checks, got %+v", i, m)
		}
	}
}

func TestHubDropsSlowClientWithoutBlocking(t *testing.T) {
	s := &Server{Client: testClient()}
	s.hub = newHub(nil, nil)
	s.hub.sendBuffer = 2
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		s.hub.run(ctx, false)
	}()
	ts := httptest.NewServer(s.routes())
	defer func() {
		cancel()
		<-hubDone
		ts.Close()
	}()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/stream"

	fast := dial(t, url)
	defer fast.Close()
	slow := dial(t, url) // never reads
	defer slow.Close()
	waitUntil(t, func() bool { return s.hub.clientCount() == 2 })

	// 1 MiB messages: the slow peer's socket buffers fill after a few, its
	// writer blocks, its queue fills and the hub must drop it — while the
	// fast client keeps receiving everything and broadcast never blocks.
	payload := bytes.Repeat([]byte("x"), 1<<20)
	const n = 48
	for i := 0; i < n; i++ {
		msg := append([]byte{byte(i)}, payload...)
		start := time.Now()
		s.hub.broadcast(msgTopology, msg)
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("broadcast %d blocked for %v", i, d)
		}
		_ = fast.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, got, err := fast.ReadMessage()
		if err != nil {
			t.Fatalf("fast client read %d: %v", i, err)
		}
		if got[0] != byte(i) || len(got) != len(msg) {
			t.Fatalf("fast client message %d corrupted", i)
		}
	}
	if got := s.hub.clientCount(); got != 1 {
		t.Fatalf("slow client must be dropped: clients=%d", got)
	}
}

func TestHubShutdownClosesClients(t *testing.T) {
	c := testClient(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	s, url, stop := startStack(t, c)
	conn := dial(t, url)
	defer conn.Close()
	readMsg(t, conn) // sync
	readMsg(t, conn) // topology
	waitUntil(t, func() bool { return s.hub.clientCount() == 1 })

	stop()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("client must be closed on shutdown")
	} else if ce, ok := err.(*websocket.CloseError); ok && ce.Code != websocket.CloseGoingAway {
		t.Fatalf("unexpected close code: %v", ce)
	}
	if s.hub.clientCount() != 0 {
		t.Fatal("hub must have no clients after shutdown")
	}
	// Late connections are refused cleanly (no hang).
	if s.hub.register(&wsClient{send: make(chan []byte, 2)}) {
		t.Fatal("register after shutdown must fail")
	}
}

func TestHubKeepalivePing(t *testing.T) {
	s := &Server{Client: testClient()}
	s.hub = newHub(nil, nil)
	s.hub.pingPeriod = 20 * time.Millisecond
	s.hub.pongWait = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		s.hub.run(ctx, false)
	}()
	ts := httptest.NewServer(s.routes())
	defer func() {
		cancel()
		<-hubDone
		ts.Close()
	}()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/stream"

	// A client that answers pings (gorilla's default ping handler replies
	// with a pong while we read) stays connected well past pongWait.
	conn := dial(t, url)
	defer conn.Close()
	pings := make(chan struct{}, 64)
	conn.SetPingHandler(func(data string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	if len(pings) == 0 {
		t.Fatal("server must send keepalive pings")
	}
	if s.hub.clientCount() != 1 {
		t.Fatal("ponging client must stay registered")
	}

	// A client that never reads (so never pongs) is detected and removed.
	dead := dial(t, url)
	defer dead.Close()
	waitUntil(t, func() bool { return s.hub.clientCount() == 2 })
	waitUntil(t, func() bool { return s.hub.clientCount() == 1 })
}
