package istio

import "strings"

// HostForm classifies a VirtualService destination host.
type HostForm int

const (
	// HostShort is a bare name ("reviews"); Istio expands it relative to
	// the VirtualService namespace.
	HostShort HostForm = iota
	// HostFQDN is "name.ns.svc.<cluster domain>".
	HostFQDN
	// HostPartial is "name.ns" or "name.ns.svc": it points at an in-cluster
	// Service but Istio does NOT expand it (only dot-less short names are
	// expanded), so it is taken literally and normally matches nothing.
	HostPartial
	// HostExternal is anything else (external DNS name, wildcard, IP...).
	HostExternal
)

// ResolveHost maps a destination host to the Service it names.
// vsNamespace is the VirtualService namespace; nsExists reports whether a
// namespace exists (used to tell "name.ns" from "example.com").
// For HostExternal ns/name are empty.
func ResolveHost(host, vsNamespace string, nsExists func(string) bool) (ns, name string, form HostForm) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || strings.Contains(h, "*") {
		return "", "", HostExternal
	}
	parts := strings.Split(h, ".")
	switch {
	case len(parts) == 1:
		return vsNamespace, parts[0], HostShort
	case len(parts) >= 4 && parts[2] == "svc":
		return parts[1], parts[0], HostFQDN
	case len(parts) == 3 && parts[2] == "svc":
		return parts[1], parts[0], HostPartial
	case len(parts) == 2 && nsExists != nil && nsExists(parts[1]):
		return parts[1], parts[0], HostPartial
	}
	return "", "", HostExternal
}

// ResolveGatewayRef resolves a spec.gateways entry: "ns/name" or a bare
// name (= the VirtualService namespace). The reserved "mesh" entry (sidecar
// routing, not a Gateway object) returns ok=false.
func ResolveGatewayRef(ref, vsNamespace string) (ns, name string, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "mesh" {
		return "", "", false
	}
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		ns, name = ref[:i], ref[i+1:]
		if ns == "" || name == "" {
			return "", "", false
		}
		return ns, name, true
	}
	return vsNamespace, ref, true
}

// HostExposed reports whether a VirtualService host (in namespace vsNs) is
// matched by a Gateway server host entry (Gateway in namespace gwNs).
// Server hosts are "[namespace/]dnsName": namespace "*" (any), "." (the
// Gateway's namespace) or a concrete namespace restricts which
// VirtualServices may bind; no namespace part means any. dnsName may be "*"
// or a "*.suffix" wildcard. A wildcard VirtualService host also binds to a
// server host it covers (Istio uses the intersection).
func HostExposed(vsHost, vsNs, serverHost, gwNs string) bool {
	nsPart, dns := "*", serverHost
	if i := strings.IndexByte(serverHost, '/'); i >= 0 {
		nsPart, dns = serverHost[:i], serverHost[i+1:]
	}
	switch nsPart {
	case "*":
	case ".":
		if vsNs != gwNs {
			return false
		}
	default:
		if vsNs != nsPart {
			return false
		}
	}
	return hostsIntersect(strings.ToLower(vsHost), strings.ToLower(dns))
}

// hostsIntersect reports whether two DNS names (each possibly "*" or a
// "*.suffix" wildcard) can match a common host.
func hostsIntersect(a, b string) bool {
	if a == "*" || b == "*" || a == b {
		return true
	}
	if strings.HasPrefix(b, "*.") && strings.HasSuffix(a, b[1:]) {
		return true
	}
	if strings.HasPrefix(a, "*.") && strings.HasSuffix(b, a[1:]) {
		return true
	}
	return false
}

// GatewayExposes reports whether any server of gw exposes vsHost.
func GatewayExposes(gw *Gateway, vsHost, vsNs string) bool {
	for _, s := range gw.Servers {
		for _, h := range s.Hosts {
			if HostExposed(vsHost, vsNs, h, gw.Namespace) {
				return true
			}
		}
	}
	return false
}
