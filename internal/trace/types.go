// Package trace answers "where does traffic for host[/path] go inside the
// cluster?" (D-038). It is a pure, deterministic engine over a Source:
// core types come from the manager's informer cache, Istio objects from the
// Istio integration snapshot (D-036: never listed here). Every claiming
// entry point becomes a branch of ordered hops in traffic direction:
//
//	host claim → external address → entry Service + endpoints → gateway /
//	controller workload → class / controller → listener / server → route →
//	backend Service + port → EndpointSlice ready → targetPort↔containerPort
//
// Node IDs follow the topology mapper scheme (internal/graphid) so the
// Overview can highlight a branch directly; trace-only nodes use the
// "trace:" prefix.
package trace

import "github.com/krbck/watchtower/internal/checker"

// Overall and branch states.
const (
	StatePassed = "Passed"
	// StateWarning: the route configuration is correct, but the request
	// would still not be served (D-040: the host is routed, the path
	// matches no rule). Ordering: Failed > Warning > Partial > Passed.
	StateWarning  = "Warning"
	StateFailed   = "Failed"
	StatePartial  = "Partial"
	StateNotFound = "NotFound"
)

// Step statuses (shared with checker.Step.Status).
const (
	StatusOK      = checker.StatusOK
	StatusFail    = checker.StatusFail
	StatusWarn    = checker.StatusWarn
	StatusUnknown = checker.StatusUnknown
	StatusSkipped = checker.StatusSkipped
)

// Hops in traffic order.
const (
	HopOutside      = "outside"
	HopClaim        = "claim"
	HopAddress      = "address"
	HopEntryService = "entryService"
	HopWorkload     = "workload"
	HopClass        = "class"
	HopListener     = "listener"
	HopRoute        = "route"
	HopBackend      = checker.HopBackend
	HopEndpoints    = checker.HopEndpoints
	HopPods         = checker.HopPods
	HopPort         = checker.HopPort
)

// Reason codes (D-038 vocabulary, shared with the P1 findings engine).
// Stable API surface: never rename.
const (
	ReasonHostNotClaimed          = "hostNotClaimed"
	ReasonNoExternalAddress       = "noExternalAddress"
	ReasonLBPending               = "lbPending"
	ReasonGatewaySelectorNoPods   = "gatewaySelectorNoPods"
	ReasonGatewayWorkloadNotReady = "gatewayWorkloadNotReady"
	ReasonGatewayPortNotExposed   = "gatewayPortNotExposed"
	ReasonControllerUnknown       = "controllerUnknown"
	ReasonClassNotFound           = "classNotFound"
	ReasonNoDefaultClass          = "noDefaultClass"
	ReasonListenerPortMismatch    = "listenerPortMismatch"
	ReasonHostNotOnGateway        = "hostNotOnGateway"
	ReasonVSNamespaceNotAllowed   = "vsNamespaceNotAllowed"
	ReasonRouteNotAttached        = "routeNotAttached"
	ReasonNoRouteMatch            = "noRouteMatch"
	// ReasonPathNotRouted (D-040): the host has route configuration on
	// the entry point, but the requested path matches none of its rules
	// (404 from the gateway). A warning, not a failure.
	ReasonPathNotRouted         = "pathNotRouted"
	ReasonConditionalMatch      = "conditionalMatch"
	ReasonHostConflict          = "hostConflict"
	ReasonBackendServiceMissing = checker.ReasonBackendServiceMissing
	ReasonBackendPortMissing    = checker.ReasonBackendPortMissing
	ReasonNoReadyEndpoints      = checker.ReasonNoReadyEndpoints
	ReasonTargetPortNotDeclared = checker.ReasonTargetPortNotDeclared
	ReasonNamedPortUnresolved   = checker.ReasonNamedPortUnresolved
	ReasonProviderInactive      = "providerInactive"
	ReasonDestinationRuleSubset = "destinationRuleSubset"
	ReasonAmbiguous             = "ambiguous"
	ReasonMultipleEntrypoints   = "multipleEntrypoints"
)

// Providers.
const (
	ProviderIstio   = "istio"
	ProviderIngress = "ingress"
)

// Request is the normalized trace query.
type Request struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	Port   int32  `json:"port"`
	Scheme string `json:"scheme"`
}

// Step is one hop finding: the checker step shape (name → detail → hint)
// plus status, hop, graph ref and reason code (D-038).
type Step struct {
	Name   string `json:"name"`
	Hop    string `json:"hop"`
	Status string `json:"status"`
	OK     bool   `json:"ok"`
	Ref    string `json:"ref,omitempty"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
	// Group labels the sub-path of a weighted destination ("backend 1/2 ·
	// weight 80 → svc:port"); empty on the shared part of the branch.
	Group string `json:"group,omitempty"`
	// Items is a long list moved out of Detail (e.g. a gateway's server
	// hosts); UIs show it on demand.
	Items []string `json:"items,omitempty"`
}

// EdgeRef is a directed edge between two node IDs.
type EdgeRef struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Highlight lists the mapper node/edge IDs of a branch for the Overview.
type Highlight struct {
	Nodes []string  `json:"nodes"`
	Edges []EdgeRef `json:"edges"`
}

// Branch is the path through one entry point that claims the host.
type Branch struct {
	Provider  string    `json:"provider"`
	Entry     string    `json:"entry"`
	State     string    `json:"state"`
	Steps     []Step    `json:"steps"`
	Highlight Highlight `json:"highlight"`
}

// Response is the trace answer.
type Response struct {
	Request      Request `json:"request"`
	GraphVersion uint64  `json:"graphVersion"`
	State        string  `json:"state"`
	// Steps are request-level findings (scope note, inactive providers,
	// hostNotClaimed, multipleEntrypoints).
	Steps    []Step           `json:"steps"`
	Branches []Branch         `json:"branches"`
	Topology checker.Topology `json:"topology"`
}
