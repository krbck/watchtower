// Package istio is the optional, read-only Istio integration (D-036, D-037).
//
// It detects networking.istio.io through discovery, runs its OWN dynamic
// informers (never the shared controller-runtime cache, whose informer sync
// hangs on GVKs that are not installed) and keeps an atomic snapshot of the
// parsed VirtualServices and Gateways. Objects are parsed from unstructured
// data — no istio.io/client-go dependency.
package istio

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Snapshot is the immutable parsed state consumed by the topology mapper.
// A nil *Snapshot means the integration is inactive.
type Snapshot struct {
	VirtualServices []VirtualService
	Gateways        []Gateway
}

// VirtualService is the subset of networking.istio.io VirtualService the
// map needs.
type VirtualService struct {
	Namespace string
	Name      string
	Hosts     []string
	// Gateways are the raw spec.gateways entries ("ns/name", "name", "mesh").
	Gateways []string
	Routes   []Route
	// HTTP keeps spec.http[] in declaration order with its match
	// conditions — the host trace evaluates first-match semantics (D-038).
	HTTP []HTTPRule
	// TLS keeps spec.tls[] (SNI routing for passthrough servers).
	TLS []TLSRule
}

// HTTPRule is one spec.http[] entry. An empty Match list matches every
// request.
type HTTPRule struct {
	Name  string
	Match []HTTPMatch
	Route []Destination
	// Redirect / DirectResponse are set when the rule answers without a
	// backend (no destination to follow).
	Redirect       bool
	DirectResponse bool
}

// HTTPMatch is one spec.http[].match[] entry; its conditions are ANDed.
type HTTPMatch struct {
	URI       StringMatch
	Authority StringMatch
	Scheme    StringMatch
	Port      uint32
	// Gateways restricts the match to the listed gateways (raw refs).
	Gateways []string
	// IgnoreURICase makes the uri match case-insensitive.
	IgnoreURICase bool
	// Conditional lists request properties the trace cannot predict from a
	// host/path query (headers, method, queryParams, withoutHeaders,
	// sourceLabels, sourceNamespace) — sorted, for stable output.
	Conditional []string
}

// StringMatch is an Istio StringMatch; Kind is "" (match anything),
// "exact", "prefix" or "regex".
type StringMatch struct {
	Kind  string
	Value string
}

// TLSRule is one spec.tls[] entry.
type TLSRule struct {
	SNIHosts []string
	Port     uint32
	Route    []Destination
}

// Destination is one route[] entry.
type Destination struct {
	Host   string
	Port   uint32
	Subset string
	Weight int32
}

// Route is one destination of an http/tcp/tls route rule, flattened.
type Route struct {
	// Protocol is the rule section: http | tcp | tls.
	Protocol string
	// Match is a short human summary of the rule match ("/api", "=/x",
	// "~regex", ":8080", "sni a.com"); "/" or "*" when the rule matches all.
	Match string
	// Host is spec.*[].route[].destination.host as written.
	Host string
	// Port is destination.port.number (0 when unset).
	Port   uint32
	Subset string
	Weight int32
}

// Gateway is the subset of networking.istio.io Gateway the map needs.
type Gateway struct {
	Namespace string
	Name      string
	Selector  map[string]string
	Servers   []Server
}

// Server is one entry of Gateway spec.servers.
type Server struct {
	Port     uint32
	PortName string
	Protocol string
	// Hosts are "[namespace/]dnsName" entries as written.
	Hosts []string
	// TLSMode is tls.mode (SIMPLE, MUTUAL, PASSTHROUGH...; "" without tls).
	TLSMode string
	// HasCredential reports whether tls.credentialName (or file-based
	// certificates) is set. Secrets are never read (D-038).
	HasCredential bool
}

// Key returns "ns/name".
func (v *VirtualService) Key() string { return v.Namespace + "/" + v.Name }

// Key returns "ns/name".
func (g *Gateway) Key() string { return g.Namespace + "/" + g.Name }

// --- unstructured parsing --------------------------------------------------

type rawDestination struct {
	Destination struct {
		Host   string `json:"host"`
		Subset string `json:"subset"`
		Port   struct {
			Number uint32 `json:"number"`
		} `json:"port"`
	} `json:"destination"`
	Weight int32 `json:"weight"`
}

type rawHTTPMatch struct {
	URI             map[string]string      `json:"uri"`
	Authority       map[string]string      `json:"authority"`
	Scheme          map[string]string      `json:"scheme"`
	Method          map[string]string      `json:"method"`
	Headers         map[string]interface{} `json:"headers"`
	QueryParams     map[string]interface{} `json:"queryParams"`
	WithoutHeaders  map[string]interface{} `json:"withoutHeaders"`
	SourceLabels    map[string]string      `json:"sourceLabels"`
	SourceNamespace string                 `json:"sourceNamespace"`
	Port            uint32                 `json:"port"`
	Gateways        []string               `json:"gateways"`
	IgnoreURICase   bool                   `json:"ignoreUriCase"`
}

type rawVS struct {
	Hosts    []string `json:"hosts"`
	Gateways []string `json:"gateways"`
	HTTP     []struct {
		Name           string                 `json:"name"`
		Match          []rawHTTPMatch         `json:"match"`
		Route          []rawDestination       `json:"route"`
		Redirect       map[string]interface{} `json:"redirect"`
		DirectResponse map[string]interface{} `json:"directResponse"`
	} `json:"http"`
	TCP []struct {
		Match []struct {
			Port uint32 `json:"port"`
		} `json:"match"`
		Route []rawDestination `json:"route"`
	} `json:"tcp"`
	TLS []struct {
		Match []struct {
			SniHosts []string `json:"sniHosts"`
			Port     uint32   `json:"port"`
		} `json:"match"`
		Route []rawDestination `json:"route"`
	} `json:"tls"`
}

type rawGW struct {
	Selector map[string]string `json:"selector"`
	Servers  []struct {
		Port struct {
			Number   uint32 `json:"number"`
			Name     string `json:"name"`
			Protocol string `json:"protocol"`
		} `json:"port"`
		Hosts []string `json:"hosts"`
		TLS   *struct {
			Mode              string `json:"mode"`
			CredentialName    string `json:"credentialName"`
			ServerCertificate string `json:"serverCertificate"`
		} `json:"tls"`
	} `json:"servers"`
}

// specOf decodes .spec of an unstructured object into out (lenient: a
// malformed spec yields the zero value).
func specOf(u *unstructured.Unstructured, out interface{}) {
	spec, ok := u.Object["spec"]
	if !ok {
		return
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, out)
}

// ParseVirtualService converts an unstructured VirtualService.
func ParseVirtualService(u *unstructured.Unstructured) VirtualService {
	var raw rawVS
	specOf(u, &raw)
	vs := VirtualService{
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		Hosts:     raw.Hosts,
		Gateways:  raw.Gateways,
	}
	for _, r := range raw.HTTP {
		match := "/"
		if len(r.Match) > 0 {
			parts := make([]string, 0, len(r.Match))
			for _, m := range r.Match {
				parts = append(parts, uriSummary(m.URI))
			}
			match = strings.Join(parts, ",")
		}
		vs.Routes = append(vs.Routes, routes("http", match, r.Route)...)
		rule := HTTPRule{
			Name:           r.Name,
			Route:          destinations(r.Route),
			Redirect:       r.Redirect != nil,
			DirectResponse: r.DirectResponse != nil,
		}
		for _, m := range r.Match {
			rule.Match = append(rule.Match, parseHTTPMatch(m))
		}
		vs.HTTP = append(vs.HTTP, rule)
	}
	for _, r := range raw.TCP {
		match := "*"
		if len(r.Match) > 0 && r.Match[0].Port != 0 {
			match = ":" + strconv.Itoa(int(r.Match[0].Port))
		}
		vs.Routes = append(vs.Routes, routes("tcp", match, r.Route)...)
	}
	for _, r := range raw.TLS {
		match := "*"
		if len(r.Match) > 0 && len(r.Match[0].SniHosts) > 0 {
			match = "sni " + strings.Join(r.Match[0].SniHosts, ",")
		}
		vs.Routes = append(vs.Routes, routes("tls", match, r.Route)...)
		if len(r.Match) == 0 {
			vs.TLS = append(vs.TLS, TLSRule{Route: destinations(r.Route)})
		}
		for _, m := range r.Match {
			vs.TLS = append(vs.TLS, TLSRule{SNIHosts: m.SniHosts, Port: m.Port, Route: destinations(r.Route)})
		}
	}
	return vs
}

func routes(proto, match string, dests []rawDestination) []Route {
	out := make([]Route, 0, len(dests))
	for _, d := range dests {
		out = append(out, Route{
			Protocol: proto,
			Match:    match,
			Host:     d.Destination.Host,
			Port:     d.Destination.Port.Number,
			Subset:   d.Destination.Subset,
			Weight:   d.Weight,
		})
	}
	return out
}

func destinations(dests []rawDestination) []Destination {
	out := make([]Destination, 0, len(dests))
	for _, d := range dests {
		out = append(out, Destination{
			Host:   d.Destination.Host,
			Port:   d.Destination.Port.Number,
			Subset: d.Destination.Subset,
			Weight: d.Weight,
		})
	}
	return out
}

// stringMatch converts an Istio StringMatch map (exact|prefix|regex).
func stringMatch(m map[string]string) StringMatch {
	for _, k := range []string{"exact", "prefix", "regex"} {
		if v, ok := m[k]; ok {
			return StringMatch{Kind: k, Value: v}
		}
	}
	return StringMatch{}
}

func parseHTTPMatch(m rawHTTPMatch) HTTPMatch {
	out := HTTPMatch{
		URI:           stringMatch(m.URI),
		Authority:     stringMatch(m.Authority),
		Scheme:        stringMatch(m.Scheme),
		Port:          m.Port,
		Gateways:      m.Gateways,
		IgnoreURICase: m.IgnoreURICase,
	}
	if len(m.Method) > 0 {
		out.Conditional = append(out.Conditional, "method")
	}
	if len(m.Headers) > 0 {
		out.Conditional = append(out.Conditional, "headers")
	}
	if len(m.QueryParams) > 0 {
		out.Conditional = append(out.Conditional, "queryParams")
	}
	if len(m.WithoutHeaders) > 0 {
		out.Conditional = append(out.Conditional, "withoutHeaders")
	}
	if len(m.SourceLabels) > 0 {
		out.Conditional = append(out.Conditional, "sourceLabels")
	}
	if m.SourceNamespace != "" {
		out.Conditional = append(out.Conditional, "sourceNamespace")
	}
	sort.Strings(out.Conditional)
	return out
}

// uriSummary renders an Istio StringMatch: prefix "/api" → "/api",
// exact → "=/x", regex → "~re"; empty → "/" (matches everything).
func uriSummary(m map[string]string) string {
	switch {
	case m["prefix"] != "":
		return m["prefix"]
	case m["exact"] != "":
		return "=" + m["exact"]
	case m["regex"] != "":
		return "~" + m["regex"]
	}
	return "/"
}

// ParseGateway converts an unstructured Gateway.
func ParseGateway(u *unstructured.Unstructured) Gateway {
	var raw rawGW
	specOf(u, &raw)
	gw := Gateway{Namespace: u.GetNamespace(), Name: u.GetName(), Selector: raw.Selector}
	for _, s := range raw.Servers {
		srv := Server{
			Port:     s.Port.Number,
			PortName: s.Port.Name,
			Protocol: s.Port.Protocol,
			Hosts:    s.Hosts,
		}
		if s.TLS != nil {
			srv.TLSMode = s.TLS.Mode
			srv.HasCredential = s.TLS.CredentialName != "" || s.TLS.ServerCertificate != ""
		}
		gw.Servers = append(gw.Servers, srv)
	}
	return gw
}

// sortSnapshot orders everything by namespace/name so the mapper output is
// deterministic (D-034 fingerprinting).
func sortSnapshot(s *Snapshot) {
	sort.SliceStable(s.VirtualServices, func(i, j int) bool {
		return s.VirtualServices[i].Key() < s.VirtualServices[j].Key()
	})
	sort.SliceStable(s.Gateways, func(i, j int) bool {
		return s.Gateways[i].Key() < s.Gateways[j].Key()
	})
}
