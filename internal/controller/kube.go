package controller

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samueltorres/agent-substrate-operator/internal/render"
)

// fieldOwner is the server-side apply field manager.
const fieldOwner = "agent-substrate-operator"

// optionalKinds are applied only where their CRD is served. PodMonitoring is
// Google Managed Prometheus's, which only GKE has.
var optionalKinds = []schema.GroupKind{
	{Group: "monitoring.googleapis.com", Kind: "PodMonitoring"},
}

// applyOrder ranks kinds so that what an object refers to is applied first.
var applyOrder = []string{
	"Namespace", "CustomResourceDefinition", "PriorityClass", "ServiceAccount",
	"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding",
	"ConfigMap", "Secret", "PersistentVolumeClaim", "Service",
}

func applyRank(kind string) int {
	if i := slices.Index(applyOrder, kind); i >= 0 {
		return i
	}
	return len(applyOrder)
}

// apply server-side applies objects in dependency order, skipping optional
// kinds the cluster does not serve.
func (r *Reconciler) apply(ctx context.Context, objs []*render.Object) error {
	sorted := slices.Clone(objs)
	slices.SortStableFunc(sorted, func(a, b *render.Object) int {
		return applyRank(a.GetKind()) - applyRank(b.GetKind())
	})
	for _, obj := range sorted {
		served, err := r.served(obj.GroupVersionKind())
		if err != nil {
			return err
		}
		if !served {
			continue
		}
		if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj.DeepCopy()),
			client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("while applying %s: %w", describe(obj), err)
		}
	}
	return nil
}

// served reports whether the cluster serves gvk. Only an optional kind may be
// missing; any other unserved kind is an error.
func (r *Reconciler) served(gvk schema.GroupVersionKind) (bool, error) {
	_, err := r.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return true, nil
	}
	if meta.IsNoMatchError(err) && slices.Contains(optionalKinds, gvk.GroupKind()) {
		return false, nil
	}
	return false, fmt.Errorf("while resolving %s: %w", gvk, err)
}

// createIfMissing creates objects that do not exist and leaves existing ones
// alone.
func (r *Reconciler) createIfMissing(ctx context.Context, objs []*render.Object) error {
	for _, obj := range objs {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Client.Create(ctx, obj.DeepCopy()); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("while creating %s: %w", describe(obj), err)
		}
	}
	return nil
}

func describe(obj client.Object) string {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if ns := obj.GetNamespace(); ns != "" {
		return fmt.Sprintf("%s %s/%s", kind, ns, obj.GetName())
	}
	return fmt.Sprintf("%s %s", kind, obj.GetName())
}

// notReady returns why the first workload in objs that has not rolled out is
// waiting, or "" when all have.
func (r *Reconciler) notReady(ctx context.Context, objs []*render.Object) (string, error) {
	for _, obj := range objs {
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
		var workload client.Object
		var wait func() string
		switch obj.GetKind() {
		case "Deployment":
			d := &appsv1.Deployment{}
			workload, wait = d, func() string { return deploymentWait(d) }
		case "DaemonSet":
			d := &appsv1.DaemonSet{}
			workload, wait = d, func() string { return daemonSetWait(d) }
		case "StatefulSet":
			s := &appsv1.StatefulSet{}
			workload, wait = s, func() string { return statefulSetWait(s) }
		default:
			continue
		}
		why := ""
		// The cache can lag the apply that just created the workload.
		if err := r.Client.Get(ctx, key, workload); apierrors.IsNotFound(err) {
			why = "not in the cache yet"
		} else if err != nil {
			return "", err
		} else {
			why = wait()
		}
		if why != "" {
			return fmt.Sprintf("%s %s: %s", obj.GetKind(), key, why), nil
		}
	}
	return "", nil
}

func deploymentWait(d *appsv1.Deployment) string {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	switch {
	case d.Status.ObservedGeneration < d.Generation:
		return "rollout not observed yet"
	case d.Status.UpdatedReplicas < want:
		return fmt.Sprintf("%d of %d replicas updated", d.Status.UpdatedReplicas, want)
	case d.Status.Replicas > d.Status.UpdatedReplicas:
		return fmt.Sprintf("%d old replicas pending termination", d.Status.Replicas-d.Status.UpdatedReplicas)
	case d.Status.AvailableReplicas < d.Status.UpdatedReplicas:
		return fmt.Sprintf("%d of %d updated replicas available", d.Status.AvailableReplicas, d.Status.UpdatedReplicas)
	}
	return ""
}

func daemonSetWait(d *appsv1.DaemonSet) string {
	switch {
	case d.Status.ObservedGeneration < d.Generation:
		return "rollout not observed yet"
	case d.Status.UpdatedNumberScheduled < d.Status.DesiredNumberScheduled:
		return fmt.Sprintf("%d of %d pods updated", d.Status.UpdatedNumberScheduled, d.Status.DesiredNumberScheduled)
	case d.Status.NumberAvailable < d.Status.DesiredNumberScheduled:
		return fmt.Sprintf("%d of %d pods available", d.Status.NumberAvailable, d.Status.DesiredNumberScheduled)
	}
	return ""
}

func statefulSetWait(s *appsv1.StatefulSet) string {
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	switch {
	case s.Status.ObservedGeneration < s.Generation:
		return "rollout not observed yet"
	case s.Status.UpdateRevision != "" && s.Status.CurrentRevision != s.Status.UpdateRevision:
		return "pods not on the current revision"
	case s.Status.ReadyReplicas < want:
		return fmt.Sprintf("%d of %d replicas ready", s.Status.ReadyReplicas, want)
	}
	return ""
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// crdsNotEstablished returns the first CRD in objs that is not yet served.
func (r *Reconciler) crdsNotEstablished(ctx context.Context, objs []*render.Object) (string, error) {
	for _, obj := range objs {
		if obj.GroupVersionKind() != crdGVK {
			continue
		}
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: obj.GetName()}, crd); err != nil {
			return "", err
		}
		if !conditionTrue(crd, "Established") {
			return "CRD " + obj.GetName() + " not established", nil
		}
	}
	return "", nil
}

func conditionTrue(obj *unstructured.Unstructured, condition string) bool {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == condition && m["status"] == "True" {
			return true
		}
	}
	return false
}

// trustBundleGK is the ClusterTrustBundle kind, served at whichever version
// the cluster enables.
var trustBundleGK = schema.GroupKind{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"}

// trustBundleGVK resolves the served version of ClusterTrustBundle.
func (r *Reconciler) trustBundleGVK() (schema.GroupVersionKind, error) {
	mapping, err := r.Client.RESTMapper().RESTMapping(trustBundleGK)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("the cluster does not serve ClusterTrustBundles, which Agent Substrate needs: %w", err)
	}
	return mapping.GroupVersionKind, nil
}

// podCertTrustBundles are the ClusterTrustBundles the podcertificate
// controller publishes once it runs. Workloads project them, so nothing that
// uses pod certificates starts before they exist.
var podCertTrustBundles = []string{
	"podidentity.podcert.ate.dev:identity:primary-bundle",
	"servicedns.podcert.ate.dev:identity:primary-bundle",
	"postgres.podcert.ate.dev:identity:primary-bundle",
}

// missingTrustBundle returns the first trust bundle not yet published.
func (r *Reconciler) missingTrustBundle(ctx context.Context) (string, error) {
	gvk, err := r.trustBundleGVK()
	if err != nil {
		return "", err
	}
	for _, name := range podCertTrustBundles {
		b := &unstructured.Unstructured{}
		b.SetGroupVersionKind(gvk)
		err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, b)
		if apierrors.IsNotFound(err) {
			return "ClusterTrustBundle " + name + " not published yet", nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}
