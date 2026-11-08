package trace

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// Source is everything the engine reads. Core types come from a cached
// reader; Istio only through the integration snapshot (nil = inactive).
// Gateway API joins in D-038c (integrations/gatewayapi snapshot).
type Source interface {
	Kube() checker.Kube
	Namespaces(ctx context.Context) ([]corev1.Namespace, error)
	Services(ctx context.Context) ([]corev1.Service, error)
	Pods(ctx context.Context) ([]corev1.Pod, error)
	Ingresses(ctx context.Context) ([]networkingv1.Ingress, error)
	IngressClasses(ctx context.Context) ([]networkingv1.IngressClass, error)
	// WorkloadIDs returns the graph IDs (graphid) of every existing
	// Deployment, StatefulSet and DaemonSet.
	WorkloadIDs(ctx context.Context) (map[string]bool, error)
	IstioSnapshot() *istio.Snapshot
}

// CachedSource reads core types from a controller-runtime reader (the
// manager cache — every type is already watched by the topology mapper)
// and Istio from the integration.
type CachedSource struct {
	Reader client.Reader
	// Istio returns the integration snapshot; nil func or nil snapshot =
	// Istio inactive.
	Istio func() *istio.Snapshot
}

var _ Source = CachedSource{}

func (s CachedSource) Kube() checker.Kube { return checker.CachedKube{R: s.Reader} }

func (s CachedSource) Namespaces(ctx context.Context) ([]corev1.Namespace, error) {
	var l corev1.NamespaceList
	err := s.Reader.List(ctx, &l)
	return l.Items, err
}

func (s CachedSource) Services(ctx context.Context) ([]corev1.Service, error) {
	var l corev1.ServiceList
	err := s.Reader.List(ctx, &l)
	return l.Items, err
}

func (s CachedSource) Pods(ctx context.Context) ([]corev1.Pod, error) {
	var l corev1.PodList
	err := s.Reader.List(ctx, &l)
	return l.Items, err
}

func (s CachedSource) Ingresses(ctx context.Context) ([]networkingv1.Ingress, error) {
	var l networkingv1.IngressList
	err := s.Reader.List(ctx, &l)
	return l.Items, err
}

func (s CachedSource) IngressClasses(ctx context.Context) ([]networkingv1.IngressClass, error) {
	var l networkingv1.IngressClassList
	err := s.Reader.List(ctx, &l)
	return l.Items, err
}

func (s CachedSource) WorkloadIDs(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	var deps appsv1.DeploymentList
	if err := s.Reader.List(ctx, &deps); err != nil {
		return nil, err
	}
	for i := range deps.Items {
		out[graphid.Deployment(deps.Items[i].Namespace, deps.Items[i].Name)] = true
	}
	var sts appsv1.StatefulSetList
	if err := s.Reader.List(ctx, &sts); err != nil {
		return nil, err
	}
	for i := range sts.Items {
		out[graphid.StatefulSet(sts.Items[i].Namespace, sts.Items[i].Name)] = true
	}
	var dss appsv1.DaemonSetList
	if err := s.Reader.List(ctx, &dss); err != nil {
		return nil, err
	}
	for i := range dss.Items {
		out[graphid.DaemonSet(dss.Items[i].Namespace, dss.Items[i].Name)] = true
	}
	return out, nil
}

func (s CachedSource) IstioSnapshot() *istio.Snapshot {
	if s.Istio == nil {
		return nil
	}
	return s.Istio()
}

// MaxPathLen caps the request path length.
const MaxPathLen = 2048

// ParseRequest normalizes the query: host lowercased, trailing dot
// stripped, "host:port" split; path defaults to "/" (query string and
// fragment dropped; control characters, <>"'` and paths over MaxPathLen
// are rejected); scheme comes from the param, else a "https://" URL prefix,
// else http (https when the port is 443);
// port defaults to 80/443 by scheme.
func ParseRequest(host, path, port, scheme string) (Request, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	// A pasted URL carries its scheme; an explicit scheme param wins.
	urlScheme := ""
	for _, sch := range []string{"http", "https"} {
		if strings.HasPrefix(h, sch+"://") {
			urlScheme, h = sch, strings.TrimPrefix(h, sch+"://")
		}
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		// "example.com/path" pasted into host: keep the path when none given.
		if path == "" && h[i] == '/' {
			path = h[i:]
		}
		h = h[:i]
	}
	var hostPort string
	if hh, pp, err := net.SplitHostPort(h); err == nil {
		h, hostPort = hh, pp
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return Request{}, fmt.Errorf("host is required")
	}
	if len(h) > 253 {
		return Request{}, fmt.Errorf("host too long")
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return Request{}, fmt.Errorf("invalid host %q (letters, digits, '-' and '.' only)", host)
		}
	}
	if strings.Contains(h, "..") || strings.HasPrefix(h, ".") || strings.HasPrefix(h, "-") {
		return Request{}, fmt.Errorf("invalid host %q", host)
	}

	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		scheme = urlScheme
	}
	switch scheme {
	case "", "http", "https":
	default:
		return Request{}, fmt.Errorf("invalid scheme %q (http or https)", scheme)
	}
	p := strings.TrimSpace(port)
	if p == "" {
		p = hostPort
	} else if hostPort != "" && hostPort != p {
		return Request{}, fmt.Errorf("port %s conflicts with host port %s", p, hostPort)
	}
	var portNum int32
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Request{}, fmt.Errorf("invalid port %q", p)
		}
		portNum = int32(n)
	}
	if scheme == "" {
		scheme = "http"
		if portNum == 443 {
			scheme = "https"
		}
	}
	if portNum == 0 {
		portNum = 80
		if scheme == "https" {
			portNum = 443
		}
	}

	path = strings.TrimSpace(path)
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if len(path) > MaxPathLen {
		return Request{}, fmt.Errorf("path too long (max %d)", MaxPathLen)
	}
	// Defense in depth: the path is echoed back and rendered by the UI.
	for _, r := range path {
		if r < 0x20 || r == 0x7f || strings.ContainsRune("<>\"'`", r) {
			return Request{}, fmt.Errorf("invalid character %q in path", r)
		}
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return Request{Host: h, Path: path, Port: portNum, Scheme: scheme}, nil
}
