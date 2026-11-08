// Package v1alpha1 contains the Watchtower API types (group: watchtower.krbck.io).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "watchtower.krbck.io", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)

// CheckStep is a single step of the connectivity chain.
type CheckStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// TopologyNode is a node in the connectivity graph; the UI renders it.
type TopologyNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // client | service | pod | port | app | namespace | deployment | statefulset | daemonset | ingress | gateway | virtualservice
	Label string `json:"label"`
	// Namespace is optional (cluster-scoped nodes leave it empty).
	Namespace string `json:"namespace,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Meta carries structured fields rendered by the UI card.
	Meta map[string]string `json:"meta,omitempty"`
	// Ports is the structured port list rendered as a dedicated block.
	Ports []PortInfo `json:"ports,omitempty"`
	// Warnings explains configuration issues found on this node (D-037);
	// precursor of the P1 findings contract.
	Warnings []NodeWarning `json:"warnings,omitempty"`
}

// NodeWarning is a single explained issue attached to a topology node.
type NodeWarning struct {
	// Reason is a stable machine-readable code (e.g. targetPortNotDeclared).
	Reason string `json:"reason,omitempty"`
	// Message states what is wrong, in human terms.
	Message string `json:"message,omitempty"`
	// Hint suggests how to fix it.
	Hint string `json:"hint,omitempty"`
	// Items is a long list moved out of the message (e.g. a gateway's server
	// hosts); UIs show it on demand.
	Items []string `json:"items,omitempty"`
}

// PortInfo describes a port exposed by a service or declared by a container.
type PortInfo struct {
	Port       int32  `json:"port"`
	TargetPort string `json:"targetPort,omitempty"`
	NodePort   int32  `json:"nodePort,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Matched    bool   `json:"matched,omitempty"`
	// Reason is a machine-readable code set when the port is not matched
	// (targetPortNotDeclared | namedPortUnresolved | noBackend | noSelector).
	Reason string `json:"reason,omitempty"`
	// Hint is the human explanation of Reason plus the suggested fix.
	Hint string `json:"hint,omitempty"`
}

// TopologyEdge is a directed edge in the connectivity graph.
type TopologyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
	OK    *bool  `json:"ok,omitempty"`
}

// Topology is the traffic-flow graph stored in the status; the UI renders it.
type Topology struct {
	Nodes []TopologyNode `json:"nodes,omitempty"`
	Edges []TopologyEdge `json:"edges,omitempty"`
}

// ConnectivityCheckSpec holds the check parameters.
type ConnectivityCheckSpec struct {
	// Target is the workload name to check (deployment/statefulset/pod prefix).
	// +kubebuilder:validation:MinLength=1
	Target string `json:"target"`

	// ServiceName is the Service to check; when empty it defaults to Target.
	ServiceName string `json:"serviceName,omitempty"`

	// Port is the port the app listens on; when 0 the chain runs in
	// discovery mode.
	Port int32 `json:"port,omitempty"`
}

// ConnectivityCheckStatus is the outcome of a chain run.
type ConnectivityCheckStatus struct {
	// State: Pending | Passed | Failed
	State string `json:"state,omitempty"`

	Steps []CheckStep `json:"steps,omitempty"`

	// Topology is the graph form of the chain result (rendered by the UI).
	Topology Topology `json:"topology,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	LastCheckedAt *metav1.Time `json:"lastCheckedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConnectivityCheck verifies the Service→Pod→Port chain of a workload.
type ConnectivityCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectivityCheckSpec   `json:"spec,omitempty"`
	Status ConnectivityCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectivityCheckList is a collection of ConnectivityCheck.
type ConnectivityCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConnectivityCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConnectivityCheck{}, &ConnectivityCheckList{})
}

// DeepCopyObject implementations (generated by hand while controller-gen is
// not available; to be replaced by zz_generated.deepcopy.go once codegen
// is wired up).

func (in *ConnectivityCheck) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(ConnectivityCheck)
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
	return out
}

func (in *ConnectivityCheckList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(ConnectivityCheckList)
	*out = *in
	out.Items = make([]ConnectivityCheck, len(in.Items))
	for i := range in.Items {
		in.Items[i].DeepCopyInto(&out.Items[i])
	}
	return out
}

func (in *ConnectivityCheckSpec) DeepCopyInto(out *ConnectivityCheckSpec) {
	*out = *in
}

// DeepCopy returns a deep copy of the status.
func (in *ConnectivityCheckStatus) DeepCopy() *ConnectivityCheckStatus {
	if in == nil {
		return nil
	}
	out := new(ConnectivityCheckStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *ConnectivityCheckStatus) DeepCopyInto(out *ConnectivityCheckStatus) {
	*out = *in
	if in.Steps != nil {
		out.Steps = make([]CheckStep, len(in.Steps))
		copy(out.Steps, in.Steps)
	}
	in.Topology.DeepCopyInto(&out.Topology)
	if in.LastCheckedAt != nil {
		t := *in.LastCheckedAt
		out.LastCheckedAt = &t
	}
}

// DeepCopyInto copies the topology graph, deep-copying pointer and map fields.
func (in *Topology) DeepCopyInto(out *Topology) {
	*out = *in
	if in.Nodes != nil {
		out.Nodes = make([]TopologyNode, len(in.Nodes))
		for i := range in.Nodes {
			out.Nodes[i] = in.Nodes[i]
			if in.Nodes[i].OK != nil {
				v := *in.Nodes[i].OK
				out.Nodes[i].OK = &v
			}
			if in.Nodes[i].Meta != nil {
				out.Nodes[i].Meta = make(map[string]string, len(in.Nodes[i].Meta))
				for k, v := range in.Nodes[i].Meta {
					out.Nodes[i].Meta[k] = v
				}
			}
			if in.Nodes[i].Ports != nil {
				out.Nodes[i].Ports = make([]PortInfo, len(in.Nodes[i].Ports))
				copy(out.Nodes[i].Ports, in.Nodes[i].Ports)
			}
			if in.Nodes[i].Warnings != nil {
				out.Nodes[i].Warnings = make([]NodeWarning, len(in.Nodes[i].Warnings))
				copy(out.Nodes[i].Warnings, in.Nodes[i].Warnings)
				for j := range in.Nodes[i].Warnings {
					if in.Nodes[i].Warnings[j].Items != nil {
						out.Nodes[i].Warnings[j].Items = append([]string{}, in.Nodes[i].Warnings[j].Items...)
					}
				}
			}
		}
	}
	if in.Edges != nil {
		out.Edges = make([]TopologyEdge, len(in.Edges))
		for i := range in.Edges {
			out.Edges[i] = in.Edges[i]
			if in.Edges[i].OK != nil {
				v := *in.Edges[i].OK
				out.Edges[i].OK = &v
			}
		}
	}
}

func (in *ConnectivityCheck) DeepCopyInto(out *ConnectivityCheck) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ConnectivityCheckList) DeepCopyInto(out *ConnectivityCheckList) {
	*out = *in
	out.Items = make([]ConnectivityCheck, len(in.Items))
	for i := range in.Items {
		in.Items[i].DeepCopyInto(&out.Items[i])
	}
}
