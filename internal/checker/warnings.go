package checker

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Machine-readable reasons for explained port issues (D-037). They are part
// of the API surface (PortInfo.Reason / NodeWarning.Reason): keep them stable.
const (
	// ReasonTargetPortNotDeclared: a numeric targetPort is not declared as a
	// containerPort by any selected backend.
	ReasonTargetPortNotDeclared = "targetPortNotDeclared"
	// ReasonNamedPortUnresolved: a named targetPort matches no named
	// containerPort of the selected backends.
	ReasonNamedPortUnresolved = "namedPortUnresolved"
	// ReasonNoBackend: the Service selector selects no workload or pod.
	ReasonNoBackend = "noBackend"
	// ReasonNoSelector: selector-less Service (manual endpoints or
	// ExternalName); targetPort cannot be checked against containers.
	ReasonNoSelector = "noSelector"
)

// DeclaredPort is a containerPort declared by a backend (name may be empty).
type DeclaredPort struct {
	Name string
	Port int32
}

// BackendPorts lists the containerPorts declared by one selected backend
// (workload or pod), identified by its display name.
type BackendPorts struct {
	Name  string
	Ports []DeclaredPort
}

// TargetPortWarning explains why a Service targetPort did not resolve
// against the selected backends' containerPorts. named=true means the
// targetPort is a port name (targetName); otherwise targetNum is the
// effective numeric targetPort.
//
// Kubernetes nuance (kept accurate on purpose): a numeric targetPort does
// not need a matching containerPort — containerPort is informational and
// traffic still flows if the process listens on the port. A named
// targetPort, however, must resolve to a named containerPort on each pod;
// pods where it does not resolve get no endpoint for that Service port.
func TargetPortWarning(named bool, targetName string, targetNum int32, backends []BackendPorts) NodeWarning {
	declared := describeBackends(backends)
	if named {
		return NodeWarning{
			Reason: ReasonNamedPortUnresolved,
			Message: fmt.Sprintf("targetPort %q does not match any named containerPort of the selected workloads (%s)",
				targetName, declared),
			Hint: fmt.Sprintf("Name a containerPort %q in the pod template, or set the Service targetPort to a declared port name or number. "+
				"Unlike a numeric targetPort, an unresolved port name means the pods get no endpoint for this Service port, so traffic to it fails.",
				targetName),
		}
	}
	return NodeWarning{
		Reason: ReasonTargetPortNotDeclared,
		Message: fmt.Sprintf("targetPort %d is not declared as a containerPort by any selected workload (%s)",
			targetNum, declared),
		Hint: fmt.Sprintf("Add containerPort %d to the pod template, or point the Service targetPort at a declared port. "+
			"Note: undeclared ports still work if the process listens on them — containerPort is informational.", targetNum),
	}
}

// NoBackendWarning explains a Service whose selector selects nothing.
func NoBackendWarning(selector, namespace string) NodeWarning {
	return NodeWarning{
		Reason:  ReasonNoBackend,
		Message: fmt.Sprintf("Service selector %s matches no workload or pod in namespace %s", selector, namespace),
		Hint: "Align the Service selector with the pod template labels of the intended workload; " +
			"without selected pods the Service has no endpoints.",
	}
}

// NoSelectorWarning explains why ports of a selector-less Service are not
// checked (informational; not added as a node warning).
func NoSelectorWarning() NodeWarning {
	return NodeWarning{
		Reason:  ReasonNoSelector,
		Message: "Service has no selector",
		Hint: "Endpoints are managed outside label selectors (manual Endpoints/EndpointSlices or ExternalName), " +
			"so targetPort is not checked against containerPorts.",
	}
}

// PortHint folds a warning into the single PortInfo.Hint string
// (message + fix), used for the UI tooltip.
func PortHint(w NodeWarning) string {
	if w.Message == "" {
		return w.Hint
	}
	return w.Message + ". " + w.Hint
}

// describeBackends renders "a declares 8080, http=8081; b declares no
// containerPorts" with de-duplicated, sorted ports per backend.
func describeBackends(backends []BackendPorts) string {
	if len(backends) == 0 {
		return "no backend selected"
	}
	parts := make([]string, 0, len(backends))
	for _, b := range backends {
		seen := map[string]bool{}
		ports := append([]DeclaredPort(nil), b.Ports...)
		sort.SliceStable(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
		labels := make([]string, 0, len(ports))
		for _, p := range ports {
			l := strconv.Itoa(int(p.Port))
			if p.Name != "" {
				l = p.Name + "=" + l
			}
			if seen[l] {
				continue
			}
			seen[l] = true
			labels = append(labels, l)
		}
		if len(labels) == 0 {
			parts = append(parts, b.Name+" declares no containerPorts")
			continue
		}
		parts = append(parts, b.Name+" declares "+strings.Join(labels, ", "))
	}
	return strings.Join(parts, "; ")
}
