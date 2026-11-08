package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/trace"
)

// maxUpstreamTraces caps the host traces one upstream query runs (each goes
// through the shared trace cache + semaphore).
const maxUpstreamTraces = 16

// UpstreamHost is one route claim reaching the Service (B-001).
type UpstreamHost struct {
	trace.UpstreamRoute
	// Service is the Service name the claim points at (the query may
	// resolve several from a target workload).
	Service string `json:"service"`
	// Trace indexes UpstreamResponse.Traces; -1 when not traced (Note set,
	// or the trace cap was reached).
	Trace int `json:"trace"`
	// Reaches reports whether a trace branch ends at the Service.
	Reaches bool `json:"reaches"`
}

// UpstreamResponse answers GET /api/v1/upstream.
type UpstreamResponse struct {
	Namespace string `json:"namespace"`
	// Service is the first resolved Service ("" when none).
	Service  string         `json:"service"`
	Services []string       `json:"services"`
	Hosts    []UpstreamHost `json:"hosts"`
	// Traces are host trace responses cut down to the branches relevant to
	// the Service (trace.KeepBranchesTo).
	Traces    []*trace.Response `json:"traces"`
	Truncated bool              `json:"truncated,omitempty"`
}

// handleUpstream answers GET /api/v1/upstream?namespace=&service=|target=
// (B-001): every north-south route (Istio VirtualService bound to a gateway,
// Ingress rule) whose destination is the Service, traced host by host.
// With target= (a workload or pod name) the Services selecting it are
// resolved first. A read-only live query: no CR, no status (D-038).
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	if s.Trace == nil {
		httpError(w, http.StatusServiceUnavailable, "trace not enabled")
		return
	}
	q := r.URL.Query()
	ns := strings.TrimSpace(q.Get("namespace"))
	svc := strings.TrimSpace(q.Get("service"))
	target := strings.TrimSpace(q.Get("target"))
	if errs := validation.IsDNS1123Label(ns); ns == "" || len(errs) > 0 {
		httpError(w, http.StatusBadRequest, "invalid namespace (DNS-1123 label required)")
		return
	}
	switch {
	case svc == "" && target == "":
		httpError(w, http.StatusBadRequest, "service or target is required")
		return
	case svc != "" && len(validation.IsDNS1123Label(svc)) > 0:
		httpError(w, http.StatusBadRequest, "invalid service name (DNS-1123 label required)")
		return
	case svc == "" && len(validation.IsDNS1123Subdomain(target)) > 0:
		httpError(w, http.StatusBadRequest, "invalid target name (DNS-1123 subdomain required)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), traceRunTimeout)
	defer cancel()
	var services []string
	if svc != "" {
		ok, err := s.serviceExists(ctx, ns, svc)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			httpError(w, http.StatusNotFound, fmt.Sprintf("service %s/%s not found", ns, svc))
			return
		}
		services = []string{svc}
	} else {
		var err error
		if services, err = trace.ServicesForTarget(ctx, s.Trace, ns, target); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	out := UpstreamResponse{Namespace: ns, Services: services, Hosts: []UpstreamHost{}, Traces: []*trace.Response{}}
	if out.Services == nil {
		out.Services = []string{}
	}
	if len(services) > 0 {
		out.Service = services[0]
	}
	traceIdx := map[string]int{}
	for _, name := range services {
		routes, err := trace.RoutesTo(ctx, s.Trace, ns, name)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		svcID := graphid.Service(ns, name)
		for _, rt := range routes {
			h := UpstreamHost{UpstreamRoute: rt, Service: name, Trace: -1}
			if !rt.Traceable() {
				out.Hosts = append(out.Hosts, h)
				continue
			}
			key := fmt.Sprintf("%s|%s|%s|%s|%d|%s", name, rt.Host, rt.Path, rt.Scheme, rt.Port, rt.Route)
			idx, seen := traceIdx[key]
			if !seen {
				if len(out.Traces) >= maxUpstreamTraces {
					out.Truncated = true
					out.Hosts = append(out.Hosts, h)
					continue
				}
				req, err := trace.ParseRequest(rt.Host, rt.Path, fmt.Sprint(rt.Port), rt.Scheme)
				if err != nil {
					// A route value the trace cannot express (e.g. odd
					// characters in a path): report, do not fail the query.
					h.Note = "not traceable: " + err.Error()
					out.Hosts = append(out.Hosts, h)
					continue
				}
				resp, status, err := s.cachedTrace(ctx, req)
				if err != nil {
					httpError(w, status, err.Error())
					return
				}
				idx = len(out.Traces)
				traceIdx[key] = idx
				out.Traces = append(out.Traces, trace.KeepBranchesTo(resp, svcID, rt.Route))
			}
			h.Trace = idx
			t := out.Traces[idx]
			for _, b := range t.Branches {
				for _, n := range b.Highlight.Nodes {
					if n == svcID {
						h.Reaches = true
					}
				}
			}
			if h.Entry == "" && len(t.Branches) > 0 {
				h.Entry = t.Branches[0].Entry
			}
			out.Hosts = append(out.Hosts, h)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serviceExists(ctx context.Context, ns, name string) (bool, error) {
	svcs, err := s.Trace.Services(ctx)
	if err != nil {
		return false, err
	}
	for i := range svcs {
		if svcs[i].Namespace == ns && svcs[i].Name == name {
			return true, nil
		}
	}
	return false, nil
}
