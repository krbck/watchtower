package ingressutil

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cls(name string, def bool) networkingv1.IngressClass {
	c := networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if def {
		c.Annotations = map[string]string{DefaultClassAnnotation: "true"}
	}
	return c
}

// Order: spec.ingressClassName → legacy annotation → default class.
func TestResolveClassOrder(t *testing.T) {
	classes := []networkingv1.IngressClass{cls("a", false), cls("b", false), cls("def", true)}
	spec := "a"
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{LegacyClassAnnotation: "b"}},
		Spec:       networkingv1.IngressSpec{IngressClassName: &spec},
	}
	if r := ResolveClass(ing, classes); r.Name != "a" || r.Source != SourceSpec || !r.Found() {
		t.Fatalf("spec must win: %+v", r)
	}
	ing.Spec.IngressClassName = nil
	if r := ResolveClass(ing, classes); r.Name != "b" || r.Source != SourceAnnotation || !r.Found() {
		t.Fatalf("annotation second: %+v", r)
	}
	ing.Annotations = nil
	if r := ResolveClass(ing, classes); r.Name != "def" || r.Source != SourceDefault || !r.Found() {
		t.Fatalf("default last: %+v", r)
	}
	if r := ResolveClass(ing, []networkingv1.IngressClass{cls("x", true), cls("y", true)}); r.Name != "" || len(r.Defaults) != 2 {
		t.Fatalf("two defaults are ambiguous: %+v", r)
	}
	if r := ResolveClass(ing, nil); r.Source != SourceNone {
		t.Fatalf("no class: %+v", r)
	}
	missing := "nginx"
	ing.Spec.IngressClassName = &missing
	if r := ResolveClass(ing, classes); r.Name != "nginx" || r.Found() {
		t.Fatalf("missing class: %+v", r)
	}
}

func TestMatchHostSingleLabelWildcard(t *testing.T) {
	cases := []struct {
		rule, host string
		want       HostMatch
	}{
		{"foo.com", "foo.com", Exact},
		{"Foo.com", "foo.com", Exact},
		{"*.foo.com", "bar.foo.com", Wildcard},
		{"*.foo.com", "baz.bar.foo.com", NoMatch}, // exactly one label
		{"*.foo.com", "foo.com", NoMatch},
		{"", "anything.io", CatchAll},
		{"bar.com", "foo.com", NoMatch},
	}
	for _, c := range cases {
		if got := MatchHost(c.rule, c.host); got != c.want {
			t.Errorf("MatchHost(%q, %q) = %v, want %v", c.rule, c.host, got, c.want)
		}
	}
}

func TestMatchPath(t *testing.T) {
	prefix, exact, impl := networkingv1.PathTypePrefix, networkingv1.PathTypeExact, networkingv1.PathTypeImplementationSpecific
	cases := []struct {
		pt         *networkingv1.PathType
		rule, path string
		want       PathMatch
	}{
		{&prefix, "/", "/anything", PathPrefix},
		{&prefix, "/foo", "/foo", PathPrefix},
		{&prefix, "/foo", "/foo/", PathPrefix},
		{&prefix, "/foo/", "/foo", PathPrefix},
		{&prefix, "/foo", "/foo/bar", PathPrefix},
		{&prefix, "/foo", "/foobar", PathNoMatch}, // segment-wise
		{&exact, "/foo", "/foo", PathExact},
		{&exact, "/foo", "/foo/", PathNoMatch},
		{&impl, "/foo", "/foo/x", PathPrefix},
		{nil, "", "/x", PathPrefix},
	}
	for _, c := range cases {
		if got := MatchPath(c.pt, c.rule, c.path); got != c.want {
			t.Errorf("MatchPath(%v, %q, %q) = %v, want %v", c.pt, c.rule, c.path, got, c.want)
		}
	}
}
