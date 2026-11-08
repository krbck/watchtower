package trace

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// --- Istio host semantics -------------------------------------------------

// istioHostMatch matches a concrete request host against an Istio host
// pattern: exact (case-insensitive), "*" (any host, weakest) or "*.suffix"
// which covers ANY number of labels in front of the suffix (unlike Ingress)
// but not the apex. score orders specificity: exact beats every wildcard,
// a longer wildcard suffix beats a shorter one, "*" is 0.
func istioHostMatch(pattern, host string) (bool, int) {
	p := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
	switch {
	case p == "":
		return false, 0
	case p == host:
		return true, 100000 + len(p)
	case p == "*":
		return true, 0
	case strings.HasPrefix(p, "*.") && strings.HasSuffix(host, p[1:]) && len(host) > len(p)-1:
		return true, len(p)
	}
	return false, 0
}

// splitServerHost splits a Gateway server host "[namespace/]dnsName"; the
// namespace part defaults to "*" (any namespace may bind).
func splitServerHost(h string) (nsPart, dns string) {
	if i := strings.IndexByte(h, '/'); i >= 0 {
		return h[:i], h[i+1:]
	}
	return "*", h
}

// nsAllowed applies the server host namespace part: "*" any, "." the
// Gateway's own namespace, otherwise exactly that namespace.
func nsAllowed(nsPart, vsNs, gwNs string) bool {
	switch nsPart {
	case "*":
		return true
	case ".":
		return vsNs == gwNs
	}
	return vsNs == nsPart
}

// boundGateways resolves a VirtualService's spec.gateways to "ns/name"
// keys ("mesh" excluded). An empty list means mesh-only.
func boundGateways(vs *istio.VirtualService) []string {
	var out []string
	for _, ref := range vs.Gateways {
		if ns, name, ok := istio.ResolveGatewayRef(ref, vs.Namespace); ok {
			out = append(out, ns+"/"+name)
		}
	}
	return out
}

func isBoundTo(vs *istio.VirtualService, gwKey string) bool {
	for _, k := range boundGateways(vs) {
		if k == gwKey {
			return true
		}
	}
	return false
}

// vsHostScore returns the best match score of a VS's hosts for host.
func vsHostScore(vs *istio.VirtualService, host string) (bool, int, string) {
	best, bestHost, ok := -1, "", false
	for _, h := range vs.Hosts {
		if m, sc := istioHostMatch(h, host); m && sc > best {
			best, bestHost, ok = sc, h, true
		}
	}
	return ok, best, bestHost
}

// --- claims -----------------------------------------------------------------

func istioClaims(w *world, req Request) []claim {
	snap := w.istio
	var out []claim
	known := map[string]bool{}
	for i := range snap.Gateways {
		gw := &snap.Gateways[i]
		known[gw.Key()] = true
		onGW, strong := false, false
		for _, s := range gw.Servers {
			for _, h := range s.Hosts {
				_, dns := splitServerHost(h)
				if m, sc := istioHostMatch(dns, req.Host); m {
					onGW = true
					if sc > 0 {
						strong = true
					}
				}
			}
		}
		var bound []*istio.VirtualService
		for j := range snap.VirtualServices {
			vs := &snap.VirtualServices[j]
			if !isBoundTo(vs, gw.Key()) {
				continue
			}
			if m, sc, _ := vsHostScore(vs, req.Host); m {
				bound = append(bound, vs)
				if sc > 0 {
					strong = true
				}
			}
		}
		if !onGW && len(bound) == 0 {
			continue
		}
		out = append(out, claim{
			provider: ProviderIstio,
			entry:    graphid.IstioGateway(gw.Namespace, gw.Name),
			weak:     !strong,
			run: func(ctx context.Context) *branchBuilder {
				return istioBranch(ctx, w, req, gw, onGW, bound)
			},
		})
	}
	// VirtualServices bound to a Gateway that does not exist.
	missing := map[string][]*istio.VirtualService{}
	for j := range snap.VirtualServices {
		vs := &snap.VirtualServices[j]
		m, _, _ := vsHostScore(vs, req.Host)
		if !m {
			continue
		}
		for _, k := range boundGateways(vs) {
			if !known[k] {
				missing[k] = append(missing[k], vs)
			}
		}
	}
	for k, vss := range missing {
		k, vss := k, vss
		ns, name, _ := strings.Cut(k, "/")
		out = append(out, claim{
			provider: ProviderIstio,
			entry:    graphid.IstioGateway(ns, name),
			run: func(context.Context) *branchBuilder {
				b := newBranch(ProviderIstio, graphid.IstioGateway(ns, name))
				b.add(scopeStep())
				b.add(hostClaimStep(b, req, fmt.Sprintf("VirtualService %s claims %s via Gateway %s", vsKeys(vss), req.Host, k), ""))
				ref := graphid.IstioGateway(ns, name)
				b.define(checker.TopologyNode{ID: ref, Kind: "gateway", Label: name, Namespace: ns, Detail: "not found"})
				b.add(Step{
					Name:   "Istio Gateway " + k,
					Hop:    HopClass,
					Ref:    ref,
					Status: StatusFail,
					Reason: ReasonRouteNotAttached,
					Detail: fmt.Sprintf("Gateway %s referenced in spec.gateways of %s does not exist, so the route is not attached to any gateway.", k, vsKeys(vss)),
					Hint:   "Create the Gateway or fix the reference (\"<namespace>/<name>\"; a bare name means the VirtualService namespace).",
				})
				return b
			},
		})
	}
	return out
}

func vsKeys(vss []*istio.VirtualService) string {
	keys := make([]string, 0, len(vss))
	for _, v := range vss {
		keys = append(keys, v.Key())
	}
	return strings.Join(keys, ", ")
}

// hostClaimStep is the first graph hop: the queried host.
func hostClaimStep(b *branchBuilder, req Request, name, detail string) Step {
	ref := graphid.TracePrefix + "host:" + req.Host
	b.define(checker.TopologyNode{
		ID: ref, Kind: "host", Label: req.Host,
		Meta: map[string]string{"request": fmt.Sprintf("%s://%s:%d%s", req.Scheme, req.Host, req.Port, req.Path)},
	})
	return Step{Name: name, Hop: HopClaim, Ref: ref, Status: StatusOK, Detail: detail}
}

// --- branch -------------------------------------------------------------------

func istioBranch(ctx context.Context, w *world, req Request, gw *istio.Gateway, onGW bool, bound []*istio.VirtualService) *branchBuilder {
	gwRef := graphid.IstioGateway(gw.Namespace, gw.Name)
	b := newBranch(ProviderIstio, gwRef)
	b.add(scopeStep())
	claimDetail := ""
	if onGW {
		claimDetail = fmt.Sprintf("Gateway %s lists the host in spec.servers[].hosts.", gw.Key())
	}
	if len(bound) > 0 {
		if claimDetail != "" {
			claimDetail += " "
		}
		claimDetail += fmt.Sprintf("VirtualService %s (bound to the gateway) lists it in spec.hosts.", vsKeys(bound))
	}
	b.add(hostClaimStep(b, req, fmt.Sprintf("Host %s claimed by Istio Gateway %s", req.Host, gw.Key()), claimDetail))

	e := w.entryForSelector(gw.Selector)
	frontHalf(ctx, w, b, e, req, "gateway", ReasonGatewayPortNotExposed)

	// Class / controller: for Istio the Gateway selector binds the proxy.
	b.add(Step{
		Name:   "Istio Gateway " + gw.Key(),
		Hop:    HopClass,
		Ref:    gwRef,
		Status: StatusOK,
		Detail: fmt.Sprintf("Gateway selector %s binds the Istio proxy pods; servers: %s.", selectorString(gw.Selector), serversSummary(gw)),
		Items:  gatewayHostList(gw),
	})

	srv, ok := istioListener(b, gw, gwRef, req, bound)
	if !ok {
		return b
	}
	istioRoute(ctx, w, b, gw, srv, req, bound)
	return b
}

func serversSummary(gw *istio.Gateway) string {
	parts := make([]string, 0, len(gw.Servers))
	for _, s := range gw.Servers {
		parts = append(parts, fmt.Sprintf("%s:%d (%s)", strings.ToLower(s.Protocol), s.Port, hostCount(len(s.Hosts))))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}

// sortedHosts returns the distinct hosts in sorted order, for Step.Items.
func sortedHosts(hosts []string) []string {
	out := dedupe(append([]string{}, hosts...))
	sort.Strings(out)
	return out
}

// gatewayHostList is every server host of the gateway (Step.Items).
func gatewayHostList(gw *istio.Gateway) []string {
	var all []string
	for _, s := range gw.Servers {
		all = append(all, s.Hosts...)
	}
	return sortedHosts(all)
}

// istioListener picks the Gateway server for port + host and checks the
// protocol and TLS credential presence.
func istioListener(b *branchBuilder, gw *istio.Gateway, ref string, req Request, bound []*istio.VirtualService) (*istio.Server, bool) {
	var best *istio.Server
	bestScore := -1
	var portHosts, otherPorts []string
	for i := range gw.Servers {
		s := &gw.Servers[i]
		hostMatch := -1
		for _, h := range s.Hosts {
			_, dns := splitServerHost(h)
			if m, sc := istioHostMatch(dns, req.Host); m && sc > hostMatch {
				hostMatch = sc
			}
		}
		if int32(s.Port) != req.Port {
			if hostMatch >= 0 {
				otherPorts = append(otherPorts, fmt.Sprintf("%d", s.Port))
			}
			continue
		}
		portHosts = append(portHosts, s.Hosts...)
		if hostMatch > bestScore {
			best, bestScore = s, hostMatch
		}
	}
	name := fmt.Sprintf("Gateway server :%d for %s", req.Port, req.Host)
	if best == nil {
		st := Step{Name: name, Hop: HopListener, Ref: ref, Status: StatusFail}
		switch {
		case len(otherPorts) > 0:
			st.Reason = ReasonListenerPortMismatch
			st.Detail = fmt.Sprintf("Istio gateway %s serves this host only on port(s) %s, not %d.", gw.Key(), strings.Join(dedupe(otherPorts), ", "), req.Port)
			st.Hint = fmt.Sprintf("Query the served port, or add a server on port %d for the host.", req.Port)
		default:
			st.Reason = ReasonHostNotOnGateway
			st.Detail = fmt.Sprintf("Istio gateway %s does not route this host: no server lists %s", gw.Key(), req.Host)
			if len(portHosts) > 0 {
				st.Items = sortedHosts(portHosts)
				st.Detail += fmt.Sprintf(" (port %d serves %s)", req.Port, hostCount(len(st.Items)))
			}
			st.Detail += "."
			st.Hint = fmt.Sprintf("Add %s (or a matching wildcard such as *.%s) to spec.servers[].hosts of Gateway %s.", req.Host, parentDomain(req.Host), gw.Key())
			if len(bound) > 0 {
				st.Hint += fmt.Sprintf(" VirtualService %s is bound to this gateway, but Istio only applies VirtualService hosts that a gateway server exposes.", vsKeys(bound))
			}
		}
		b.add(st)
		if st.Reason == ReasonHostNotOnGateway {
			// Show which VirtualService claims the unexposed host: its
			// node joins the path (not applied, so the step is skipped).
			for _, vs := range bound {
				b.add(Step{
					Name:   "VirtualService " + vs.Key() + " (not applied)",
					Hop:    HopRoute,
					Ref:    graphid.VirtualService(vs.Namespace, vs.Name),
					Status: StatusSkipped,
					Detail: fmt.Sprintf("Lists %s in spec.hosts and is bound to gateway %s, but no server of that gateway exposes the host, so Istio does not apply it there.", req.Host, gw.Key()),
				})
			}
		}
		return nil, false
	}
	st := Step{
		Name:   name,
		Hop:    HopListener,
		Ref:    ref,
		Status: StatusOK,
		Detail: fmt.Sprintf("Server %s:%d (%s) matches.", strings.ToLower(best.Protocol), best.Port, hostCount(len(best.Hosts))),
		Items:  sortedHosts(best.Hosts),
	}
	proto := strings.ToUpper(best.Protocol)
	switch {
	case req.Scheme == "https" && proto != "HTTPS" && proto != "TLS":
		st.Status, st.Reason = StatusWarn, ReasonListenerPortMismatch
		st.Detail += fmt.Sprintf(" But the server protocol is %s while the request is https.", best.Protocol)
		st.Hint = "Use an HTTPS (or TLS) server for https traffic, or query with scheme=http."
	case req.Scheme == "http" && (proto == "HTTPS" || proto == "TLS"):
		st.Status, st.Reason = StatusWarn, ReasonListenerPortMismatch
		st.Detail += fmt.Sprintf(" But the server protocol is %s while the request is plain http.", best.Protocol)
		st.Hint = "Query with scheme=https, or add an HTTP server for the host."
	case tlsPassthrough(best):
		st.Detail += fmt.Sprintf(" TLS mode %s: the gateway does not terminate TLS and routes by SNI (tls[]); no certificate is needed on the gateway.", strings.ToUpper(best.TLSMode))
	case terminatesTLS(best):
		if best.HasCredential {
			st.Detail += " TLS credentialName is set (the Secret itself is not read)."
		} else {
			st.Status = StatusWarn
			st.Detail += " TLS credentialName is not set."
			st.Hint = "Set tls.credentialName to the certificate Secret in the gateway namespace (Watchtower never reads Secrets)."
		}
	}
	b.add(st)
	return best, true
}

// tlsPassthrough reports a server that forwards TLS untouched (SNI routing).
func tlsPassthrough(s *istio.Server) bool {
	m := strings.ToUpper(s.TLSMode)
	return m == "PASSTHROUGH" || m == "AUTO_PASSTHROUGH"
}

// terminatesTLS reports a server that terminates TLS at the gateway (HTTPS
// or TLS protocol with SIMPLE/MUTUAL/ISTIO_MUTUAL or unset mode): only then
// a certificate (credentialName) is required.
func terminatesTLS(s *istio.Server) bool {
	p := strings.ToUpper(s.Protocol)
	return (p == "HTTPS" || p == "TLS") && !tlsPassthrough(s)
}

func tlsModeOrDefault(s *istio.Server) string {
	if s.TLSMode == "" {
		return "SIMPLE (default)"
	}
	return strings.ToUpper(s.TLSMode)
}

func parentDomain(host string) string {
	if i := strings.IndexByte(host, '.'); i >= 0 && strings.Contains(host[i+1:], ".") {
		return host[i+1:]
	}
	return host
}

// istioRoute selects the VirtualService (exact host beats wildcard; VS
// namespace must be allowed by the server host), applies http[] first-match
// semantics and follows the destinations.
func istioRoute(ctx context.Context, w *world, b *branchBuilder, gw *istio.Gateway, srv *istio.Server, req Request, bound []*istio.VirtualService) {
	gwRef := graphid.IstioGateway(gw.Namespace, gw.Name)
	type cand struct {
		vs    *istio.VirtualService
		score int
		host  string
	}
	var allowed []cand
	var denied []string
	for _, vs := range bound {
		_, sc, h := vsHostScore(vs, req.Host)
		ok := false
		for _, sh := range srv.Hosts {
			nsPart, dns := splitServerHost(sh)
			if m, _ := istioHostMatch(dns, req.Host); m && nsAllowed(nsPart, vs.Namespace, gw.Namespace) {
				ok = true
			}
		}
		if ok {
			allowed = append(allowed, cand{vs, sc, h})
		} else {
			denied = append(denied, vs.Key())
		}
	}
	if len(allowed) == 0 {
		st := Step{Name: "Route for " + req.Host, Hop: HopRoute, Ref: gwRef, Status: StatusFail}
		if len(denied) > 0 {
			st.Reason = ReasonVSNamespaceNotAllowed
			st.Detail = fmt.Sprintf("VirtualService %s matches the host, but the server (%s) does not allow its namespace to bind.", strings.Join(denied, ", "), hostCount(len(srv.Hosts)))
			st.Items = sortedHosts(srv.Hosts)
			st.Hint = "Use a server host of the form \"<vs-namespace>/host\" or \"*/host\", or move the VirtualService."
		} else {
			st.Reason = ReasonNoRouteMatch
			st.Detail = fmt.Sprintf("No VirtualService bound to gateway %s serves host %s, so the gateway has no route for it (Envoy answers 404).", gw.Key(), req.Host)
			st.Hint = fmt.Sprintf("Create a VirtualService with hosts: [%s] and gateways: [%s], or remove the host from the Gateway.", req.Host, gw.Key())
		}
		b.add(st)
		return
	}
	sort.SliceStable(allowed, func(i, j int) bool {
		if allowed[i].score != allowed[j].score {
			return allowed[i].score > allowed[j].score
		}
		return allowed[i].vs.Key() < allowed[j].vs.Key()
	})
	chosen := allowed[0]
	vsRef := graphid.VirtualService(chosen.vs.Namespace, chosen.vs.Name)
	if len(allowed) > 1 && allowed[1].score == chosen.score {
		var same []string
		for _, c := range allowed {
			if c.score == chosen.score {
				same = append(same, c.vs.Key())
			}
		}
		b.add(Step{
			Name:   "VirtualService host conflict",
			Hop:    HopRoute,
			Ref:    vsRef,
			Status: StatusWarn,
			Reason: ReasonHostConflict,
			Detail: fmt.Sprintf("VirtualServices %s all bind %q to gateway %s; Istio merges them in an order you do not control. Following %s.", strings.Join(same, ", "), chosen.host, gw.Key(), chosen.vs.Key()),
			Hint:   "Keep one VirtualService per host and gateway (or use delegate VirtualServices).",
		})
	}

	// PASSTHROUGH / AUTO_PASSTHROUGH never terminate TLS: routing is by
	// SNI via tls[] whatever the server protocol (HTTPS or TLS). Only a
	// terminating HTTPS server (SIMPLE/MUTUAL/ISTIO_MUTUAL) uses http[].
	if tlsPassthrough(srv) {
		istioTLSRoute(ctx, w, b, chosen.vs, vsRef, req)
		return
	}
	if strings.EqualFold(srv.Protocol, "TLS") {
		b.add(Step{
			Name:   fmt.Sprintf("Route: VirtualService %s (TLS-terminated TCP)", chosen.vs.Key()),
			Hop:    HopRoute,
			Ref:    vsRef,
			Status: StatusUnknown,
			Detail: fmt.Sprintf("Server protocol TLS with mode %s terminates TLS and routes the stream via tcp[] rules, which this trace does not evaluate yet.", tlsModeOrDefault(srv)),
		})
		return
	}

	rule, conditional, idx := selectHTTPRule(chosen.vs, gw, srv, req)
	name := fmt.Sprintf("Route: VirtualService %s", chosen.vs.Key())
	if len(conditional) > 0 {
		b.add(Step{
			Name:   name + " — conditional rules first",
			Hop:    HopRoute,
			Ref:    vsRef,
			Status: StatusWarn,
			Reason: ReasonConditionalMatch,
			Detail: "Rules " + strings.Join(conditional, "; ") + " match the path but also require request properties a host/path trace cannot know; matching requests go there instead.",
			Hint:   "Verify with real requests (headers / method / query) which rule applies.",
		})
	}
	if rule == nil {
		// D-040: the host is routed by a specific VS host and some http[]
		// entries apply to this gateway/port — only the path misses. That
		// is a warning (config is fine, the gateway answers 404 there),
		// not a broken route. A "*" VS host is a weak claim (D-038a) and
		// a VS without applicable entries has no route at all: both stay
		// noRouteMatch.
		if served, ok := servedIstioPaths(chosen.vs, gw, srv, req); ok && chosen.score > 0 {
			b.add(pathNotRoutedStep(name+" — path "+req.Path+" not routed", vsRef, `a catch-all http[] rule with uri prefix "/"`, served))
			return
		}
		b.add(Step{
			Name:   name,
			Hop:    HopRoute,
			Ref:    vsRef,
			Status: StatusFail,
			Reason: ReasonNoRouteMatch,
			Detail: fmt.Sprintf("Host %q (from %s) matches, but no http[] rule matches path %s on port %d. Rules: %s.", chosen.host, chosen.vs.Key(), req.Path, req.Port, describeRules(chosen.vs)),
			Hint:   "Add a catch-all rule (no match, or uri prefix \"/\") or a rule matching this path; unmatched requests get a 404 from the gateway.",
		})
		return
	}
	st := Step{
		Name:   name,
		Hop:    HopRoute,
		Ref:    vsRef,
		Status: StatusOK,
		Detail: fmt.Sprintf("Host %q matches; http[%d] (%s) is the first rule matching %s.", chosen.host, idx, ruleSummary(rule), req.Path),
	}
	if rule.Redirect || rule.DirectResponse {
		kind := "redirect"
		if rule.DirectResponse {
			kind = "direct response"
		}
		st.Detail += " The rule answers with a " + kind + " — no backend is involved."
		b.add(st)
		return
	}
	if len(rule.Route) == 0 {
		st.Status, st.Reason = StatusFail, ReasonNoRouteMatch
		st.Detail += " The rule has no destination."
		b.add(st)
		return
	}
	b.add(st)
	followDestinations(ctx, w, b, chosen.vs, vsRef, rule.Route)
}

// selectHTTPRule applies Istio first-match semantics: rules in order; a
// rule matches when it has no match block or any match entry matches
// (conditions ANDed). Entries that also need headers/method/query are
// "conditional": listed, but the first unconditional match is followed;
// with none, the first conditional one is followed (as a candidate).
func selectHTTPRule(vs *istio.VirtualService, gw *istio.Gateway, srv *istio.Server, req Request) (*istio.HTTPRule, []string, int) {
	var conditional []string
	firstCond, firstIdx := (*istio.HTTPRule)(nil), -1
	for i := range vs.HTTP {
		r := &vs.HTTP[i]
		matched, cond := httpRuleMatches(r, vs, gw, srv, req)
		if !matched {
			continue
		}
		if !cond {
			return r, conditional, i
		}
		conditional = append(conditional, fmt.Sprintf("http[%d] (%s)", i, ruleSummary(r)))
		if firstCond == nil {
			firstCond, firstIdx = r, i
		}
	}
	if firstCond != nil {
		return firstCond, conditional, firstIdx
	}
	return nil, conditional, -1
}

func httpRuleMatches(r *istio.HTTPRule, vs *istio.VirtualService, gw *istio.Gateway, srv *istio.Server, req Request) (matched, conditional bool) {
	if len(r.Match) == 0 {
		return true, false
	}
	condOnly := false
	for _, m := range r.Match {
		if !httpMatchApplies(m, vs, gw, srv, req) {
			continue
		}
		if len(m.Conditional) == 0 {
			return true, false
		}
		condOnly = true
	}
	return condOnly, condOnly
}

func httpMatchApplies(m istio.HTTPMatch, vs *istio.VirtualService, gw *istio.Gateway, srv *istio.Server, req Request) bool {
	if len(m.Gateways) > 0 {
		ok := false
		for _, ref := range m.Gateways {
			if ns, name, valid := istio.ResolveGatewayRef(ref, vs.Namespace); valid && ns == gw.Namespace && name == gw.Name {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if m.Port != 0 && m.Port != srv.Port {
		return false
	}
	return stringMatches(m.URI, req.Path, m.IgnoreURICase) &&
		stringMatches(m.Authority, req.Host, true) &&
		stringMatches(m.Scheme, req.Scheme, true)
}

// stringMatches evaluates an Istio StringMatch (empty = any). Regex is
// RE2, anchored (Envoy safe_regex full match); an invalid regex matches
// nothing.
func stringMatches(m istio.StringMatch, v string, ignoreCase bool) bool {
	val, want := v, m.Value
	if ignoreCase {
		val, want = strings.ToLower(v), strings.ToLower(m.Value)
	}
	switch m.Kind {
	case "":
		return true
	case "exact":
		return val == want
	case "prefix":
		return strings.HasPrefix(val, want)
	case "regex":
		expr := "^(?:" + m.Value + ")$"
		if ignoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		return err == nil && re.MatchString(v)
	}
	return false
}

// servedIstioPaths lists, in declaration order, the uri matches of the
// http[] entries that apply to this request in everything but the path
// (gateway, port, authority, scheme). ok is false when no entry applies at
// all: then the VirtualService has no route for the request, not merely no
// rule for this path.
func servedIstioPaths(vs *istio.VirtualService, gw *istio.Gateway, srv *istio.Server, req Request) ([]string, bool) {
	var out []string
	ok := false
	for i := range vs.HTTP {
		for _, m := range vs.HTTP[i].Match {
			anyPath := m
			anyPath.URI = istio.StringMatch{}
			if !httpMatchApplies(anyPath, vs, gw, srv, req) {
				continue
			}
			ok = true
			if m.URI.Kind == "" {
				continue
			}
			d := m.URI.Kind + " " + m.URI.Value
			if m.IgnoreURICase {
				d += " (case-insensitive)"
			}
			if len(m.Conditional) > 0 {
				d += " (+" + strings.Join(m.Conditional, ", ") + ")"
			}
			out = append(out, d)
		}
	}
	return dedupe(out), ok
}

// maxInlinePaths bounds how many served paths the pathNotRouted hint
// names; longer lists move to Step.Items (D-039 pattern).
const maxInlinePaths = 4

// pathNotRoutedStep is the D-040 warning: the route configuration exists
// and is correct, but the requested path matches none of its rules.
func pathNotRoutedStep(name, ref, catchAll string, served []string) Step {
	st := Step{
		Name:   name,
		Hop:    HopRoute,
		Ref:    ref,
		Status: StatusWarn,
		Reason: ReasonPathNotRouted,
		Detail: "Route configuration is correct; no rule matches this path (the request would get a 404 from the gateway). The endpoint or path may be misconfigured, or the site serves nothing there.",
	}
	tail := " Request one of these paths, or add a rule for this path (e.g. " + catchAll + ") if it should be served."
	switch {
	case len(served) == 0:
		st.Hint = "The matching rules serve no plain path (only conditional rules)." + tail
	case len(served) <= maxInlinePaths:
		st.Hint = "The matching rules serve: " + strings.Join(served, ", ") + "." + tail
	default:
		st.Hint = fmt.Sprintf("The matching rules serve %d paths (see the list).", len(served)) + tail
		st.Items = served
	}
	return st
}

func ruleSummary(r *istio.HTTPRule) string {
	if len(r.Match) == 0 {
		return "no match: all requests"
	}
	parts := make([]string, 0, len(r.Match))
	for _, m := range r.Match {
		var c []string
		if m.URI.Kind != "" {
			c = append(c, "uri "+m.URI.Kind+" "+m.URI.Value)
		}
		if m.Authority.Kind != "" {
			c = append(c, "authority "+m.Authority.Kind+" "+m.Authority.Value)
		}
		if m.Port != 0 {
			c = append(c, fmt.Sprintf("port %d", m.Port))
		}
		if len(m.Gateways) > 0 {
			c = append(c, "gateways "+strings.Join(m.Gateways, ","))
		}
		c = append(c, m.Conditional...)
		if len(c) == 0 {
			c = append(c, "any")
		}
		parts = append(parts, strings.Join(c, " & "))
	}
	return strings.Join(parts, " | ")
}

func describeRules(vs *istio.VirtualService) string {
	if len(vs.HTTP) == 0 {
		return "none (no http[] section)"
	}
	parts := make([]string, 0, len(vs.HTTP))
	for i := range vs.HTTP {
		parts = append(parts, fmt.Sprintf("http[%d] %s", i, ruleSummary(&vs.HTTP[i])))
	}
	return strings.Join(parts, "; ")
}

// istioTLSRoute handles TLS passthrough servers: tls[] rules by SNI.
func istioTLSRoute(ctx context.Context, w *world, b *branchBuilder, vs *istio.VirtualService, vsRef string, req Request) {
	for i, r := range vs.TLS {
		if r.Port != 0 && int32(r.Port) != req.Port {
			continue
		}
		match := len(r.SNIHosts) == 0
		for _, h := range r.SNIHosts {
			if m, _ := istioHostMatch(h, req.Host); m {
				match = true
			}
		}
		if !match {
			continue
		}
		b.add(Step{Name: "Route: VirtualService " + vs.Key() + " (TLS passthrough)", Hop: HopRoute, Ref: vsRef, Status: StatusOK,
			Detail: fmt.Sprintf("tls[%d] matches SNI %s.", i, req.Host)})
		followDestinations(ctx, w, b, vs, vsRef, r.Route)
		return
	}
	b.add(Step{Name: "Route: VirtualService " + vs.Key() + " (TLS passthrough)", Hop: HopRoute, Ref: vsRef, Status: StatusFail,
		Reason: ReasonNoRouteMatch, Detail: "No tls[] rule matches SNI " + req.Host + ".",
		Hint: "Add a tls[] rule with match.sniHosts covering the host."})
}

// followDestinations walks every destination; weighted ones become
// sub-paths (one group per backend) forked at the VirtualService.
func followDestinations(ctx context.Context, w *world, b *branchBuilder, vs *istio.VirtualService, vsRef string, dests []istio.Destination) {
	for i, d := range dests {
		b.at(vsRef)
		target := d.Host
		if d.Port != 0 {
			target += fmt.Sprintf(":%d", d.Port)
		}
		group := weightLabel(i, len(dests), d.Weight, target)
		ns, name, form := istio.ResolveHost(d.Host, vs.Namespace, func(n string) bool { return w.namespaces[n] })
		switch form {
		case istio.HostExternal:
			b.add(Step{Name: "Destination " + d.Host, Hop: HopBackend, Status: StatusUnknown, Group: group,
				Detail: "The destination is not a Service in this cluster (external host / ServiceEntry); the trace stops here.",
				Hint:   "If this is not intended, use the Service short name or its FQDN name.namespace.svc.cluster.local."})
			continue
		case istio.HostPartial:
			ref := graphid.Service(ns, name)
			b.define(checker.TopologyNode{ID: ref, Kind: "service", Label: name, Namespace: ns})
			b.add(Step{Name: "Destination " + d.Host, Hop: HopBackend, Ref: ref, Status: StatusFail, Group: group,
				Reason: ReasonBackendServiceMissing,
				Detail: fmt.Sprintf("Host %q is partially qualified: Istio expands only dot-less short names, so it is taken literally and matches no Service.", d.Host),
				Hint:   fmt.Sprintf("Use %q (if the VirtualService is in %s) or the FQDN %q.", name, ns, name+"."+ns+".svc.cluster.local")})
			continue
		}
		port := int32(d.Port)
		if svc := w.svcByKey[ns+"/"+name]; svc != nil && port == 0 {
			if len(svc.Spec.Ports) == 1 {
				port = svc.Spec.Ports[0].Port
			} else {
				ref := graphid.Service(ns, name)
				b.add(Step{Name: "Destination port", Hop: HopBackend, Ref: ref, Status: StatusFail, Group: group,
					Reason: ReasonBackendPortMissing,
					Detail: fmt.Sprintf("destination.port is not set and Service %s/%s has %d ports, so Istio cannot pick one.", ns, name, len(svc.Spec.Ports)),
					Hint:   "Set destination.port.number."})
				continue
			}
		}
		if d.Subset != "" {
			b.add(Step{Name: "DestinationRule subset " + d.Subset, Hop: HopBackend, Ref: graphid.Service(ns, name), Status: StatusSkipped, Group: group,
				Reason: ReasonDestinationRuleSubset,
				Detail: "Subset routing is not validated yet (DestinationRule subsets come with a later slice); the whole Service is traced."})
		}
		backendSteps(ctx, w, b, ns, name, port, group)
	}
}
