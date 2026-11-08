// Package graphid holds the node ID scheme of the cluster graph. The
// topology mapper and the host trace (D-038) both emit these IDs so the
// Overview can highlight a trace directly on the mapper graph.
package graphid

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Namespace returns the namespace node ID.
func Namespace(name string) string { return "ns:" + name }

// Service returns the Service node ID.
func Service(ns, name string) string { return "svc:" + ns + "/" + name }

// Pod returns the standalone pod node ID.
func Pod(ns, name string) string { return "pod:" + ns + "/" + name }

// Deployment returns the Deployment node ID.
func Deployment(ns, name string) string { return "dep:" + ns + "/" + name }

// StatefulSet returns the StatefulSet node ID.
func StatefulSet(ns, name string) string { return "sts:" + ns + "/" + name }

// DaemonSet returns the DaemonSet node ID.
func DaemonSet(ns, name string) string { return "ds:" + ns + "/" + name }

// Ingress returns the Ingress node ID.
func Ingress(ns, name string) string { return "ingress:" + ns + "/" + name }

// IngressClass returns the (cluster-scoped) IngressClass node ID.
func IngressClass(name string) string { return "ingc:" + name }

// IstioGateway returns the Istio Gateway node ID.
func IstioGateway(ns, name string) string { return "gw:" + ns + "/" + name }

// VirtualService returns the Istio VirtualService node ID.
func VirtualService(ns, name string) string { return "vs:" + ns + "/" + name }

// TracePrefix marks nodes that exist only in a trace (never in the mapper
// graph), e.g. the queried host or an external address.
const TracePrefix = "trace:"

// PodOwner resolves the workload node ID that owns a pod from its
// ownerReferences: StatefulSet/DaemonSet directly; ReplicaSet → Deployment by
// stripping the pod-template hash (rs name = <deployment>-<hash>), which
// avoids watching ReplicaSets (minimal RBAC). Empty when not owned by one.
func PodOwner(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			if i := strings.LastIndexByte(ref.Name, '-'); i > 0 {
				return Deployment(pod.Namespace, ref.Name[:i])
			}
		case "StatefulSet":
			return StatefulSet(pod.Namespace, ref.Name)
		case "DaemonSet":
			return DaemonSet(pod.Namespace, ref.Name)
		}
	}
	return ""
}
