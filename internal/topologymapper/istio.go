package topologymapper

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/integrations/istio"
)

// Warning reasons for Istio routing objects (D-037). Stable API codes.
const (
	ReasonGatewayNoWorkload         = "gatewayNoWorkload"
	ReasonVSGatewayNotFound         = "vsGatewayNotFound"
	ReasonVSHostNotExposed          = "vsHostNotExposed"
	ReasonVSDestinationNotFound     = "vsDestinationNotFound"
	ReasonVSDestinationPortNotFound = "vsDestinationPortNotFound"
	ReasonVSHostNotFullyQualified   = "vsHostNotFullyQualified"
	ReasonVSExternalDestination     = "vsExternalDestination"
)

// Node kinds of the Istio routing objects.
const (
	istioGatewayKind = "gateway"
	istioVSKind      = "virtualservice"
)

func gwID(ns, name string) string { return graphid.IstioGateway(ns, name) }
func vsID(ns, name string) string { return graphid.VirtualService(ns, name) }

// istioInputs is the cluster state the Istio graph is resolved against.
type istioInputs struct {
	services   map[string]*corev1.Service // "ns/name"
	nsExists   func(string) bool
	workloads  []workload
	standalone []*corev1.Pod
}

// addIstio adds gateway and virtualservice nodes and the north-south chain
//
//	LB service → gateway workload → gateway → virtualservice → service
//
// Edges follow traffic: workload → gateway ("serves", Gateway spec.selector
// against pod-template labels in any namespace — Istio's default gateway
// selection scope), gateway → virtualservice ("binds", spec.gateways) and
// virtualservice → service ("routes → <ports>", destination hosts).
// Namespace anchoring (D-032) is left to addNamespaceEdges: only chain roots
// get a namespace edge, so a served gateway and a bound virtualservice get
// none.
func addIstio(b *builder, snap *istio.Snapshot, in istioInputs) {
	gateways := make(map[string]*istio.Gateway, len(snap.Gateways))
	for i := range snap.Gateways {
		gw := &snap.Gateways[i]
		gateways[gw.Key()] = gw
		if isSystemNamespace(gw.Namespace) {
			continue
		}
		id := gwID(gw.Namespace, gw.Name)
		node := checker.TopologyNode{
			ID:        id,
			Kind:      istioGatewayKind,
			Label:     gw.Name,
			Namespace: gw.Namespace,
			Detail:    serversSummary(gw),
			Meta:      gatewayMeta(gw),
		}
		served := false
		if len(gw.Selector) > 0 {
			for j := range in.workloads {
				w := &in.workloads[j]
				// Same label logic as Service → workload: pod template
				// plus live pod labels (D-037 deviation 1).
				if w.selectedBy(gw.Selector) {
					b.addEdge(checker.TopologyEdge{From: w.id, To: id, Label: "serves"})
					served = true
				}
			}
			for _, pod := range in.standalone {
				if selectorMatches(gw.Selector, pod.Labels) {
					b.addEdge(checker.TopologyEdge{From: b.podID(pod.Namespace, pod.Name), To: id, Label: "serves"})
					served = true
				}
			}
		}
		if !served {
			sel := selectorString(gw.Selector)
			msg := "Gateway selector " + sel + " matches no workload or pod"
			if sel == "" {
				msg = "Gateway has no selector, so no gateway workload is bound to it"
			}
			node.Warnings = append(node.Warnings, checker.NodeWarning{
				Reason:  ReasonGatewayNoWorkload,
				Message: msg,
				Hint: "Set spec.selector to the labels of the Istio ingress/egress gateway pods " +
					"(e.g. istio=ingressgateway) and make sure that gateway deployment is running.",
			})
		}
		b.addNode(node)
	}

	for i := range snap.VirtualServices {
		vs := &snap.VirtualServices[i]
		if isSystemNamespace(vs.Namespace) {
			continue
		}
		addVirtualService(b, vs, gateways, in)
	}
}

func addVirtualService(b *builder, vs *istio.VirtualService, gateways map[string]*istio.Gateway, in istioInputs) {
	id := vsID(vs.Namespace, vs.Name)
	node := checker.TopologyNode{
		ID:        id,
		Kind:      istioVSKind,
		Label:     vs.Name,
		Namespace: vs.Namespace,
		Detail:    strings.Join(vs.Hosts, ", "),
		Meta:      map[string]string{},
	}
	warn := func(reason, msg, hint string) {
		node.Warnings = append(node.Warnings, checker.NodeWarning{Reason: reason, Message: msg, Hint: hint})
	}
	if len(vs.Hosts) > 0 {
		node.Meta["hosts"] = strings.Join(vs.Hosts, ", ")
	}
	if len(vs.Gateways) > 0 {
		node.Meta["gateways"] = strings.Join(vs.Gateways, ", ")
	}

	// Gateway bindings + host exposure check.
	var edges []checker.TopologyEdge
	for _, ref := range vs.Gateways {
		ns, name, ok := istio.ResolveGatewayRef(ref, vs.Namespace)
		if !ok {
			continue // "mesh": sidecar routing, no Gateway object
		}
		gw := gateways[ns+"/"+name]
		if gw == nil {
			warn(ReasonVSGatewayNotFound,
				"gateway "+ns+"/"+name+" referenced in spec.gateways does not exist",
				"Create the Gateway or fix the reference (\"<namespace>/<name>\"; a bare name means the VirtualService namespace "+vs.Namespace+").")
			continue
		}
		var unexposed []string
		for _, h := range vs.Hosts {
			if !istio.GatewayExposes(gw, h, vs.Namespace) {
				unexposed = append(unexposed, h)
			}
		}
		if len(unexposed) > 0 {
			hosts := append([]string{}, gatewayHosts(gw)...)
			sort.Strings(hosts)
			node.Warnings = append(node.Warnings, checker.NodeWarning{
				Reason:  ReasonVSHostNotExposed,
				Message: "host " + strings.Join(unexposed, ", ") + " is not exposed by gateway " + gw.Key() + " (" + serverHostCount(len(hosts)) + ")",
				Hint: "Add the host to the Gateway spec.servers[].hosts (or a matching wildcard such as *.domain), or remove it from the VirtualService; " +
					"Istio only applies a VirtualService host to a Gateway server whose hosts match it.",
				Items: hosts,
			})
		}
		e := checker.TopologyEdge{From: gwID(gw.Namespace, gw.Name), To: id, Label: "binds"}
		if len(vs.Hosts) > 0 && len(unexposed) == len(vs.Hosts) {
			f := false
			e.OK = &f
		}
		edges = append(edges, e)
	}

	// Destinations, grouped per Service so one edge carries all its ports.
	type dest struct {
		key   string
		ports []string
		bad   bool
	}
	var dests []*dest
	byKey := map[string]*dest{}
	var summary []string
	for _, r := range vs.Routes {
		target := r.Host
		if r.Port != 0 {
			target += ":" + itoa(int(r.Port))
		}
		summary = append(summary, r.Match+" → "+target)

		ns, name, form := istio.ResolveHost(r.Host, vs.Namespace, in.nsExists)
		if form == istio.HostExternal {
			warn(ReasonVSExternalDestination,
				"destination host "+r.Host+" is not a Service in this cluster",
				"If this is intended (external service via ServiceEntry), ignore; otherwise use the Service short name or its FQDN name.namespace.svc.cluster.local.")
			continue
		}
		svc := in.services[ns+"/"+name]
		if svc == nil {
			warn(ReasonVSDestinationNotFound,
				"destination host "+r.Host+" resolves to Service "+ns+"/"+name+", which does not exist",
				"Create the Service or fix destination.host (a short name is resolved in the VirtualService namespace "+vs.Namespace+").")
			continue
		}
		d := byKey[ns+"/"+name]
		if d == nil {
			d = &dest{key: ns + "/" + name}
			byKey[d.key] = d
			dests = append(dests, d)
		}
		if form == istio.HostPartial {
			warn(ReasonVSHostNotFullyQualified,
				"destination host "+r.Host+" is partially qualified; Istio expands only short names, so it is taken literally and does not match Service "+ns+"/"+name,
				"Use \""+name+"\" (if the VirtualService is in "+ns+") or the FQDN \""+name+"."+ns+".svc.cluster.local\".")
			d.bad = true
		}
		if r.Port != 0 {
			p := itoa(int(r.Port))
			if !serviceHasPort(svc, int32(r.Port)) {
				warn(ReasonVSDestinationPortNotFound,
					"destination port "+p+" is not a port of Service "+ns+"/"+name+" (ports: "+servicePortList(svc)+")",
					"Set destination.port.number to one of the Service ports.")
				d.bad = true
			}
			if !contains(d.ports, p) {
				d.ports = append(d.ports, p)
			}
		}
	}
	if len(summary) > 0 {
		node.Meta["routes"] = strings.Join(summary, "; ")
	}
	b.addNode(node)

	for _, e := range edges {
		if b.has(e.From) {
			b.addEdge(e)
		}
	}
	for _, d := range dests {
		ns, name, _ := strings.Cut(d.key, "/")
		svcID := b.svcID(ns, name)
		if !b.has(svcID) {
			continue // service in a hidden (system) namespace
		}
		label := "routes"
		if len(d.ports) > 0 {
			label += " → " + strings.Join(d.ports, ", ")
		}
		e := checker.TopologyEdge{From: id, To: svcID, Label: label}
		if d.bad {
			f := false
			e.OK = &f
		}
		b.addEdge(e)
	}
}

// serversSummary renders "http:80 → 7 hosts; https:443 → 1 host".
func serversSummary(gw *istio.Gateway) string {
	parts := make([]string, 0, len(gw.Servers))
	for _, s := range gw.Servers {
		proto := strings.ToLower(s.Protocol)
		if proto == "" {
			proto = s.PortName
		}
		n := len(s.Hosts)
		unit := " hosts"
		if n == 1 {
			unit = " host"
		}
		parts = append(parts, proto+":"+itoa(int(s.Port))+" → "+itoa(n)+unit)
	}
	return strings.Join(parts, "; ")
}

func gatewayMeta(gw *istio.Gateway) map[string]string {
	meta := map[string]string{}
	if s := serversSummary(gw); s != "" {
		meta["servers"] = s
	}
	if h := gatewayHosts(gw); len(h) > 0 {
		meta["hosts"] = strings.Join(h, ", ")
	}
	if sel := selectorString(gw.Selector); sel != "" {
		meta["selector"] = sel
	}
	return meta
}

func serverHostCount(n int) string {
	if n == 1 {
		return "1 server host"
	}
	return strconv.Itoa(n) + " server hosts"
}

// gatewayHosts lists the distinct server hosts in declaration order.
func gatewayHosts(gw *istio.Gateway) []string {
	var out []string
	for _, s := range gw.Servers {
		for _, h := range s.Hosts {
			if !contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}

func serviceHasPort(svc *corev1.Service, port int32) bool {
	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

func servicePortList(svc *corev1.Service) string {
	ports := make([]int, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, int(p.Port))
	}
	sort.Ints(ports)
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, itoa(p))
	}
	return strings.Join(out, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
