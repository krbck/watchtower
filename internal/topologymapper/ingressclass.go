package topologymapper

import (
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/krbck/watchtower/internal/checker"
	"github.com/krbck/watchtower/internal/graphid"
	"github.com/krbck/watchtower/internal/ingressutil"
)

// Warning reasons for Ingress class resolution (D-038 vocabulary).
const (
	ReasonClassNotFound  = "classNotFound"
	ReasonNoDefaultClass = "noDefaultClass"
	ReasonAmbiguous      = "ambiguous"
)

// ingressClassKind is the node kind of an IngressClass (cluster-scoped).
const ingressClassKind = "ingressclass"

// addIngressClasses adds one cluster-scoped node per IngressClass.
func addIngressClasses(b *builder, classes []networkingv1.IngressClass) {
	for i := range classes {
		c := &classes[i]
		meta := map[string]string{"controller": c.Spec.Controller}
		if strings.EqualFold(c.Annotations[ingressutil.DefaultClassAnnotation], "true") {
			meta["default"] = "true"
		}
		if k, ok := ingressutil.LookupController(c.Spec.Controller); ok {
			meta["workload"] = selectorString(k.Selector)
		}
		b.addNode(checker.TopologyNode{
			ID:     graphid.IngressClass(c.Name),
			Kind:   ingressClassKind,
			Label:  c.Name,
			Detail: c.Spec.Controller,
			Meta:   meta,
		})
	}
}

// linkIngressClass resolves an Ingress's class and adds the config edge
// class → ingress. A named class without an IngressClass object becomes a
// red ghost class node plus a classNotFound warning on the Ingress; no
// class and no (single) default class only warns (D-038).
func linkIngressClass(b *builder, node *checker.TopologyNode, ing *networkingv1.Ingress, classes []networkingv1.IngressClass) {
	res := ingressutil.ResolveClass(ing, classes)
	warn := func(reason, msg, hint string) {
		node.Warnings = append(node.Warnings, checker.NodeWarning{Reason: reason, Message: msg, Hint: hint})
	}
	switch {
	case res.Found():
		node.Meta["class"] = res.Name
	case res.Name != "":
		node.Meta["class"] = res.Name + " (not found)"
		warn(ReasonClassNotFound,
			"IngressClass "+res.Name+" ("+string(res.Source)+") does not exist, so no known controller serves this Ingress",
			"Create the IngressClass for the controller you run (kubectl get ingressclass) or point ingressClassName at an existing class.")
		ghostID := graphid.IngressClass(res.Name)
		if !b.has(ghostID) {
			f := false
			b.addNode(checker.TopologyNode{
				ID:     ghostID,
				Kind:   ingressClassKind,
				Label:  res.Name,
				OK:     &f,
				Detail: "not found",
				Meta:   map[string]string{"status": "not found"},
				Warnings: []checker.NodeWarning{{
					Reason:  ReasonClassNotFound,
					Message: "IngressClass " + res.Name + " is referenced by Ingresses but does not exist",
					Hint:    "Create it (spec.controller of the controller you run) or fix the references.",
				}},
			})
		}
		f := false
		b.addEdge(checker.TopologyEdge{From: ghostID, To: node.ID, Label: "class", OK: &f, Config: true})
		return
	case len(res.Defaults) > 1:
		warn(ReasonAmbiguous,
			"Ingress has no class and several IngressClasses are marked default ("+strings.Join(res.Defaults, ", ")+")",
			"Set spec.ingressClassName explicitly, or keep only one default IngressClass.")
		return
	default:
		warn(ReasonNoDefaultClass,
			"Ingress has no class (spec.ingressClassName / "+ingressutil.LegacyClassAnnotation+") and no default IngressClass exists",
			"Set spec.ingressClassName, or mark one IngressClass with "+ingressutil.DefaultClassAnnotation+"=true.")
		return
	}
	b.addEdge(checker.TopologyEdge{From: graphid.IngressClass(res.Name), To: node.ID, Label: "class", Config: true})
}
