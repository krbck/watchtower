package v1alpha1

import "testing"

// D-037: warnings and explained ports must be deep-copied (CR status).
func TestTopologyDeepCopyWarnings(t *testing.T) {
	in := ConnectivityCheckStatus{Topology: Topology{Nodes: []TopologyNode{{
		ID:       "svc",
		Ports:    []PortInfo{{Port: 8082, Reason: "targetPortNotDeclared", Hint: "h"}},
		Warnings: []NodeWarning{{Reason: "targetPortNotDeclared", Message: "m", Hint: "h", Items: []string{"a"}}},
	}}}}
	out := in.DeepCopy()
	out.Topology.Nodes[0].Warnings[0].Message = "changed"
	out.Topology.Nodes[0].Warnings[0].Items[0] = "changed"
	if in.Topology.Nodes[0].Warnings[0].Items[0] != "a" {
		t.Error("Items not deep-copied")
	}
	out.Topology.Nodes[0].Ports[0].Hint = "changed"
	if in.Topology.Nodes[0].Warnings[0].Message != "m" || in.Topology.Nodes[0].Ports[0].Hint != "h" {
		t.Fatal("DeepCopy must not share warnings/ports backing arrays")
	}
	if out.Topology.Nodes[0].Ports[0].Reason != "targetPortNotDeclared" {
		t.Fatal("reason not copied")
	}
}
