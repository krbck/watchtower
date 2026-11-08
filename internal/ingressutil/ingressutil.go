// Package ingressutil holds Ingress semantics shared by the topology mapper
// (class nodes, D-038) and the host trace: IngressClass resolution, the
// known-controller table and Ingress host/path matching.
package ingressutil

import (
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// Annotations used for class resolution.
const (
	// LegacyClassAnnotation is the pre-1.18 class annotation, still honored
	// by most controllers.
	LegacyClassAnnotation = "kubernetes.io/ingress.class"
	// DefaultClassAnnotation marks the cluster default IngressClass.
	DefaultClassAnnotation = "ingressclass.kubernetes.io/is-default-class"
)

// ClassSource tells where the class name came from.
type ClassSource string

const (
	SourceSpec       ClassSource = "spec.ingressClassName"
	SourceAnnotation ClassSource = "annotation " + LegacyClassAnnotation
	SourceDefault    ClassSource = "default IngressClass"
	SourceNone       ClassSource = "none"
)

// ClassResolution is the outcome of resolving an Ingress's class.
type ClassResolution struct {
	// Name is the class name (empty when none could be determined).
	Name   string
	Source ClassSource
	// Class is the IngressClass object (nil when not found).
	Class *networkingv1.IngressClass
	// Defaults lists the default classes when more than one is marked
	// default (ambiguous) — sorted.
	Defaults []string
}

// Found reports whether an IngressClass object backs the resolution.
func (r ClassResolution) Found() bool { return r.Class != nil }

// ResolveClass resolves the class of an Ingress in the order controllers use:
// spec.ingressClassName → legacy annotation → the default IngressClass.
func ResolveClass(ing *networkingv1.Ingress, classes []networkingv1.IngressClass) ClassResolution {
	byName := make(map[string]*networkingv1.IngressClass, len(classes))
	for i := range classes {
		byName[classes[i].Name] = &classes[i]
	}
	if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
		n := *ing.Spec.IngressClassName
		return ClassResolution{Name: n, Source: SourceSpec, Class: byName[n]}
	}
	if n := strings.TrimSpace(ing.Annotations[LegacyClassAnnotation]); n != "" {
		return ClassResolution{Name: n, Source: SourceAnnotation, Class: byName[n]}
	}
	defs := DefaultClasses(classes)
	switch len(defs) {
	case 0:
		return ClassResolution{Source: SourceNone}
	case 1:
		return ClassResolution{Name: defs[0], Source: SourceDefault, Class: byName[defs[0]]}
	}
	return ClassResolution{Source: SourceDefault, Defaults: defs}
}

// DefaultClasses returns the names of the classes annotated as default,
// sorted.
func DefaultClasses(classes []networkingv1.IngressClass) []string {
	var out []string
	for i := range classes {
		if strings.EqualFold(classes[i].Annotations[DefaultClassAnnotation], "true") {
			out = append(out, classes[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

// KnownController maps an IngressClass spec.controller to the pod labels of
// its controller workload (D-038 table). The selector is matched in every
// namespace — controllers usually live in system namespaces (traefik and
// k3s svclb in kube-system), so no system-namespace filter applies.
type KnownController struct {
	Controller string
	Name       string
	Selector   map[string]string
}

// KnownControllers is the controller → workload table.
var KnownControllers = []KnownController{
	{Controller: "k8s.io/ingress-nginx", Name: "ingress-nginx", Selector: map[string]string{"app.kubernetes.io/name": "ingress-nginx"}},
	{Controller: "traefik.io/ingress-controller", Name: "traefik", Selector: map[string]string{"app.kubernetes.io/name": "traefik"}},
	{Controller: "istio.io/ingress-controller", Name: "istio", Selector: map[string]string{"istio": "ingressgateway"}},
}

// LookupController returns the table entry for a controller string.
func LookupController(controller string) (KnownController, bool) {
	for _, k := range KnownControllers {
		if k.Controller == controller {
			return k, true
		}
	}
	return KnownController{}, false
}

// HostMatch classifies how an Ingress rule host matches a request host.
type HostMatch int

const (
	// NoMatch: the rule does not apply.
	NoMatch HostMatch = iota
	// CatchAll: the rule has no host (applies to every host).
	CatchAll
	// Wildcard: "*.example.com" matching exactly one extra label.
	Wildcard
	// Exact: the rule host equals the request host.
	Exact
)

// MatchHost applies Ingress host semantics: exact (case-insensitive), a
// "*.suffix" wildcard covers exactly ONE label ("*.foo.com" matches
// "bar.foo.com" but neither "foo.com" nor "baz.bar.foo.com"), and an empty
// rule host matches every host.
func MatchHost(ruleHost, host string) HostMatch {
	ruleHost = strings.ToLower(strings.TrimSpace(ruleHost))
	host = strings.ToLower(host)
	if ruleHost == "" {
		return CatchAll
	}
	if ruleHost == host {
		return Exact
	}
	if strings.HasPrefix(ruleHost, "*.") {
		suffix := ruleHost[1:] // ".foo.com"
		if strings.HasSuffix(host, suffix) {
			label := strings.TrimSuffix(host, suffix)
			if label != "" && !strings.Contains(label, ".") {
				return Wildcard
			}
		}
	}
	return NoMatch
}

// PathMatch is the outcome of matching one Ingress path.
type PathMatch int

const (
	PathNoMatch PathMatch = iota
	// PathPrefix: a Prefix (or ImplementationSpecific treated as Prefix)
	// match; longer prefixes win.
	PathPrefix
	// PathExact: an Exact match; always beats Prefix.
	PathExact
)

// MatchPath applies Ingress pathType semantics to a request path. Prefix
// matching is element-wise on "/"-separated segments ("/foo" matches "/foo"
// and "/foo/bar" but not "/foobar"; a trailing slash is ignored).
// ImplementationSpecific is treated as Prefix (the caller warns). An empty
// path means "/".
func MatchPath(pathType *networkingv1.PathType, rulePath, reqPath string) PathMatch {
	if rulePath == "" {
		rulePath = "/"
	}
	pt := networkingv1.PathTypeImplementationSpecific
	if pathType != nil {
		pt = *pathType
	}
	if pt == networkingv1.PathTypeExact {
		if rulePath == reqPath {
			return PathExact
		}
		return PathNoMatch
	}
	if prefixMatches(rulePath, reqPath) {
		return PathPrefix
	}
	return PathNoMatch
}

func prefixMatches(prefix, path string) bool {
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		return true // "/" matches everything
	}
	q := strings.TrimRight(path, "/")
	if q == p {
		return true
	}
	return strings.HasPrefix(q, p+"/")
}

// PrefixLen is the specificity of a prefix path (length without the
// trailing slash) used for longest-match ordering.
func PrefixLen(rulePath string) int { return len(strings.TrimRight(rulePath, "/")) }
