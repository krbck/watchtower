// Package controller contains the Watchtower reconcilers.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
	"github.com/krbck/watchtower/internal/checker"
)

// requeuePeriod is the interval for periodic re-checks (even when the spec
// does not change).
const requeuePeriod = 5 * time.Minute

// ConnectivityCheckReconciler watches ConnectivityCheck CRs, runs the chain
// and writes the outcome into the status subresource.
type ConnectivityCheckReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Kube is the read surface for the chain engine; when nil a client-go
	// clientset is derived from the manager configuration.
	Kube checker.Kube
}

// +kubebuilder:rbac:groups=watchtower.krbck.io,resources=connectivitychecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=watchtower.krbck.io,resources=connectivitychecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile runs the connectivity chain for a CR and updates its status.
func (r *ConnectivityCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("connectivitycheck", req.NamespacedName)

	var check watchtowerv1alpha1.ConnectivityCheck
	if err := r.Get(ctx, req.NamespacedName, &check); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res := checker.Run(ctx, r.Kube, check.Namespace, checker.Input{
		Target:      check.Spec.Target,
		ServiceName: check.Spec.ServiceName,
		Port:        check.Spec.Port,
	})

	state := "Failed"
	if res.Passed {
		state = "Passed"
	}

	logger.Info("connectivity chain finished",
		"state", state, "steps", len(res.Steps), "nodes", len(res.Topology.Nodes))

	steps := make([]watchtowerv1alpha1.CheckStep, 0, len(res.Steps))
	for _, s := range res.Steps {
		steps = append(steps, watchtowerv1alpha1.CheckStep{
			Name:   s.Name,
			OK:     s.OK,
			Detail: s.Detail,
			Hint:   s.Hint,
		})
	}

	topology := watchtowerv1alpha1.Topology{
		Nodes: make([]watchtowerv1alpha1.TopologyNode, 0, len(res.Topology.Nodes)),
		Edges: make([]watchtowerv1alpha1.TopologyEdge, 0, len(res.Topology.Edges)),
	}
	for _, n := range res.Topology.Nodes {
		node := watchtowerv1alpha1.TopologyNode{
			ID:        n.ID,
			Kind:      n.Kind,
			Label:     n.Label,
			Namespace: n.Namespace,
			OK:        n.OK,
			Detail:    n.Detail,
			Meta:      n.Meta,
		}
		for _, pi := range n.Ports {
			node.Ports = append(node.Ports, watchtowerv1alpha1.PortInfo{
				Port:       pi.Port,
				TargetPort: pi.TargetPort,
				NodePort:   pi.NodePort,
				Protocol:   pi.Protocol,
				Matched:    pi.Matched,
				Reason:     pi.Reason,
				Hint:       pi.Hint,
			})
		}
		for _, w := range n.Warnings {
			node.Warnings = append(node.Warnings, watchtowerv1alpha1.NodeWarning(w))
		}
		topology.Nodes = append(topology.Nodes, node)
	}
	for _, e := range res.Topology.Edges {
		// Field-wise: checker edges carry graph-only fields (Config) that
		// the CRD status does not store.
		topology.Edges = append(topology.Edges, watchtowerv1alpha1.TopologyEdge{
			From: e.From, To: e.To, Label: e.Label, OK: e.OK,
		})
	}

	now := metav1.Now()
	prevState := check.Status.State
	newStatus := watchtowerv1alpha1.ConnectivityCheckStatus{
		State:              state,
		Steps:              steps,
		Topology:           topology,
		ObservedGeneration: check.Generation,
		LastCheckedAt:      &now,
	}

	// Compare the full status (ignoring the timestamp) so newly added fields
	// — e.g. structured ports — are written even when the state is unchanged.
	if !statusEqual(&check.Status, &newStatus) {
		// Emit a kubernetes event only on state transitions (visible in
		// kubectl describe; avoids spam on periodic requeues).
		if r.Recorder != nil && prevState != "" && prevState != state {
			eventType := corev1.EventTypeNormal
			if state == "Failed" {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(&check, eventType, "ConnectivityCheck"+state,
				"chain result changed: %s → %s (%d steps)", prevState, state, len(steps))
		}

		check.Status = newStatus
		if err := r.Status().Update(ctx, &check); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: requeuePeriod}, nil
}

// statusEqual compares two statuses while ignoring LastCheckedAt.
func statusEqual(a, b *watchtowerv1alpha1.ConnectivityCheckStatus) bool {
	ac := a.DeepCopy()
	bc := b.DeepCopy()
	ac.LastCheckedAt = nil
	bc.LastCheckedAt = nil
	ab, errA := json.Marshal(ac)
	bb, errB := json.Marshal(bc)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// SetupWithManager registers the controller with the manager.
func (r *ConnectivityCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("watchtower-controller")
	}
	if r.Kube == nil {
		kube, err := checker.KubeFromConfig(mgr.GetConfig())
		if err != nil {
			return err
		}
		r.Kube = kube
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&watchtowerv1alpha1.ConnectivityCheck{}).
		Complete(r)
}
