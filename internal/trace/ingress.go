package trace

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/ingressutil"
)

// ingressGroup is every Ingress of one class that claims the host: a
// controller merges them, so they form one entry point.
type ingressGroup struct {
	key       string // class name, "" when none could be resolved
	res       ingressutil.ClassResolution
	ingresses []*networkingv1.Ingress
	strong    bool
}

func ingressClaims(w *world, req Request) []claim {
	groups := map[string]*ingressGroup{}
	var order []string
	for i := range w.ingresses {
		ing := &w.ingresses[i]
		best := ingressutil.NoMatch
		for _, r := range ing.Spec.Rules {
			if m := ingressutil.MatchHost(r.Host, req.Host); m > best {
				best = m
			}
		}
		if best == ingressutil.NoMatch && len(ing.Spec.Rules) == 0 && ing.Spec.DefaultBackend != nil {
			best = ingressutil.CatchAll
		}
		if best == ingressutil.NoMatch {
			continue
		}
		res := ingressutil.ResolveClass(ing, w.classes)
		k := res.Name
		if k == "" && len(res.Defaults) > 1 {
			k = "(ambiguous)"
		}
		g := groups[k]
		if g == nil {
			g = &ingressGroup{key: k, res: res}
			groups[k] = g
			order = append(order, k)
		}
		g.ingresses = append(g.ingresses, ing)
		if best >= ingressutil.Wildcard {
			g.strong = true
		}
	}
	sort.Strings(order)
	out := make([]claim, 0, len(order))
	for _, k := range order {
		g := groups[k]
		entryID := graphid.IngressClass(g.key)
		if g.key == "" || g.key == "(ambiguous)" {
			entryID = graphid.Ingress(g.ingresses[0].Namespace, g.ingresses[0].Name)
		}
		out = append(out, claim{
			provider: ProviderIngress,
			entry:    entryID,
			weak:     !g.strong,
			run:      func(ctx context.Context) *branchBuilder { return ingressBranch(ctx, w, req, g, entryID) },
		})
	}
	return out
}

func ingressBranch(ctx context.Context, w *world, req Request, g *ingressGroup, entryID string) *branchBuilder {
	b := newBranch(ProviderIngress, entryID)
	b.add(scopeStep())
	names := make([]string, 0, len(g.ingresses))
	for _, ing := range g.ingresses {
		names = append(names, ing.Namespace+"/"+ing.Name)
	}
	detail := "Ingress rules matching the host: " + strings.Join(names, ", ") + "."
	if !g.strong {
		detail += " Only a catch-all rule (no host) matches — it applies to every host this controller receives."
	}
	b.add(hostClaimStep(b, req, fmt.Sprintf("Host %s claimed by Ingress %s", req.Host, strings.Join(names, ", ")), detail))

	// Controller → workload resolution: IngressClass controller table, then
	// the Ingress status address (IP/hostname + port) against LB Services.
	ctl := resolveController(w, req, g)
	switch {
	case ctl.entry != nil:
		frontHalf(ctx, w, b, *ctl.entry, req, "ingress controller", ReasonListenerPortMismatch)
	default:
		// External address as published in the Ingress status.
		if addrs := ingressStatusAddrs(g); len(addrs) > 0 {
			b.add(Step{Name: "External address", Hop: HopAddress, Status: StatusUnknown,
				Detail: "The Ingress status publishes " + strings.Join(addrs, ", ") + ", but no controller Service could be matched to it (stale status from a removed controller?)."})
		} else {
			b.add(Step{Name: "External address", Hop: HopAddress, Status: StatusFail, Reason: ReasonNoExternalAddress,
				Detail: "The Ingress status has no load balancer address and no controller was resolved — no controller has admitted this Ingress.",
				Hint:   "Install / fix the ingress controller for this class; it publishes its address into status.loadBalancer."})
		}
		st := Step{Name: "Ingress controller", Hop: HopEntryService, Status: StatusFail, Reason: ReasonControllerUnknown, Detail: ctl.why,
			Hint: "Use an IngressClass whose spec.controller is a running controller (known: ingress-nginx, traefik, istio), or make sure the controller publishes its address in the Ingress status."}
		if len(ctl.candidates) > 1 {
			st.Status, st.Reason = StatusWarn, ReasonAmbiguous
			st.Detail = "Several LoadBalancer Services match the Ingress status address and port: " + strings.Join(ctl.candidates, ", ") + "."
			st.Hint = "k3s ServiceLB gives every LoadBalancer the node IPs; set an IngressClass with a known controller to disambiguate."
		}
		b.add(st)
		b.add(Step{Name: "Ingress controller workload", Hop: HopWorkload, Status: StatusUnknown, Detail: "Unknown — the controller could not be resolved."})
	}

	classStep(b, g, ctl)
	if req.Scheme == "https" {
		ingressTLSStep(b, req, g)
	}
	ingressRoute(ctx, w, b, req, g)
	return b
}

// ingressTLSStep reports whether an Ingress spec.tls entry covers the host
// (the Secret is only named, never read — D-038).
func ingressTLSStep(b *branchBuilder, req Request, g *ingressGroup) {
	for _, ing := range g.ingresses {
		for _, t := range ing.Spec.TLS {
			for _, h := range t.Hosts {
				if ingressutil.MatchHost(h, req.Host) >= ingressutil.Wildcard {
					st := Step{Name: "TLS for " + req.Host, Hop: HopListener, Ref: graphid.Ingress(ing.Namespace, ing.Name), Status: StatusOK,
						Detail: fmt.Sprintf("spec.tls of %s/%s covers the host", ing.Namespace, ing.Name)}
					if t.SecretName != "" {
						st.Detail += " (secret " + t.SecretName + " referenced; not read)."
					} else {
						st.Detail += " without secretName (controller default certificate)."
					}
					b.add(st)
					return
				}
			}
		}
	}
	ing := g.ingresses[0]
	b.add(Step{Name: "TLS for " + req.Host, Hop: HopListener, Ref: graphid.Ingress(ing.Namespace, ing.Name), Status: StatusWarn,
		Detail: "No spec.tls entry covers the host: the controller serves https with its default certificate (if at all).",
		Hint:   "Add a spec.tls entry with the host and its certificate Secret."})
}

type controllerResolution struct {
	entry      *entry
	via        string
	why        string
	candidates []string
}

// resolveController maps the group's class to a controller workload.
// Order: known controller table (IngressClass spec.controller → pod labels,
// all namespaces), then Ingress status IP/hostname + request port against
// LoadBalancer Services — always IP+port, because k3s ServiceLB gives every
// LB Service the same node IPs. No system-namespace filter (traefik and
// svclb run in kube-system).
func resolveController(w *world, req Request, g *ingressGroup) controllerResolution {
	var out controllerResolution
	if g.res.Found() {
		if k, ok := ingressutil.LookupController(g.res.Class.Spec.Controller); ok {
			e := w.entryForSelector(k.Selector)
			if len(e.pods) > 0 || len(e.services) > 0 {
				out.entry = &e
				out.via = fmt.Sprintf("known controller %s (pods %s)", k.Name, selectorString(k.Selector))
				return out
			}
			out.why = fmt.Sprintf("IngressClass %s names controller %s, but no pod matches %s in any namespace.", g.res.Name, k.Controller, selectorString(k.Selector))
		} else {
			out.why = fmt.Sprintf("Controller %q of IngressClass %s is not in the known-controller table.", g.res.Class.Spec.Controller, g.res.Name)
		}
	}
	addrs := ingressStatusAddrs(g)
	if len(addrs) > 0 {
		for i := range w.services {
			svc := &w.services[i]
			if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
				continue
			}
			have := map[string]bool{}
			for _, in := range svc.Status.LoadBalancer.Ingress {
				have[in.IP], have[in.Hostname] = true, true
			}
			covers := true
			for _, a := range addrs {
				if !have[a] {
					covers = false
				}
			}
			exposes := false
			for _, p := range svc.Spec.Ports {
				if p.Port == req.Port {
					exposes = true
				}
			}
			if covers && exposes {
				out.candidates = append(out.candidates, svc.Namespace+"/"+svc.Name)
			}
		}
		if len(out.candidates) == 1 {
			svc := w.svcByKey[out.candidates[0]]
			e := w.entryForService(svc)
			out.entry = &e
			out.via = fmt.Sprintf("Ingress status address %s + port %d match LoadBalancer %s", strings.Join(addrs, ", "), req.Port, out.candidates[0])
			return out
		}
		if len(out.candidates) == 0 {
			if out.why != "" {
				out.why += " "
			}
			out.why += fmt.Sprintf("No LoadBalancer Service carries all status addresses %s with port %d.", strings.Join(addrs, ", "), req.Port)
		}
	}
	if out.why == "" {
		out.why = "No IngressClass with a known controller and no status address to match against."
	}
	return out
}

func ingressStatusAddrs(g *ingressGroup) []string {
	var out []string
	for _, ing := range g.ingresses {
		for _, in := range ing.Status.LoadBalancer.Ingress {
			out = append(out, in.IP, in.Hostname)
		}
	}
	out = dedupe(out)
	sort.Strings(out)
	return out
}

func classStep(b *branchBuilder, g *ingressGroup, ctl controllerResolution) {
	ref := graphid.IngressClass(g.res.Name)
	st := Step{Hop: HopClass, Ref: ref}
	switch {
	case g.res.Found():
		st.Name = "IngressClass " + g.res.Name
		st.Status = StatusOK
		st.Detail = fmt.Sprintf("From %s; controller %s.", g.res.Source, g.res.Class.Spec.Controller)
		if ctl.via != "" {
			st.Detail += " Resolved via " + ctl.via + "."
		}
	case g.res.Name != "":
		st.Name = "IngressClass " + g.res.Name
		st.Status, st.Reason = StatusFail, ReasonClassNotFound
		st.Detail = fmt.Sprintf("IngressClass %q (from %s) does not exist, so no known controller serves these Ingresses.", g.res.Name, g.res.Source)
		st.Hint = "Create the IngressClass of the controller you run (kubectl get ingressclass), or point ingressClassName at an existing class."
		b.define(checker.TopologyNode{ID: ref, Kind: "ingressclass", Label: g.res.Name, Detail: "not found"})
	case len(g.res.Defaults) > 1:
		st.Name, st.Ref = "IngressClass (default)", ""
		st.Status, st.Reason = StatusWarn, ReasonAmbiguous
		st.Detail = "No class set and several default IngressClasses exist: " + strings.Join(g.res.Defaults, ", ") + "."
		st.Hint = "Set spec.ingressClassName explicitly or keep a single default class."
	default:
		st.Name, st.Ref = "IngressClass", ""
		st.Status, st.Reason = StatusFail, ReasonNoDefaultClass
		st.Detail = "The Ingress sets no class and no IngressClass is marked default."
		st.Hint = "Set spec.ingressClassName, or mark one IngressClass with " + ingressutil.DefaultClassAnnotation + "=true."
	}
	b.add(st)
}

// ingressPathCand is one path rule considered for the request.
type ingressPathCand struct {
	ing   *networkingv1.Ingress
	path  networkingv1.HTTPIngressPath
	match ingressutil.PathMatch
	tier  ingressutil.HostMatch
}

// ingressRoute applies host tiers (exact > wildcard > catch-all) and path
// precedence (Exact > longest Prefix, segment-wise; ImplementationSpecific
// as Prefix with a warning), then defaultBackend, else noRouteMatch.
func ingressRoute(ctx context.Context, w *world, b *branchBuilder, req Request, g *ingressGroup) {
	// Pick the most specific host tier present across the group.
	tier := ingressutil.NoMatch
	for _, ing := range g.ingresses {
		for _, r := range ing.Spec.Rules {
			if m := ingressutil.MatchHost(r.Host, req.Host); m > tier {
				tier = m
			}
		}
	}
	var cands []ingressPathCand
	for _, ing := range g.ingresses {
		for _, r := range ing.Spec.Rules {
			if tier == ingressutil.NoMatch || ingressutil.MatchHost(r.Host, req.Host) != tier || r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if m := ingressutil.MatchPath(p.PathType, p.Path, req.Path); m != ingressutil.PathNoMatch {
					cands = append(cands, ingressPathCand{ing: ing, path: p, match: m, tier: tier})
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, c := cands[i], cands[j]
		if a.match != c.match {
			return a.match > c.match
		}
		return ingressutil.PrefixLen(a.path.Path) > ingressutil.PrefixLen(c.path.Path)
	})

	if len(cands) == 0 {
		for _, ing := range g.ingresses {
			if ing.Spec.DefaultBackend != nil {
				ref := graphid.Ingress(ing.Namespace, ing.Name)
				b.add(Step{Name: "Route: Ingress " + ing.Namespace + "/" + ing.Name + " defaultBackend", Hop: HopRoute, Ref: ref, Status: StatusOK,
					Detail: fmt.Sprintf("No rule path matches %s; the Ingress defaultBackend takes the request.", req.Path)})
				ingressBackend(ctx, w, b, req, ing, *ing.Spec.DefaultBackend)
				return
			}
		}
		ing := g.ingresses[0]
		// D-040: a specific (exact/wildcard) host rule exists with paths,
		// only the requested path matches none of them and there is no
		// defaultBackend → warning, not a broken route. A catch-all rule
		// (no host) is a weak claim (D-038a) and keeps noRouteMatch.
		if served, first := servedIngressPaths(g, req, tier); first != nil && tier >= ingressutil.Wildcard {
			b.add(pathNotRoutedStep(fmt.Sprintf("Route: Ingress %s/%s — path %s not routed", first.Namespace, first.Name, req.Path),
				graphid.Ingress(first.Namespace, first.Name), `path "/" with pathType Prefix, or a spec.defaultBackend`, served))
			return
		}
		b.add(Step{Name: "Route for " + req.Host + req.Path, Hop: HopRoute, Ref: graphid.Ingress(ing.Namespace, ing.Name), Status: StatusFail,
			Reason: ReasonNoRouteMatch,
			Detail: fmt.Sprintf("No path of the matching Ingress rules covers %s and no defaultBackend is set.", req.Path),
			Hint:   "Add a path (e.g. Prefix /) for the request or a spec.defaultBackend."})
		return
	}
	c := cands[0]
	ref := graphid.Ingress(c.ing.Namespace, c.ing.Name)
	pt := "ImplementationSpecific"
	if c.path.PathType != nil {
		pt = string(*c.path.PathType)
	}
	path := c.path.Path
	if path == "" {
		path = "/"
	}
	st := Step{
		Name:   fmt.Sprintf("Route: Ingress %s/%s path %s (%s)", c.ing.Namespace, c.ing.Name, path, pt),
		Hop:    HopRoute,
		Ref:    ref,
		Status: StatusOK,
		Detail: fmt.Sprintf("%s rule host matches; path %s %s is the most specific match for %s (Exact > longest Prefix).", tierName(c.tier), pt, path, req.Path),
	}
	if pt == string(networkingv1.PathTypeImplementationSpecific) {
		st.Status = StatusWarn
		st.Detail += " ImplementationSpecific is controller-defined; it was evaluated as Prefix."
		st.Hint = "Use pathType Prefix or Exact for portable, predictable matching."
	}
	b.add(st)
	ingressBackend(ctx, w, b, req, c.ing, c.path.Backend)
}

// servedIngressPaths lists (sorted, distinct) the paths of the group's
// rules in the chosen host tier, plus the first Ingress that has any; nil
// when the matching rules carry no paths at all.
func servedIngressPaths(g *ingressGroup, req Request, tier ingressutil.HostMatch) ([]string, *networkingv1.Ingress) {
	var out []string
	var first *networkingv1.Ingress
	for _, ing := range g.ingresses {
		for _, r := range ing.Spec.Rules {
			if tier == ingressutil.NoMatch || ingressutil.MatchHost(r.Host, req.Host) != tier || r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if first == nil {
					first = ing
				}
				out = append(out, describeIngressPath(p))
			}
		}
	}
	out = dedupe(out)
	sort.Strings(out)
	return out, first
}

func describeIngressPath(p networkingv1.HTTPIngressPath) string {
	path := p.Path
	if path == "" {
		path = "/"
	}
	switch {
	case p.PathType != nil && *p.PathType == networkingv1.PathTypeExact:
		return "exact " + path
	case p.PathType != nil && *p.PathType == networkingv1.PathTypePrefix:
		return "prefix " + path
	}
	return "prefix " + path + " (implementation-specific)"
}

func tierName(t ingressutil.HostMatch) string {
	switch t {
	case ingressutil.Exact:
		return "Exact"
	case ingressutil.Wildcard:
		return "Wildcard"
	}
	return "Catch-all"
}

// ingressBackend resolves the backend port (number or Service port name)
// and runs the service tail.
func ingressBackend(ctx context.Context, w *world, b *branchBuilder, req Request, ing *networkingv1.Ingress, be networkingv1.IngressBackend) {
	if be.Service == nil {
		kind := "resource"
		if be.Resource != nil {
			kind = be.Resource.Kind + " " + be.Resource.Name
		}
		b.add(Step{Name: "Backend " + kind, Hop: HopBackend, Status: StatusSkipped,
			Detail: "Resource backends are not traced (no Service)."})
		return
	}
	ns, name := ing.Namespace, be.Service.Name
	port := be.Service.Port.Number
	if port == 0 && be.Service.Port.Name != "" {
		if svc := w.svcByKey[ns+"/"+name]; svc != nil {
			for _, p := range svc.Spec.Ports {
				if p.Name == be.Service.Port.Name {
					port = p.Port
				}
			}
			if port == 0 {
				b.add(Step{Name: "Backend port " + be.Service.Port.Name, Hop: HopBackend, Ref: graphid.Service(ns, name), Status: StatusFail,
					Reason: ReasonBackendPortMissing,
					Detail: fmt.Sprintf("Service %s/%s has no port named %q (ports: %s).", ns, name, be.Service.Port.Name, portList(svc)),
					Hint:   "Use an existing Service port name or number in the Ingress backend."})
				return
			}
		}
	}
	backendSteps(ctx, w, b, ns, name, port, "")
}
