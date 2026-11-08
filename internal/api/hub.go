package api

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/api/meta"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
	"github.com/krbck/watchtower/internal/topologymapper"
)

// WebSocket hub tuning (D-034).
const (
	// defaultSendBuffer is the per-client queue length; a client whose queue
	// is full is dropped instead of blocking the hub.
	defaultSendBuffer = 16
	// writeWait bounds a single frame write.
	writeWait = 10 * time.Second
	// pongWait: a client that does not answer pings within this is dead.
	pongWait = 60 * time.Second
	// pingPeriod must be shorter than pongWait.
	pingPeriod = (pongWait * 9) / 10
	// checksPollInterval is the fallback cadence for the single centralized
	// check list when no ConnectivityCheck informer is available.
	checksPollInterval = time.Second
	// maxClientMessage caps what we accept from clients (we ignore it anyway).
	maxClientMessage = 4096
)

// wsClient is one WebSocket subscriber. send is owned by the hub: only the
// hub closes it (under hub.mu), exactly once, when it removes the client.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// hub serializes every stream message once and fans it out to all clients.
// One goroutine (run) produces messages: topology on mapper version change,
// checks on ConnectivityCheck informer events (or a 1s centralized poll as a
// fallback). Each client has a buffered queue drained by its own writer
// goroutine, so a slow client never blocks the hub or the other clients.
type hub struct {
	mapper     *topologymapper.Mapper
	listChecks func(ctx context.Context) ([]CheckSummary, error)
	sendBuffer int
	pingPeriod time.Duration
	pongWait   time.Duration

	checksDirty chan struct{}

	mu       sync.Mutex
	clients  map[*wsClient]struct{}
	lastSync []byte
	lastTopo []byte
	topoVer  uint64
	hasTopo  bool
	stopped  bool

	// topoSerializations counts topology json.Marshal calls (tests assert
	// that one change is serialized exactly once regardless of clients).
	topoSerializations atomic.Int64
}

func newHub(m *topologymapper.Mapper, list func(ctx context.Context) ([]CheckSummary, error)) *hub {
	return &hub{
		mapper:      m,
		listChecks:  list,
		sendBuffer:  defaultSendBuffer,
		pingPeriod:  pingPeriod,
		pongWait:    pongWait,
		checksDirty: make(chan struct{}, 1),
		clients:     map[*wsClient]struct{}{},
	}
}

// markChecksDirty raises the non-blocking "checks changed" signal.
func (h *hub) markChecksDirty() {
	select {
	case h.checksDirty <- struct{}{}:
	default:
	}
}

// watchChecks attaches an event handler to the ConnectivityCheck informer.
// It returns a function that removes the handler again.
func (h *hub) watchChecks(ctx context.Context, inf topologymapper.InformerGetter) (func(), error) {
	informer, err := inf.GetInformer(ctx, &watchtowerv1alpha1.ConnectivityCheck{})
	if err != nil {
		return nil, err
	}
	reg, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(interface{}) { h.markChecksDirty() },
		UpdateFunc: func(oldObj, newObj interface{}) {
			if o, err1 := meta.Accessor(oldObj); err1 == nil {
				if n, err2 := meta.Accessor(newObj); err2 == nil &&
					o.GetResourceVersion() != "" && o.GetResourceVersion() == n.GetResourceVersion() {
					return // resync only
				}
			}
			h.markChecksDirty()
		},
		DeleteFunc: func(interface{}) { h.markChecksDirty() },
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = informer.RemoveEventHandler(reg) }, nil
}

// run is the single producer loop. It returns after ctx is cancelled and all
// clients have been told to close.
func (h *hub) run(ctx context.Context, pollChecks bool) {
	logger := log.FromContext(ctx).WithName("api-hub")

	var topoC <-chan uint64
	if h.mapper != nil {
		ch, cancel := h.mapper.Subscribe()
		defer cancel()
		topoC = ch
	}
	var pollC <-chan time.Time
	if pollChecks {
		t := time.NewTicker(checksPollInterval)
		defer t.Stop()
		pollC = t.C
	}

	h.publishChecks(ctx, logger.Error)
	h.publishTopology(logger.Error)

	for {
		select {
		case <-ctx.Done():
			h.shutdown()
			return
		case <-topoC:
			h.publishTopology(logger.Error)
		case <-h.checksDirty:
			h.publishChecks(ctx, logger.Error)
		case <-pollC:
			h.publishChecks(ctx, logger.Error)
		}
	}
}

type errLogger func(err error, msg string, kv ...interface{})

// publishChecks lists checks once (cache read), serializes once and fans out
// only when the payload changed.
func (h *hub) publishChecks(ctx context.Context, logErr errLogger) {
	if h.listChecks == nil {
		return
	}
	checks, err := h.listChecks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logErr(err, "listing checks failed")
		}
		return
	}
	payload, err := json.Marshal(map[string]interface{}{"type": "sync", "checks": checks})
	if err != nil {
		logErr(err, "serializing checks failed")
		return
	}
	h.mu.Lock()
	same := h.lastSync != nil && bytes.Equal(h.lastSync, payload)
	h.mu.Unlock()
	if same {
		return
	}
	h.broadcast(msgSync, payload)
}

// publishTopology serializes the current mapper snapshot once per version.
func (h *hub) publishTopology(logErr errLogger) {
	if h.mapper == nil {
		return
	}
	g := h.mapper.Snapshot()
	h.mu.Lock()
	same := h.hasTopo && h.topoVer == g.Version
	h.mu.Unlock()
	if same {
		return
	}
	h.topoSerializations.Add(1)
	payload, err := json.Marshal(map[string]interface{}{"type": "topology", "topology": g})
	if err != nil {
		logErr(err, "serializing topology failed")
		return
	}
	h.mu.Lock()
	h.topoVer, h.hasTopo = g.Version, true
	h.mu.Unlock()
	h.broadcast(msgTopology, payload)
}

type msgKind int

const (
	msgSync msgKind = iota
	msgTopology
)

// broadcast caches the message as the latest of its kind and enqueues it to
// every client without blocking; clients with a full queue are dropped.
func (h *hub) broadcast(kind msgKind, payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch kind {
	case msgSync:
		h.lastSync = payload
	case msgTopology:
		h.lastTopo = payload
	}
	for c := range h.clients {
		select {
		case c.send <- payload:
		default:
			// Slow consumer: drop it. Closing the conn also unblocks a writer
			// stuck in a socket write.
			h.removeLocked(c)
			_ = c.conn.Close()
		}
	}
}

// register adds a client and immediately queues the latest sync + topology
// messages (in that order, like the original stream). Returns false when the
// hub is shutting down.
func (h *hub) register(c *wsClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return false
	}
	h.clients[c] = struct{}{}
	// The queue is fresh and at least 2 deep, so these never block.
	if h.lastSync != nil {
		c.send <- h.lastSync
	}
	if h.lastTopo != nil {
		c.send <- h.lastTopo
	}
	return true
}

// unregister removes a client (idempotent).
func (h *hub) unregister(c *wsClient) {
	h.mu.Lock()
	h.removeLocked(c)
	h.mu.Unlock()
}

// removeLocked deletes the client and closes its queue exactly once; the
// writer then sends a close frame and closes the connection.
func (h *hub) removeLocked(c *wsClient) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	close(c.send)
}

// shutdown stops accepting clients and closes every client queue.
func (h *hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for c := range h.clients {
		h.removeLocked(c)
	}
}

func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// serve runs one client's lifecycle: a writer goroutine drains the queue
// (plus keepalive pings) while the calling goroutine runs the read loop that
// detects closed peers. It returns once both are done.
func (h *hub) serve(conn *websocket.Conn) {
	buf := h.sendBuffer
	if buf < 2 {
		buf = 2
	}
	c := &wsClient{conn: conn, send: make(chan []byte, buf)}
	if !h.register(c) {
		_ = conn.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.writePump(c)
	}()
	h.readPump(c)
	h.unregister(c)
	<-done
}

// writePump is the only goroutine writing to the connection.
func (h *hub) writePump(c *wsClient) {
	ticker := time.NewTicker(h.pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, ""))
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump consumes (and discards) client frames so control frames (pong,
// close) are processed; a missing pong or a closed peer ends the loop.
func (h *hub) readPump(c *wsClient) {
	c.conn.SetReadLimit(maxClientMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(h.pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(h.pongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(h.pongWait))
	}
}
