package trace

import (
	"context"
	"sort"
	"strings"
)

// HostEntry is one host some entry point claims (UI autocomplete).
type HostEntry struct {
	Host    string   `json:"host"`
	Sources []string `json:"sources"`
}

// Hosts lists every host claimed by Istio Gateway servers, VirtualServices
// bound to a gateway and Ingress rules, sorted. Empty and wildcard hosts
// ("*", "*.example.com") are left out.
func Hosts(ctx context.Context, src Source) ([]HostEntry, error) {
	by := map[string]map[string]bool{}
	add := func(host, source string) {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		// Wildcards ("*", "*.x") are patterns, not traceable hosts: a
		// trace for one would be rejected (400), so they are left out.
		if h == "" || strings.HasPrefix(h, "*") {
			return
		}
		if by[h] == nil {
			by[h] = map[string]bool{}
		}
		by[h][source] = true
	}
	if snap := src.IstioSnapshot(); snap != nil {
		for i := range snap.Gateways {
			gw := &snap.Gateways[i]
			for _, s := range gw.Servers {
				for _, h := range s.Hosts {
					_, dns := splitServerHost(h)
					add(dns, "istio gateway "+gw.Key())
				}
			}
		}
		for i := range snap.VirtualServices {
			vs := &snap.VirtualServices[i]
			if len(boundGateways(vs)) == 0 {
				continue
			}
			for _, h := range vs.Hosts {
				add(h, "istio virtualservice "+vs.Key())
			}
		}
	}
	ings, err := src.Ingresses(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ings {
		for _, r := range ings[i].Spec.Rules {
			add(r.Host, "ingress "+ings[i].Namespace+"/"+ings[i].Name)
		}
	}
	out := make([]HostEntry, 0, len(by))
	for h, srcs := range by {
		e := HostEntry{Host: h}
		for s := range srcs {
			e.Sources = append(e.Sources, s)
		}
		sort.Strings(e.Sources)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}
