// Package controller reconciles the SubstrateInstall into the cluster.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/render"
	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

// finalizer holds the SubstrateInstall until teardown has finished.
const finalizer = "substrate.samueltorres.github.io/teardown"

const (
	// waitInterval is how soon to look again while a step waits on the
	// cluster.
	waitInterval = 5 * time.Second
	// resyncInterval re-applies a ready install, undoing drift in objects
	// the operator does not watch.
	resyncInterval = 10 * time.Minute
)

// Reconciler installs, upgrades, and tears down Agent Substrate.
type Reconciler struct {
	// Client reads from the cache, which holds the operator's own objects
	// and the labeled workloads.
	Client client.Client
	// APIReader reads uncached, for Secrets and everything else the cache
	// does not hold.
	APIReader client.Reader
	Clientset kubernetes.Interface
	Exec      PodExecutor
}

// +kubebuilder:rbac:groups=substrate.samueltorres.github.io,resources=substrateinstalls,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=substrate.samueltorres.github.io,resources=substrateinstalls/status;substrateinstalls/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces;configmaps;secrets;services;serviceaccounts;persistentvolumeclaims,verbs=*
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=get;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;statefulsets,verbs=*
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=*
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=*
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=*
// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=*
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=*
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings,verbs=*
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=*
// +kubebuilder:rbac:groups=ate.dev,resources=sandboxconfigs;workerpools;csidriverconfigs,verbs=*
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=clustertrustbundles,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=monitoring.googleapis.com,resources=podmonitorings,verbs=*
// +kubebuilder:rbac:urls=/.well-known/openid-configuration,verbs=get

// Reconcile drives the install one step at a time. A step that has to wait
// on the cluster requeues rather than blocking.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != v1alpha1.SingletonName {
		return ctrl.Result{}, nil
	}
	install := &v1alpha1.SubstrateInstall{}
	if err := r.Client.Get(ctx, req.NamespacedName, install); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !install.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, install)
	}
	if controllerutil.AddFinalizer(install, finalizer) {
		if err := r.Client.Update(ctx, install); err != nil {
			return ctrl.Result{}, err
		}
	}

	base := install.DeepCopy()
	step, wait, err := r.install(ctx, install)
	return r.report(ctx, base, install, step, wait, err)
}

// errInvalid marks an error that retrying cannot fix: the spec has to change.
var errInvalid = errors.New("invalid spec")

// install runs the install steps in order and returns the step it stopped at,
// what that step waits on, or why it failed.
func (r *Reconciler) install(ctx context.Context, install *v1alpha1.SubstrateInstall) (string, string, error) {
	spec := render.WithDefaults(install.Spec)
	in, err := r.inputs(ctx, spec)
	if err != nil {
		return "Render", "", err
	}
	bundle, err := render.Render(in)
	if err != nil {
		return "Render", "", fmt.Errorf("%w: %w", errInvalid, err)
	}

	steps := []struct {
		name string
		run  func() (string, error)
	}{
		{"Namespace", func() (string, error) { return "", r.apply(ctx, bundle.Namespace) }},
		{"CRDs", func() (string, error) {
			if err := r.apply(ctx, bundle.CRDs); err != nil {
				return "", err
			}
			return r.crdsNotEstablished(ctx, bundle.CRDs)
		}},
		{"Secrets", func() (string, error) {
			return "", r.ensureControlPlaneSecrets(ctx, spec.ActorJWT.Algorithm)
		}},
		{"PodCertificate", func() (string, error) { return r.podCertificate(ctx, bundle.PodCertificate) }},
		{"SandboxConfig", func() (string, error) { return "", r.apply(ctx, bundle.SandboxConfig) }},
		{"Postgres", func() (string, error) { return r.postgres(ctx, install, bundle.Postgres) }},
		{"ControlPlane", func() (string, error) {
			if err := r.apply(ctx, bundle.System); err != nil {
				return "", err
			}
			return "", r.createIfMissing(ctx, bundle.CreateOnly)
		}},
		{"Prune", func() (string, error) { return "", r.prune(ctx, bundle.All()) }},
		{"Rollout", func() (string, error) { return r.notReady(ctx, bundle.System) }},
	}
	for _, s := range steps {
		wait, err := s.run()
		if err != nil || wait != "" {
			return s.name, wait, err
		}
	}
	return "Ready", "", nil
}

// inputs gathers what the render reads from the cluster.
func (r *Reconciler) inputs(ctx context.Context, spec v1alpha1.SubstrateInstallSpec) (render.Inputs, error) {
	in := render.Inputs{Spec: spec}
	issuer, err := r.issuer(ctx, spec)
	if err != nil {
		return in, err
	}
	in.Issuer = issuer
	if name := spec.Postgres.ConnectionSecretName; name != "" {
		s := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: render.Namespace, Name: name}, s); err != nil {
			return in, fmt.Errorf("while reading the PostgreSQL connection secret: %w", err)
		}
		in.ExternalPostgres = s.Data
	}
	return in, nil
}

// issuer determines the service account token issuer ate-api-server trusts:
// the spec's, else the GKE cluster's, else the one the cluster's OpenID
// discovery document names, else the in-cluster default.
func (r *Reconciler) issuer(ctx context.Context, spec v1alpha1.SubstrateInstallSpec) (string, error) {
	if spec.APIServer.ExpectedJWTIssuer != "" {
		return spec.APIServer.ExpectedJWTIssuer, nil
	}
	if spec.GCP != nil {
		return render.GKEIssuer(spec.GCP), nil
	}
	raw, err := r.Clientset.Discovery().RESTClient().Get().AbsPath("/.well-known/openid-configuration").DoRaw(ctx)
	if apierrors.IsNotFound(err) {
		return render.InClusterIssuer, nil
	}
	// Any other failure is retried rather than falling back: a changed issuer
	// rewrites ate-api-server's authentication config and restarts it.
	if err != nil {
		return "", fmt.Errorf("while reading the cluster's OpenID discovery document: %w", err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("while parsing the cluster's OpenID discovery document: %w", err)
	}
	if doc.Issuer == "" {
		return render.InClusterIssuer, nil
	}
	return doc.Issuer, nil
}

// podCertificate creates the signer pools, applies the controller, and waits
// until it has rolled out and published its trust bundles.
func (r *Reconciler) podCertificate(ctx context.Context, objs []*render.Object) (string, error) {
	// The pools live in the controller's namespace, which the manifest
	// creates.
	var ns []*render.Object
	for _, obj := range objs {
		if obj.GetKind() == "Namespace" {
			ns = append(ns, obj)
		}
	}
	if err := r.apply(ctx, ns); err != nil {
		return "", err
	}
	if err := r.ensurePodCertificateCAs(ctx); err != nil {
		return "", err
	}
	if err := r.apply(ctx, objs); err != nil {
		return "", err
	}
	if wait, err := r.notReady(ctx, objs); err != nil || wait != "" {
		return wait, err
	}
	return r.missingTrustBundle(ctx)
}

// postgres applies the bundled PostgreSQL and, once its pod is ready, creates
// Substrate's roles and schema in it. The setup runs again whenever the pod is
// replaced; the script is idempotent.
func (r *Reconciler) postgres(ctx context.Context, install *v1alpha1.SubstrateInstall, objs []*render.Object) (string, error) {
	if len(objs) == 0 {
		return "", nil
	}
	if err := r.apply(ctx, objs); err != nil {
		return "", err
	}
	if wait, err := r.notReady(ctx, objs); err != nil || wait != "" {
		return wait, err
	}
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: render.Namespace, Name: postgresPod}, pod); err != nil {
		return "", err
	}
	if install.Status.PostgresSetupPodUID == string(pod.UID) {
		return "", nil
	}
	command, script := postgresSetupCommand()
	if err := r.Exec.Exec(ctx, render.Namespace, postgresPod, postgresContainer, command, script); err != nil {
		return "", fmt.Errorf("while setting up the bundled PostgreSQL identities: %w", err)
	}
	install.Status.PostgresSetupPodUID = string(pod.UID)
	return "", nil
}

// report writes the outcome of a reconcile to the status and decides when to
// look again.
func (r *Reconciler) report(ctx context.Context, base, install *v1alpha1.SubstrateInstall, step, wait string, stepErr error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	status := &install.Status
	status.ObservedGeneration = install.Generation
	status.Version = upstream.Version
	status.Step = step

	set := func(condition string, s metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: condition, Status: s, Reason: reason, Message: message, ObservedGeneration: install.Generation,
		})
	}
	var result ctrl.Result
	switch {
	case stepErr != nil:
		log.Error(stepErr, "install step failed", "step", step)
		set(v1alpha1.ConditionReady, metav1.ConditionFalse, step+"Failed", stepErr.Error())
		set(v1alpha1.ConditionProgressing, metav1.ConditionFalse, step+"Failed", "")
		set(v1alpha1.ConditionDegraded, metav1.ConditionTrue, step+"Failed", stepErr.Error())
	case wait != "":
		set(v1alpha1.ConditionReady, metav1.ConditionFalse, "Waiting", wait)
		set(v1alpha1.ConditionProgressing, metav1.ConditionTrue, step, wait)
		set(v1alpha1.ConditionDegraded, metav1.ConditionFalse, step, "")
		result.RequeueAfter = waitInterval
	default:
		set(v1alpha1.ConditionReady, metav1.ConditionTrue, "Installed", "Agent Substrate "+upstream.Version+" is installed")
		set(v1alpha1.ConditionProgressing, metav1.ConditionFalse, "Installed", "")
		set(v1alpha1.ConditionDegraded, metav1.ConditionFalse, "Installed", "")
		result.RequeueAfter = resyncInterval
	}
	// A patch, not an update: the cache may still hold the object from
	// before the last status write, and an update from it would conflict.
	if err := r.Client.Status().Patch(ctx, install, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	if stepErr != nil && !errors.Is(stepErr, errInvalid) {
		// Back off through the controller's rate limiter.
		return ctrl.Result{}, stepErr
	}
	return result, nil
}

// SetupWithManager registers the controller. Workloads the operator installed
// requeue the install when their spec changes or they are deleted, so drift
// is undone promptly; rollout progress is polled.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	toInstall := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: v1alpha1.SingletonName}}}
	})
	installed := builder.WithPredicates(predicate.GenerationChangedPredicate{}, predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[render.LabelInstall] != ""
	}))
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SubstrateInstall{}).
		Watches(&appsv1.Deployment{}, toInstall, installed).
		Watches(&appsv1.DaemonSet{}, toInstall, installed).
		Watches(&appsv1.StatefulSet{}, toInstall, installed).
		Named("substrateinstall").
		Complete(r)
}
