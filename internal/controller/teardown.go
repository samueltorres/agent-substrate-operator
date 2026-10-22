package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/render"
)

// objectKey identifies an object across kinds.
type objectKey struct {
	gk        schema.GroupKind
	namespace string
	name      string
}

func keyOf(obj client.Object) objectKey {
	return objectKey{obj.GetObjectKind().GroupVersionKind().GroupKind(), obj.GetNamespace(), obj.GetName()}
}

// installed lists every object of a managed kind that carries the install
// label.
func (r *Reconciler) installed(ctx context.Context) ([]*unstructured.Unstructured, error) {
	kinds, err := render.ManagedKinds()
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, gvk := range kinds {
		if _, err := r.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err := r.APIReader.List(ctx, list, client.HasLabels{render.LabelInstall})
		// The RESTMapper keeps a kind for a while after its CRD is deleted,
		// as teardown does, and the list then finds no such resource.
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("while listing %s: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			out = append(out, &list.Items[i])
		}
	}
	return out, nil
}

// prune deletes installed objects the current spec no longer renders, such
// as the bundled credential provider once injection is turned off.
func (r *Reconciler) prune(ctx context.Context, desired []*render.Object) error {
	want := map[objectKey]bool{}
	for _, obj := range desired {
		want[keyOf(obj)] = true
	}
	existing, err := r.installed(ctx)
	if err != nil {
		return err
	}
	for _, obj := range existing {
		if want[keyOf(obj)] {
			continue
		}
		logf.FromContext(ctx).Info("pruning", "object", describe(obj))
		if err := r.Client.Delete(ctx, obj, client.PropagationPolicy("Background")); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("while pruning %s: %w", describe(obj), err)
		}
	}
	return nil
}

// sandboxConfigFinalizer is the WorkerPool protection finalizer atecontroller
// keeps on SandboxConfigs (upstream pkg/api/v1alpha1).
const sandboxConfigFinalizer = "sandboxconfig.ate.dev/workerpool-protection"

// substrateTrustBundles maps each ClusterTrustBundle Substrate publishes to
// its signer: the podcertificate controller's, and atecontroller's egress
// MITM bundle.
var substrateTrustBundles = map[string]string{
	"podidentity.podcert.ate.dev:identity:primary-bundle": "podidentity.podcert.ate.dev/identity",
	"servicedns.podcert.ate.dev:identity:primary-bundle":  "servicedns.podcert.ate.dev/identity",
	"postgres.podcert.ate.dev:identity:primary-bundle":    "postgres.podcert.ate.dev/identity",
	"egress-mitm.ate.dev:mitm:primary-bundle":             "egress-mitm.ate.dev/mitm",
}

// finalize runs on deletion. Under the Retain policy it only lets go of the
// object. Under Delete it tears the install down in upstream's order, one
// step per pass: the installed objects, the namespaces and CRDs, then the
// SandboxConfig finalizers atecontroller can no longer remove, and last the
// cluster-scoped trust bundles, once the controllers that republish them are
// gone.
func (r *Reconciler) finalize(ctx context.Context, install *v1alpha1.SubstrateInstall) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(install, finalizer) {
		return ctrl.Result{}, nil
	}
	if install.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete {
		wait, err := r.teardown(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if wait != "" {
			base := install.DeepCopy()
			install.Status.Step = "Teardown"
			meta.SetStatusCondition(&install.Status.Conditions, metav1.Condition{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "TearingDown", Message: wait,
				ObservedGeneration: install.Generation,
			})
			if err := r.Client.Status().Patch(ctx, install, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: waitInterval}, nil
		}
	}
	controllerutil.RemoveFinalizer(install, finalizer)
	return ctrl.Result{}, r.Client.Update(ctx, install)
}

func (r *Reconciler) teardown(ctx context.Context) (string, error) {
	existing, err := r.installed(ctx)
	if err != nil {
		return "", err
	}
	for _, obj := range existing {
		if err := r.Client.Delete(ctx, obj, client.PropagationPolicy("Background")); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("while deleting %s: %w", describe(obj), err)
		}
	}

	namespaces := []string{render.Namespace, render.PodCertNamespace, "otel-system"}
	for _, name := range namespaces {
		ns := &corev1.Namespace{}
		ns.Name = name
		if err := r.Client.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("while deleting namespace %s: %w", name, err)
		}
	}
	crds := &unstructured.UnstructuredList{}
	crds.SetGroupVersionKind(crdGVK.GroupVersion().WithKind("CustomResourceDefinitionList"))
	if err := r.APIReader.List(ctx, crds, client.HasLabels{render.LabelInstall}); err != nil {
		return "", err
	}
	for i := range crds.Items {
		if err := r.Client.Delete(ctx, &crds.Items[i]); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("while deleting CRD %s: %w", crds.Items[i].GetName(), err)
		}
	}

	if wait, err := r.namespaceGone(ctx, render.Namespace); err != nil || wait != "" {
		return wait, err
	}
	// atecontroller, which removes this finalizer, went with its namespace,
	// and deleting the CRDs deleted every WorkerPool, so no pool uses a
	// config anymore.
	if err := r.releaseSandboxConfigs(ctx); err != nil {
		return "", err
	}
	for _, name := range namespaces[1:] {
		if wait, err := r.namespaceGone(ctx, name); err != nil || wait != "" {
			return wait, err
		}
	}
	if err := r.deleteTrustBundles(ctx); err != nil {
		return "", err
	}
	for i := range crds.Items {
		if wait, err := r.gone(ctx, &crds.Items[i]); err != nil || wait != "" {
			return wait, err
		}
	}
	return "", nil
}

func (r *Reconciler) namespaceGone(ctx context.Context, name string) (string, error) {
	ns := &corev1.Namespace{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "namespace " + name + " still terminating", nil
}

func (r *Reconciler) gone(ctx context.Context, obj *unstructured.Unstructured) (string, error) {
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), got)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return describe(obj) + " still deleting", nil
}

var sandboxConfigGVK = schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "SandboxConfig"}

func (r *Reconciler) releaseSandboxConfigs(ctx context.Context) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(sandboxConfigGVK.GroupVersion().WithKind("SandboxConfigList"))
	err := r.APIReader.List(ctx, list)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("while listing SandboxConfigs: %w", err)
	}
	for i := range list.Items {
		sc := &list.Items[i]
		if !slices.Contains(sc.GetFinalizers(), sandboxConfigFinalizer) {
			continue
		}
		patch := client.MergeFrom(sc.DeepCopy())
		sc.SetFinalizers(slices.DeleteFunc(sc.GetFinalizers(), func(f string) bool { return f == sandboxConfigFinalizer }))
		if err := r.Client.Patch(ctx, sc, patch); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("while releasing SandboxConfig %s: %w", sc.GetName(), err)
		}
	}
	return nil
}

// deleteTrustBundles deletes Substrate's ClusterTrustBundles. A bundle goes
// only when both its name and signer match, since anyone allowed to attest
// for one of these signers may publish under another name.
func (r *Reconciler) deleteTrustBundles(ctx context.Context) error {
	gvk, err := r.trustBundleGVK()
	if err != nil {
		// A cluster that serves no trust bundles has none to delete.
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind("ClusterTrustBundleList"))
	if err := r.APIReader.List(ctx, list); err != nil {
		return fmt.Errorf("while listing ClusterTrustBundles: %w", err)
	}
	for i := range list.Items {
		b := &list.Items[i]
		signer, _, _ := unstructured.NestedString(b.Object, "spec", "signerName")
		if want, ok := substrateTrustBundles[b.GetName()]; !ok || signer != want {
			continue
		}
		uid := b.GetUID()
		if err := r.Client.Delete(ctx, b, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("while deleting ClusterTrustBundle %s: %w", b.GetName(), err)
		}
	}
	return nil
}
