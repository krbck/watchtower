// Package api exposes the Watchtower HTTP API: REST endpoints for check
// management and a WebSocket stream for live updates (D-008: REST + WS).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/topologymapper"
	"github.com/krbck/watchtower/internal/trace"
)

// CheckSummary is the UI-facing representation of a ConnectivityCheck.
type CheckSummary struct {
	Name          string                         `json:"name"`
	Namespace     string                         `json:"namespace"`
	Target        string                         `json:"target"`
	ServiceName   string                         `json:"serviceName,omitempty"`
	Port          int32                          `json:"port,omitempty"`
	State         string                         `json:"state,omitempty"`
	Steps         []watchtowerv1alpha1.CheckStep `json:"steps,omitempty"`
	Topology      *watchtowerv1alpha1.Topology   `json:"topology,omitempty"`
	LastCheckedAt *metav1.Time                   `json:"lastCheckedAt,omitempty"`
}

// Server serves the REST + WebSocket API.
type Server struct {
	Client client.Client
	Addr   string

	// Mapper supplies the cluster-wide graph (D-015); optional in tests.
	Mapper *topologymapper.Mapper

	// Informers (optional) lets the stream hub react to ConnectivityCheck
	// informer events instead of polling the check list (D-034).
	Informers topologymapper.InformerGetter

	// Trace (optional) is the host trace source (D-038); nil disables
	// /api/v1/trace and /api/v1/hosts.
	Trace trace.Source

	hub    *hub
	tracer tracer
}

var upgrader = websocket.Upgrader{
	// The UI is served through the same origin (reverse proxy); direct
	// browser access during development is allowed too.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Start implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("api")

	// Wait until the cache can list ConnectivityChecks.
	for {
		var list watchtowerv1alpha1.ConnectivityCheckList
		if err := s.Client.List(ctx, &list); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}

	// One hub per server: serializes each stream message once and fans it
	// out to every WebSocket client (D-034).
	s.hub = newHub(s.Mapper, s.listChecks)
	pollChecks := true
	if s.Informers != nil {
		stopWatch, err := s.hub.watchChecks(ctx, s.Informers)
		if err != nil {
			logger.Error(err, "ConnectivityCheck informer unavailable; polling the check list centrally")
		} else {
			defer stopWatch()
			pollChecks = false
		}
	}
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		s.hub.run(ctx, pollChecks)
	}()

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", s.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		// The hub closes every WebSocket (hijacked connections are not
		// tracked by http.Server.Shutdown).
		<-hubDone
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// routes builds the HTTP handler (CORS-wrapped mux).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/checks", s.handleList)
	mux.HandleFunc("POST /api/v1/checks", s.handleCreate)
	mux.HandleFunc("DELETE /api/v1/checks/{namespace}/{name}", s.handleDelete)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("GET /api/v1/topology", s.handleTopology)
	mux.HandleFunc("GET /api/v1/namespaces", s.handleNamespaces)
	mux.HandleFunc("GET /api/v1/workloads", s.handleWorkloads)
	mux.HandleFunc("GET /api/v1/trace", s.handleTrace)
	mux.HandleFunc("GET /api/v1/hosts", s.handleHosts)
	mux.HandleFunc("GET /api/v1/upstream", s.handleUpstream)
	return cors(mux)
}

// handleList returns all checks across all namespaces.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	checks, err := s.listChecks(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, checks)
}

// handleCreate creates a ConnectivityCheck from a UI form submission.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Namespace   string `json:"namespace"`
		Target      string `json:"target"`
		ServiceName string `json:"serviceName"`
		Port        int32  `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	if req.Target == "" {
		httpError(w, http.StatusBadRequest, "target is required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}

	name := sanitizeName(req.Target)
	if req.ServiceName != "" && req.ServiceName != req.Target {
		name += "-vs-" + sanitizeName(req.ServiceName)
	}
	name += "-check"
	check := &watchtowerv1alpha1.ConnectivityCheck{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: req.Namespace},
		Spec: watchtowerv1alpha1.ConnectivityCheckSpec{
			Target:      req.Target,
			ServiceName: req.ServiceName,
			Port:        req.Port,
		},
	}

	ctx := r.Context()
	if err := s.Client.Create(ctx, check); err != nil {
		// Name collision: if the spec differs (e.g. another port), update the
		// existing check so the form always takes effect.
		if client.IgnoreAlreadyExists(err) == nil {
			var existing watchtowerv1alpha1.ConnectivityCheck
			if getErr := s.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: name}, &existing); getErr == nil {
				if existing.Spec != check.Spec {
					existing.Spec = check.Spec
					if updErr := s.Client.Update(ctx, &existing); updErr != nil {
						httpError(w, http.StatusInternalServerError, updErr.Error())
						return
					}
				}
				writeJSON(w, http.StatusOK, summarize(&existing))
				return
			}
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, summarize(check))
}

// handleDelete removes a ConnectivityCheck.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	if ns == "" || name == "" {
		httpError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}
	check := &watchtowerv1alpha1.ConnectivityCheck{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := s.Client.Delete(r.Context(), check); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTopology returns the current cluster graph (D-015), optionally
// filtered with ?namespace=.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	if s.Mapper == nil {
		httpError(w, http.StatusServiceUnavailable, "topology mapper not enabled")
		return
	}
	writeJSON(w, http.StatusOK, filterGraphByNamespace(s.Mapper.Snapshot(), r.URL.Query().Get("namespace")))
}

// handleNamespaces returns the namespaces visible in the current graph —
// feeds the UI dropdown (D-014).
func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	if s.Mapper == nil {
		httpError(w, http.StatusServiceUnavailable, "topology mapper not enabled")
		return
	}
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, n := range s.Mapper.Snapshot().Nodes {
		if n.Kind != "namespace" {
			continue
		}
		if !seen[n.Label] {
			seen[n.Label] = true
			out = append(out, n.Label)
		}
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, out)
}

// handleWorkloads returns deployable targets (workloads + services) per
// namespace — feeds the UI form autocomplete (D-014).
func (s *Server) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	if s.Mapper == nil {
		httpError(w, http.StatusServiceUnavailable, "topology mapper not enabled")
		return
	}
	nsFilter := r.URL.Query().Get("namespace")
	type entry struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
	}
	out := make([]entry, 0, 64)
	for _, n := range s.Mapper.Snapshot().Nodes {
		switch n.Kind {
		case "deployment", "statefulset", "daemonset", "service":
			if nsFilter != "" && n.Namespace != nsFilter {
				continue
			}
			out = append(out, entry{Namespace: n.Namespace, Name: n.Label, Kind: n.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

// filterGraphByNamespace keeps the namespace node itself plus every node
// that belongs to it (cluster-scoped nodes pass through only on full view).
func filterGraphByNamespace(g *topologymapper.Graph, ns string) *topologymapper.Graph {
	if g == nil || ns == "" {
		return g
	}
	kept := map[string]bool{}
	out := &topologymapper.Graph{Version: g.Version, Nodes: make([]checker.TopologyNode, 0), Edges: make([]checker.TopologyEdge, 0)}
	for _, n := range g.Nodes {
		if n.Kind == "namespace" && n.Label == ns {
			kept[n.ID] = true
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, n := range g.Nodes {
		if kept[n.ID] {
			continue
		}
		if n.Namespace == ns {
			kept[n.ID] = true
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		if kept[e.From] && kept[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// handleStream upgrades to WebSocket and subscribes the connection to the
// hub, which pushes the latest check list (`type: "sync"`) and cluster graph
// (`type: "topology"`) immediately and then on every change (D-034).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		httpError(w, http.StatusServiceUnavailable, "stream not ready")
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.serve(conn)
}

// listChecks lists all checks across namespaces, sorted for stable diffs.
func (s *Server) listChecks(ctx context.Context) ([]CheckSummary, error) {
	var list watchtowerv1alpha1.ConnectivityCheckList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]CheckSummary, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func summarize(c *watchtowerv1alpha1.ConnectivityCheck) CheckSummary {
	s := CheckSummary{
		Name:          c.Name,
		Namespace:     c.Namespace,
		Target:        c.Spec.Target,
		ServiceName:   c.Spec.ServiceName,
		Port:          c.Spec.Port,
		State:         c.Status.State,
		Steps:         c.Status.Steps,
		LastCheckedAt: c.Status.LastCheckedAt,
	}
	if len(c.Status.Topology.Nodes) > 0 {
		topo := c.Status.Topology
		s.Topology = &topo
	}
	return s
}

// sanitizeName converts a target string into a valid k8s object name chunk.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "check"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// cors allows direct browser access during development; in the standard
// deployment the UI is served through the same origin via reverse proxy.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
