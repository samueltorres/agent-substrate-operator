package render

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

func baseSpec() v1alpha1.SubstrateInstallSpec {
	return v1alpha1.SubstrateInstallSpec{
		Platform: v1alpha1.PlatformGeneric,
		Images:   v1alpha1.ImagesSpec{Repository: "registry.example.com/substrate"},
		Storage: v1alpha1.StorageSpec{
			S3:                        &v1alpha1.S3Spec{Region: "eu-west-1", Endpoint: "https://s3.example.com", UsePathStyle: true, CredentialsSecretName: "s3-creds"},
			ServiceAccountAnnotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/substrate"},
		},
		Atenet: v1alpha1.AtenetSpec{CredentialProvider: v1alpha1.CredentialProviderSpec{Name: "k8s.io"}},
	}
}

func mustRender(t *testing.T, spec v1alpha1.SubstrateInstallSpec, external map[string][]byte) *Bundle {
	t.Helper()
	b, err := Render(Inputs{Spec: WithDefaults(spec), Issuer: InClusterIssuer, ExternalPostgres: external})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return b
}

func mustFind(t *testing.T, objs []*Object, kind, name string) *Object {
	t.Helper()
	obj, err := find(objs, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func container(t *testing.T, obj *Object, field, name string) map[string]any {
	t.Helper()
	list, _, _ := unstructured.NestedSlice(obj.Object, append(append([]string{}, podSpecPath...), field)...)
	for _, item := range list {
		if c := item.(map[string]any); c["name"] == name {
			return c
		}
	}
	t.Fatalf("%s has no %s %s", describe(obj), field, name)
	return nil
}

func env(c map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := c["env"].([]any)
	for _, e := range list {
		m := e.(map[string]any)
		if v, ok := m["value"].(string); ok {
			out[m["name"].(string)] = v
		}
	}
	return out
}

func toYAML(t *testing.T, objs []*Object) string {
	t.Helper()
	var b strings.Builder
	for _, obj := range objs {
		out, err := yaml.Marshal(obj.Object)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(out)
		b.WriteString("---\n")
	}
	return b.String()
}

func secretValue(t *testing.T, obj *Object, key string) string {
	t.Helper()
	enc, _, _ := unstructured.NestedString(obj.Object, "data", key)
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(dec)
}

func TestRenderEveryPlatform(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformGeneric, v1alpha1.PlatformGKE, v1alpha1.PlatformKind} {
		t.Run(string(platform), func(t *testing.T) {
			spec := baseSpec()
			spec.Platform = platform
			if platform != v1alpha1.PlatformGeneric {
				spec.Storage = v1alpha1.StorageSpec{}
			}
			b := mustRender(t, spec, nil)
			all := toYAML(t, b.All())
			if strings.Contains(all, "ko://") || strings.Contains(all, "${ENVOY_DATAPLANE_IMAGE}") {
				t.Error("unresolved image reference left in the render")
			}
			want := "registry.example.com/substrate/ateapi:" + upstream.Version
			if !strings.Contains(all, want) {
				t.Errorf("render does not reference %s", want)
			}
			if !strings.Contains(all, "registry.example.com/substrate/envoy-dataplane:"+upstream.Version) {
				t.Error("envoy dataplane image not resolved")
			}
			for _, obj := range b.All() {
				if obj.GetLabels()[LabelInstall] != "substrate" {
					t.Errorf("%s has no install label", describe(obj))
				}
			}
			for _, name := range []string{"ate-api-server", "ate-controller", "atenet-router", "atenet-egress", "k8s-credential-provider"} {
				mustFind(t, b.System, "Deployment", name)
			}
			mustFind(t, b.System, "DaemonSet", "atelet")
			mustFind(t, b.Postgres, "StatefulSet", "postgres")
			mustFind(t, b.PodCertificate, "Deployment", "podcertificate-controller")
			mustFind(t, b.CRDs, "CustomResourceDefinition", "workerpools.ate.dev")
			mustFind(t, b.SandboxConfig, "SandboxConfig", "gvisor-default")
			mustFind(t, b.CreateOnly, "ConfigMap", "k8s-credential-provider-namespace-policy")
		})
	}
}

func TestRenderStorage(t *testing.T) {
	t.Run("generic s3", func(t *testing.T) {
		b := mustRender(t, baseSpec(), nil)
		for _, u := range objectstoreUsers {
			c := container(t, mustFind(t, b.System, u.kind, u.name), "initContainers", "objectstore-plugin")
			got := env(c)
			if got["ATE_STORAGE_BACKEND"] != "s3" || got["AWS_REGION"] != "eu-west-1" ||
				got["AWS_ENDPOINT_URL"] != "https://s3.example.com" || got["AWS_S3_USE_PATH_STYLE"] != "true" {
				t.Errorf("%s objectstore env = %v", u.name, got)
			}
			if !strings.Contains(toYAML(t, []*Object{mustFind(t, b.System, u.kind, u.name)}), "name: s3-creds") {
				t.Errorf("%s does not read the credentials secret", u.name)
			}
			sa := mustFind(t, b.System, "ServiceAccount", u.name)
			if sa.GetAnnotations()["eks.amazonaws.com/role-arn"] == "" {
				t.Errorf("service account %s lacks the storage annotation", u.name)
			}
		}
		atelet := toYAML(t, []*Object{mustFind(t, b.System, "DaemonSet", "atelet")})
		if strings.Contains(atelet, "image-credential-provider") {
			t.Error("generic atelet still mounts the GKE image credential provider")
		}
	})
	t.Run("gke gcs", func(t *testing.T) {
		spec := baseSpec()
		spec.Platform = v1alpha1.PlatformGKE
		spec.Storage = v1alpha1.StorageSpec{}
		b := mustRender(t, spec, nil)
		c := container(t, mustFind(t, b.System, "DaemonSet", "atelet"), "initContainers", "objectstore-plugin")
		if got := env(c); got["ATE_STORAGE_BACKEND"] != "gcs" || got["AWS_REGION"] != "" {
			t.Errorf("objectstore env = %v", got)
		}
		if !strings.Contains(toYAML(t, []*Object{mustFind(t, b.System, "DaemonSet", "atelet")}), "--image-credential-provider-config") {
			t.Error("gke atelet lost the image credential provider")
		}
	})
	t.Run("kind rustfs", func(t *testing.T) {
		spec := baseSpec()
		spec.Platform = v1alpha1.PlatformKind
		spec.Storage = v1alpha1.StorageSpec{}
		b := mustRender(t, spec, nil)
		mustFind(t, b.System, "Deployment", "rustfs")
		c := container(t, mustFind(t, b.System, "Deployment", "ate-api-server"), "initContainers", "objectstore-plugin")
		if got := env(c); got["AWS_ENDPOINT_URL"] != "http://rustfs.ate-system.svc:9000" {
			t.Errorf("objectstore env = %v", got)
		}
	})
	t.Run("s3 without settings", func(t *testing.T) {
		spec := baseSpec()
		spec.Storage.S3 = nil
		if _, err := Render(Inputs{Spec: WithDefaults(spec)}); err == nil {
			t.Error("Render accepted s3 storage without storage.s3")
		}
	})
}

func TestRenderPostgres(t *testing.T) {
	t.Run("bundled", func(t *testing.T) {
		spec := baseSpec()
		spec.Postgres.StorageClassName = "fast"
		b := mustRender(t, spec, nil)
		env := mustFind(t, b.System, "Secret", secretAPIEnvVars)
		if got := secretValue(t, env, "ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING"); got != bundledPostgresDSN(postgresReadWriteUser) {
			t.Errorf("read-write DSN = %q", got)
		}
		if got := secretValue(t, env, "ATE_API_POSTGRES_SCHEMA"); got != "substrate" {
			t.Errorf("schema = %q", got)
		}
		if !strings.Contains(toYAML(t, b.Postgres), "storageClassName: fast") {
			t.Error("storage class not set on the postgres claim")
		}
	})
	t.Run("external", func(t *testing.T) {
		spec := baseSpec()
		spec.Postgres = v1alpha1.PostgresSpec{ConnectionSecretName: "db", Schema: "ate"}
		b := mustRender(t, spec, map[string][]byte{
			KeyReadWriteConnectionString: []byte("postgres://rw@db/ate"),
			KeyServerCA:                  []byte("PEM"),
		})
		if len(b.Postgres) != 0 {
			t.Error("bundled postgres rendered beside an external database")
		}
		env := mustFind(t, b.System, "Secret", secretAPIEnvVars)
		if got := secretValue(t, env, "ATE_API_POSTGRES_OWNER_CONNECTION_STRING"); got != "postgres://rw@db/ate" {
			t.Errorf("owner DSN = %q, want the read-write one", got)
		}
		if got := secretValue(t, mustFind(t, b.System, "Secret", secretPostgresServerCA), "server-ca.pem"); got != "PEM" {
			t.Errorf("server CA = %q", got)
		}
	})
	t.Run("external without DSN", func(t *testing.T) {
		spec := baseSpec()
		spec.Postgres = v1alpha1.PostgresSpec{ConnectionSecretName: "db"}
		if _, err := Render(Inputs{Spec: WithDefaults(spec)}); err == nil {
			t.Error("Render accepted a connection secret with no DSN")
		}
	})
	t.Run("cloud sql", func(t *testing.T) {
		spec := baseSpec()
		spec.Platform = v1alpha1.PlatformGKE
		spec.Storage = v1alpha1.StorageSpec{}
		spec.Postgres = v1alpha1.PostgresSpec{CloudSQL: &v1alpha1.CloudSQLSpec{
			Instance: "p:r:i", ServiceAccount: "ateapi@p.iam.gserviceaccount.com",
		}}
		b := mustRender(t, spec, nil)
		dep := mustFind(t, b.System, "Deployment", "ate-api-server")
		container(t, dep, "initContainers", "cloud-sql-proxy")
		env := mustFind(t, b.System, "Secret", secretAPIEnvVars)
		if got := secretValue(t, env, "ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING"); !strings.HasPrefix(got, "user=ateapi@p.iam host=127.0.0.1") {
			t.Errorf("DSN = %q", got)
		}
		cm := mustFind(t, b.System, "ConfigMap", configMapAPIEnvVars)
		if v, _, _ := unstructured.NestedString(cm.Object, "data", "CSQL_PROXY_PRIVATE_IP"); v != "true" {
			t.Error("private IP not selected by default")
		}
		sa := mustFind(t, b.System, "ServiceAccount", "ate-api-server")
		if sa.GetAnnotations()[workloadIdentityAnnotation] != "ateapi@p.iam.gserviceaccount.com" {
			t.Error("workload identity annotation missing")
		}
	})
}

func TestRenderSize10AndCordon(t *testing.T) {
	spec := baseSpec()
	spec.ClusterSize = v1alpha1.ClusterSize10
	spec.CordonControlPlane = true
	spec.PodCertificate.WorkersPerSigner = 4
	b := mustRender(t, spec, nil)

	pc := mustFind(t, b.PodCertificate, "Deployment", "podcertificate-controller")
	c := container(t, pc, "containers", "controller")
	if !strings.Contains(toYAML(t, []*Object{pc}), "--kube-api-qps=100") {
		t.Error("size10 podcert flags missing")
	}
	if env(c)["WORKERS_PER_SIGNER"] != "4" {
		t.Error("WORKERS_PER_SIGNER not set")
	}
	pg := container(t, mustFind(t, b.Postgres, "StatefulSet", "postgres"), "containers", "postgres")
	if cpu, _, _ := unstructured.NestedString(pg, "resources", "requests", "cpu"); cpu != size10PostgresCPURequest {
		t.Errorf("postgres cpu request = %q", cpu)
	}
	env := mustFind(t, b.System, "Secret", secretAPIEnvVars)
	if got := secretValue(t, env, "ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING"); !strings.HasSuffix(got, size10PostgresPoolParams) {
		t.Errorf("size10 DSN = %q", got)
	}
	for _, objs := range [][]*Object{b.System, b.PodCertificate} {
		for _, obj := range objs {
			if obj.GetKind() != "Deployment" || obj.GetName() == "rustfs" {
				continue
			}
			sel, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "spec", "nodeSelector", "ate.dev/workloadType")
			if sel != "ate-control-plane" {
				t.Errorf("%s is not pinned to the control plane pool", describe(obj))
			}
		}
	}
	sel, _, _ := unstructured.NestedString(mustFind(t, b.Postgres, "StatefulSet", "postgres").Object, "spec", "template", "spec", "nodeSelector", "ate.dev/workloadType")
	if sel != "ate-postgres" {
		t.Error("postgres is not pinned to its pool")
	}
}

func TestRenderEgress(t *testing.T) {
	t.Run("bundled provider", func(t *testing.T) {
		b := mustRender(t, baseSpec(), nil)
		egress := toYAML(t, []*Object{mustFind(t, b.System, "Deployment", "atenet-egress")})
		if !strings.Contains(egress, "--credential-provider-address="+k8sCredentialProviderAddress) ||
			!strings.Contains(egress, "--credential-provider-server-name=k8s-credential-provider.ate-system.svc") {
			t.Error("egress gateway is not pointed at the bundled provider")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		spec := baseSpec()
		off := false
		spec.Atenet.CredentialProvider = v1alpha1.CredentialProviderSpec{Enabled: &off}
		b := mustRender(t, spec, nil)
		if _, err := find(b.System, "Deployment", "k8s-credential-provider"); err == nil {
			t.Error("bundled provider rendered with injection off")
		}
		if len(b.CreateOnly) != 0 {
			t.Error("namespace policy rendered with injection off")
		}
		if strings.Contains(toYAML(t, b.System), "--credential-provider-name") {
			t.Error("egress gateway has provider flags with injection off")
		}
	})
	t.Run("own provider needs address", func(t *testing.T) {
		spec := baseSpec()
		spec.Atenet.CredentialProvider = v1alpha1.CredentialProviderSpec{Name: "vault"}
		if _, err := Render(Inputs{Spec: WithDefaults(spec)}); err == nil {
			t.Error("Render accepted a provider with no address")
		}
	})
	t.Run("extproc", func(t *testing.T) {
		spec := baseSpec()
		spec.Atenet.AdditionalEgressExtprocService = "policy/authz:9000"
		b := mustRender(t, spec, nil)
		cm := toYAML(t, []*Object{mustFind(t, b.System, "ConfigMap", "atenet-egress")})
		if strings.Count(cm, "cluster_name: additional_egress_ext_proc") != 3 || !strings.Contains(cm, "address: authz.policy.svc.cluster.local") {
			t.Error("ext_proc filter and cluster not spliced into the egress config")
		}
	})
}

func TestConfigHashFollowsConfig(t *testing.T) {
	hash := func(spec v1alpha1.SubstrateInstallSpec) string {
		dep := mustFind(t, mustRender(t, spec, nil).System, "Deployment", "ate-api-server")
		h, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", AnnotationConfigHash)
		if h == "" {
			t.Fatal("ate-api-server has no config hash")
		}
		return h
	}
	spec := baseSpec()
	before := hash(spec)
	if again := hash(spec); again != before {
		t.Error("config hash is not stable across renders")
	}
	spec.ActorJWT.Issuer = "https://issuer.example.com"
	if hash(spec) == before {
		t.Error("config hash did not change with ate-api-server's environment")
	}
	spec.OTLPEndpoint = "http://collector:4317"
	b := mustRender(t, spec, nil)
	if v, _, _ := unstructured.NestedString(mustFind(t, b.System, "ConfigMap", "ate-otel-config").Object, "data", "OTEL_EXPORTER_OTLP_ENDPOINT"); v != "http://collector:4317" {
		t.Errorf("otel endpoint = %q", v)
	}
}

func TestImageRepositoryDefault(t *testing.T) {
	spec := baseSpec()
	spec.Images = v1alpha1.ImagesSpec{}
	want := DefaultImageRepository + "/ateapi:" + upstream.Version
	if !strings.Contains(toYAML(t, mustRender(t, spec, nil).System), want) {
		t.Errorf("render does not reference %s", want)
	}
}

func TestActorJWTIssuerDefault(t *testing.T) {
	cm := mustFind(t, mustRender(t, baseSpec(), nil).System, "ConfigMap", configMapAPIEnvVars)
	// ate-api-server refuses to start without one.
	if v, _, _ := unstructured.NestedString(cm.Object, "data", "ATE_API_ACTOR_JWT_ISSUER"); v != DefaultActorJWTIssuer {
		t.Errorf("ATE_API_ACTOR_JWT_ISSUER = %q, want %q", v, DefaultActorJWTIssuer)
	}
}

func TestAuthenticationConfig(t *testing.T) {
	in := authenticationConfig(InClusterIssuer)
	if !strings.Contains(in, "discoveryTokenFile") || !strings.HasPrefix(in, "actorIdentityJWTProvider: kubernetes\n") {
		t.Errorf("in-cluster config = %q", in)
	}
	gke := authenticationConfig(GKEIssuer(&v1alpha1.GCPSpec{ProjectID: "p", ClusterLocation: "l", ClusterName: "c"}))
	if strings.Contains(gke, "discoveryTokenFile") || !strings.Contains(gke, "issuer: https://container.googleapis.com/v1/projects/p/locations/l/clusters/c") {
		t.Errorf("gke config = %q", gke)
	}
}

func TestExamplesRender(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var install v1alpha1.SubstrateInstall
			if err := yaml.UnmarshalStrict(raw, &install); err != nil {
				t.Fatal(err)
			}
			mustRender(t, install.Spec, nil)
		})
	}
}
