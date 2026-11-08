package checker

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CachedKube adapts a controller-runtime client.Reader — normally the
// manager's informer cache — to the Kube interface. The host trace (D-038)
// uses it so an on-demand query never hits the API server: every type it
// reads is already watched by the topology mapper.
type CachedKube struct{ R client.Reader }

var (
	_ Kube           = CachedKube{}
	_ WorkloadLister = CachedKube{}
)

func (k CachedKube) Service(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	var svc corev1.Service
	if err := k.R.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		return nil, err
	}
	return &svc, nil
}

func (k CachedKube) Services(ctx context.Context, namespace string) ([]corev1.Service, error) {
	var l corev1.ServiceList
	if err := k.R.List(ctx, &l, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k CachedKube) Pods(ctx context.Context, namespace string, selector labels.Selector) ([]corev1.Pod, error) {
	var l corev1.PodList
	if err := k.R.List(ctx, &l, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k CachedKube) EndpointSlices(ctx context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error) {
	var l discoveryv1.EndpointSliceList
	if err := k.R.List(ctx, &l, client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k CachedKube) DesiredReplicas(ctx context.Context, namespace, name string) (int32, bool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	var d appsv1.Deployment
	if err := k.R.Get(ctx, key, &d); err == nil {
		return replicasOrDefault(d.Spec.Replicas), true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	var s appsv1.StatefulSet
	if err := k.R.Get(ctx, key, &s); err == nil {
		return replicasOrDefault(s.Spec.Replicas), true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	var ds appsv1.DaemonSet
	if err := k.R.Get(ctx, key, &ds); err == nil {
		return ds.Status.DesiredNumberScheduled, true, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, false, err
	}
	return 0, false, nil
}

// SelectedWorkloads returns the desired replicas of the Deployments,
// StatefulSets and DaemonSets in namespace whose pod template labels are
// selected by selector (WorkloadLister).
func (k CachedKube) SelectedWorkloads(ctx context.Context, namespace string, selector map[string]string) ([]int32, error) {
	if len(selector) == 0 {
		return nil, nil
	}
	sel := labels.SelectorFromSet(selector)
	var out []int32
	var deps appsv1.DeploymentList
	if err := k.R.List(ctx, &deps, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range deps.Items {
		if sel.Matches(labels.Set(deps.Items[i].Spec.Template.Labels)) {
			out = append(out, replicasOrDefault(deps.Items[i].Spec.Replicas))
		}
	}
	var sts appsv1.StatefulSetList
	if err := k.R.List(ctx, &sts, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range sts.Items {
		if sel.Matches(labels.Set(sts.Items[i].Spec.Template.Labels)) {
			out = append(out, replicasOrDefault(sts.Items[i].Spec.Replicas))
		}
	}
	var dss appsv1.DaemonSetList
	if err := k.R.List(ctx, &dss, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range dss.Items {
		if sel.Matches(labels.Set(dss.Items[i].Spec.Template.Labels)) {
			out = append(out, dss.Items[i].Status.DesiredNumberScheduled)
		}
	}
	return out, nil
}
