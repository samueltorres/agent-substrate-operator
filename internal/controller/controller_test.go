package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/pki"
	"github.com/samueltorres/agent-substrate-operator/internal/render"
)

var testClient client.Client
var testClientset kubernetes.Interface

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// `make test` provides the envtest binaries.
		os.Exit(m.Run())
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "agent-substrate-operator", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	api := env.ControlPlane.GetAPIServer().Configure()
	api.Set("feature-gates", "ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true")
	api.Set("runtime-config", "certificates.k8s.io/v1beta1=true")
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	if testClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	testClientset = kubernetes.NewForConfigOrDie(cfg)
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if testClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make test`")
	}
}

type fakeExec struct{ calls []string }

func (f *fakeExec) Exec(_ context.Context, namespace, pod, container string, command []string, stdin string) error {
	if !strings.Contains(stdin, "BEGIN;") {
		panic("setup script is not a transaction")
	}
	f.calls = append(f.calls, namespace+"/"+pod+"/"+container+" "+strings.Join(command, " "))
	return nil
}

func validSpec() v1alpha1.SubstrateInstallSpec {
	return v1alpha1.SubstrateInstallSpec{
		Platform: v1alpha1.PlatformGeneric,
		Images:   v1alpha1.ImagesSpec{Repository: "registry.example.com/substrate"},
		Storage: v1alpha1.StorageSpec{
			Backend: v1alpha1.StorageBackendS3,
			S3:      &v1alpha1.S3Spec{Region: "us-east-1"},
		},
		Atenet:         v1alpha1.AtenetSpec{CredentialProvider: v1alpha1.CredentialProviderSpec{Name: "k8s.io"}},
		DeletionPolicy: v1alpha1.DeletionPolicyDelete,
	}
}

func TestValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	off := false
	for _, tc := range []struct {
		name   string
		obj    string
		mutate func(*v1alpha1.SubstrateInstallSpec)
		want   string
	}{
		{name: "wrong name", obj: "other", want: "must be named substrate"},
		{name: "kind with storage", mutate: func(s *v1alpha1.SubstrateInstallSpec) { s.Platform = v1alpha1.PlatformKind }, want: "bundled rustfs"},
		{name: "provider without name", mutate: func(s *v1alpha1.SubstrateInstallSpec) {
			s.Atenet.CredentialProvider = v1alpha1.CredentialProviderSpec{}
		}, want: "set name"},
		{name: "disabled provider with name", mutate: func(s *v1alpha1.SubstrateInstallSpec) {
			s.Atenet.CredentialProvider.Enabled = &off
		}, want: "set name"},
		{name: "own provider without address", mutate: func(s *v1alpha1.SubstrateInstallSpec) {
			s.Atenet.CredentialProvider.Name = "vault"
		}, want: "needs an address"},
		{name: "bundled postgres with schema", mutate: func(s *v1alpha1.SubstrateInstallSpec) {
			s.Postgres.Schema = "mine"
		}, want: "fixed roles and schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.obj
			if name == "" {
				name = v1alpha1.SingletonName
			}
			spec := validSpec()
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			obj := &v1alpha1.SubstrateInstall{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
			err := testClient.Create(ctx, obj, client.DryRunAll)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Create error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// The operator writes the whole object back when it adds its finalizer, so
// sections a spec leaves out must stay out of the JSON it sends.
func TestUnsetSectionsStayUnset(t *testing.T) {
	requireEnvtest(t)
	spec := validSpec()
	spec.Platform = v1alpha1.PlatformKind
	spec.Storage = v1alpha1.StorageSpec{}
	obj := &v1alpha1.SubstrateInstall{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}, Spec: spec}
	if err := testClient.Create(context.Background(), obj, client.DryRunAll); err != nil {
		t.Errorf("Create of a kind install without storage: %v", err)
	}
}

// markReady reports every installed workload in namespace as rolled out, as
// the controllers and kubelet a real cluster runs would.
func markReady(t *testing.T, ctx context.Context) {
	t.Helper()
	deps := &appsv1.DeploymentList{}
	if err := testClient.List(ctx, deps, client.HasLabels{render.LabelInstall}); err != nil {
		t.Fatal(err)
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		n := int32(1)
		if d.Spec.Replicas != nil {
			n = *d.Spec.Replicas
		}
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n}
		if err := testClient.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	dss := &appsv1.DaemonSetList{}
	if err := testClient.List(ctx, dss, client.HasLabels{render.LabelInstall}); err != nil {
		t.Fatal(err)
	}
	for i := range dss.Items {
		d := &dss.Items[i]
		d.Status = appsv1.DaemonSetStatus{ObservedGeneration: d.Generation, DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}
		if err := testClient.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	stss := &appsv1.StatefulSetList{}
	if err := testClient.List(ctx, stss, client.HasLabels{render.LabelInstall}); err != nil {
		t.Fatal(err)
	}
	for i := range stss.Items {
		s := &stss.Items[i]
		s.Status = appsv1.StatefulSetStatus{ObservedGeneration: s.Generation, Replicas: 1, ReadyReplicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r1"}
		if err := testClient.Status().Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func publishTrustBundles(t *testing.T, ctx context.Context) {
	t.Helper()
	ca, err := pki.GenerateCA("1", pki.KeyTypeECDSAP256, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range podCertTrustBundles {
		signer := strings.Join(strings.SplitN(name, ":", 3)[:2], "/")
		b := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "certificates.k8s.io/v1beta1",
			"kind":       "ClusterTrustBundle",
			"metadata":   map[string]any{"name": name},
			"spec":       map[string]any{"signerName": signer, "trustBundle": string(ca.TLSCertificateChainPEM())},
		}}
		if err := testClient.Create(ctx, b); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

func reconcileOnce(t *testing.T, ctx context.Context, r *Reconciler) *v1alpha1.SubstrateInstall {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.SingletonName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &v1alpha1.SubstrateInstall{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, got); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return got
}

func exists(t *testing.T, ctx context.Context, obj client.Object, namespace, name string) bool {
	t.Helper()
	err := testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetDeletionTimestamp() == nil
}

func TestInstallLifecycle(t *testing.T) {
	requireEnvtest(t)
	ctx := ctrl.LoggerInto(context.Background(), ctrl.Log)
	exec := &fakeExec{}
	r := &Reconciler{Client: testClient, APIReader: testClient, Clientset: testClientset, Exec: exec}

	install := &v1alpha1.SubstrateInstall{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}, Spec: validSpec()}
	if err := testClient.Create(ctx, install); err != nil {
		t.Fatal(err)
	}

	// Until the podcertificate controller runs, the install waits on it,
	// with the namespace, CRDs, and every pool in place.
	var got *v1alpha1.SubstrateInstall
	for range 5 {
		if got = reconcileOnce(t, ctx, r); got.Status.Step == "PodCertificate" {
			break
		}
	}
	if got.Status.Step != "PodCertificate" || !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionProgressing) {
		t.Fatalf("status = %+v, want waiting on PodCertificate", got.Status)
	}
	for _, s := range []struct{ ns, name string }{
		{render.Namespace, secretActorIDJWTPool}, {render.Namespace, secretActorIDCAPool},
		{render.Namespace, secretActorIDCACerts}, {render.Namespace, secretEgressMITMCAPool},
		{render.PodCertNamespace, secretServiceDNSCA}, {render.PodCertNamespace, secretPodIdentityCA},
		{render.PodCertNamespace, secretPostgresCA},
	} {
		if !exists(t, ctx, &corev1.Secret{}, s.ns, s.name) {
			t.Errorf("secret %s/%s not created", s.ns, s.name)
		}
	}
	jwtPool := &corev1.Secret{}
	exists(t, ctx, jwtPool, render.Namespace, secretActorIDJWTPool)

	// Once the controller is up and has published its bundles, the install
	// moves on to the bundled PostgreSQL.
	markReady(t, ctx)
	publishTrustBundles(t, ctx)
	if got = reconcileOnce(t, ctx, r); got.Status.Step != "Postgres" {
		t.Fatalf("step = %q (%v), want Postgres", got.Status.Step, got.Status.Conditions)
	}
	if !exists(t, ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "ate.dev/v1alpha1", "kind": "SandboxConfig"}}, "", "gvisor-default") {
		t.Error("default SandboxConfig not applied")
	}

	// With PostgreSQL ready, the identities are set up once and the control
	// plane is applied.
	markReady(t, ctx)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: render.Namespace, Name: postgresPod},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: postgresContainer, Image: "postgres"}}},
	}
	if err := testClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if got = reconcileOnce(t, ctx, r); got.Status.Step != "Rollout" {
		t.Fatalf("step = %q (%v), want Rollout", got.Status.Step, got.Status.Conditions)
	}
	if len(exec.calls) != 1 || !strings.HasPrefix(exec.calls[0], "ate-system/postgres-0/postgres psql") {
		t.Errorf("postgres setup calls = %v", exec.calls)
	}
	if got.Status.PostgresSetupPodUID != string(pod.UID) {
		t.Error("postgres setup not recorded against the pod")
	}

	markReady(t, ctx)
	if got = reconcileOnce(t, ctx, r); !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatalf("status = %+v, want Ready", got.Status)
	}
	if len(exec.calls) != 1 {
		t.Errorf("postgres setup ran %d times for one pod", len(exec.calls))
	}
	again := &corev1.Secret{}
	exists(t, ctx, again, render.Namespace, secretActorIDJWTPool)
	if string(again.Data["pool"]) != string(jwtPool.Data["pool"]) {
		t.Error("JWT pool regenerated on a later reconcile")
	}

	// Turning injection off prunes the bundled provider and leaves its
	// namespace policy, which holds the administrator's allow-list.
	got.Spec.Atenet.CredentialProvider = v1alpha1.CredentialProviderSpec{Enabled: new(false)}
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, r)
	if exists(t, ctx, &appsv1.Deployment{}, render.Namespace, "k8s-credential-provider") {
		t.Error("bundled credential provider not pruned")
	}
	if !exists(t, ctx, &corev1.ConfigMap{}, render.Namespace, "k8s-credential-provider-namespace-policy") {
		t.Error("namespace policy pruned")
	}

	// Deleting with the Delete policy tears down what the operator applied.
	// envtest runs no namespace controller, so the namespaces stay
	// Terminating and the finalizer holds.
	if err := testClient.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = reconcileOnce(t, ctx, r)
	if got == nil || got.Status.Step != "Teardown" {
		t.Fatalf("got %+v, want waiting in Teardown", got)
	}
	if exists(t, ctx, &appsv1.Deployment{}, render.Namespace, "ate-api-server") {
		t.Error("ate-api-server not deleted")
	}
	if exists(t, ctx, &corev1.Namespace{}, "", render.Namespace) {
		t.Error("ate-system not deleted")
	}

	// Later passes must keep going once Substrate's CRDs are gone, though
	// the RESTMapper still knows their kinds.
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	deadline := time.Now().Add(30 * time.Second)
	for !apierrors.IsNotFound(testClient.Get(ctx, client.ObjectKey{Name: "sandboxconfigs.ate.dev"}, crd)) {
		if time.Now().After(deadline) {
			t.Fatal("sandboxconfigs CRD not deleted")
		}
		time.Sleep(200 * time.Millisecond)
	}
	reconcileOnce(t, ctx, r)
}

func TestActorJWTIssuerValidation(t *testing.T) {
	requireEnvtest(t)
	for _, issuer := range []string{"http://idp.example.com", "https://idp.example.com?x=1", "idp.example.com"} {
		spec := validSpec()
		spec.ActorJWT.Issuer = issuer
		obj := &v1alpha1.SubstrateInstall{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}, Spec: spec}
		if err := testClient.Create(context.Background(), obj, client.DryRunAll); err == nil {
			t.Errorf("issuer %q accepted", issuer)
		}
	}
}
