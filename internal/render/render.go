// Package render turns a SubstrateInstall spec into the objects that install
// the embedded Agent Substrate release. It reproduces what upstream's
// `ate-setup deploy ate-system` applies, grouped into the stages the
// controller applies and waits on in order.
package render

import (
	"fmt"
	"io/fs"
	"path"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

// Namespaces the upstream manifests name literally.
const (
	Namespace        = "ate-system"
	PodCertNamespace = "podcertificate-controller-system"
)

// Labels and annotations the operator puts on what it renders.
const (
	// LabelInstall marks every object the operator applies, so it can find
	// and prune them.
	LabelInstall = "substrate.samueltorres.github.io/install"
	// LabelManagedBy is the standard managed-by label.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is LabelManagedBy's value.
	ManagedBy = "agent-substrate-operator"
	// AnnotationConfigHash on a pod template digests the rendered ConfigMaps
	// and Secrets the pods read, so a change to them rolls the pods.
	AnnotationConfigHash = "substrate.samueltorres.github.io/config-hash"
)

// DefaultImageRepository is where this project's CI publishes the Substrate
// images: rebuilds of the embedded upstream release, which publishes none.
const DefaultImageRepository = "ghcr.io/samueltorres/substrate"

// DefaultActorJWTIssuer is upstream's default iss claim for actor JWTs, the
// in-cluster address of its identity provider service.
const DefaultActorJWTIssuer = "https://idp.ate-system.svc"

// installDir is the upstream manifest root.
const installDir = "manifests/ate-install"

// k8sCredentialProviderName and its default address are upstream's bundled
// Kubernetes Secrets provider.
const (
	k8sCredentialProviderName    = "k8s.io"
	k8sCredentialProviderAddress = "k8s-credential-provider.ate-system.svc:50051"
)

// Inputs is what Render needs beyond the spec: values the controller reads
// from the cluster.
type Inputs struct {
	Spec v1alpha1.SubstrateInstallSpec
	// Issuer is the service account token issuer ate-api-server trusts.
	Issuer string
	// ExternalPostgres is the data of the Secret
	// spec.postgres.connectionSecretName names.
	ExternalPostgres map[string][]byte
}

// Bundle is the rendered install, in the order the stages are applied.
type Bundle struct {
	// Namespace is ate-system, which everything else lands in.
	Namespace []*Object
	// CRDs are Substrate's CRDs and the RBAC generated beside them.
	CRDs []*Object
	// PodCertificate is the podcertificate-controller. Every component
	// projects its certificates and trust bundles.
	PodCertificate []*Object
	// SandboxConfig is the SandboxConfig admission policy and the default
	// gVisor SandboxConfig. It needs the CRDs.
	SandboxConfig []*Object
	// Postgres is the bundled PostgreSQL, empty with an external database.
	Postgres []*Object
	// System is the rest of the control plane: ate-api-server,
	// ate-controller, atelet, atenet, and their configuration.
	System []*Object
	// CreateOnly objects are created when missing and otherwise left alone:
	// they hold settings an administrator edits in the cluster.
	CreateOnly []*Object
}

// All returns every object Render produced except CreateOnly.
func (b *Bundle) All() []*Object {
	return slices.Concat(b.Namespace, b.CRDs, b.PodCertificate, b.SandboxConfig, b.Postgres, b.System)
}

// WithDefaults fills in the defaults a spec leaves unset. The CRD defaults
// most fields; these depend on other fields or on the embedded release.
func WithDefaults(spec v1alpha1.SubstrateInstallSpec) v1alpha1.SubstrateInstallSpec {
	if spec.Platform == "" {
		spec.Platform = v1alpha1.PlatformGeneric
	}
	if spec.Images.Repository == "" {
		spec.Images.Repository = DefaultImageRepository
	}
	if spec.Images.Tag == "" {
		spec.Images.Tag = upstream.Version
	}
	if spec.ClusterSize == "" {
		spec.ClusterSize = v1alpha1.ClusterSize0
	}
	if spec.Storage.Backend == "" && spec.Platform != v1alpha1.PlatformKind {
		spec.Storage.Backend = v1alpha1.StorageBackendS3
		if spec.Platform == v1alpha1.PlatformGKE {
			spec.Storage.Backend = v1alpha1.StorageBackendGCS
		}
	}
	if spec.ActorJWT.Algorithm == "" {
		spec.ActorJWT.Algorithm = "ES256"
	}
	if spec.ActorJWT.Issuer == "" {
		spec.ActorJWT.Issuer = DefaultActorJWTIssuer
	}
	return spec
}

// Render renders the install. in.Spec must have WithDefaults applied.
func Render(in Inputs) (*Bundle, error) {
	spec := &in.Spec
	if err := validate(spec); err != nil {
		return nil, err
	}
	k, err := newKustomizer()
	if err != nil {
		return nil, err
	}
	r := &renderer{spec: spec, k: k, images: images{repository: spec.Images.Repository, tag: spec.Images.Tag}}

	b := &Bundle{}
	if b.Namespace, err = r.file(false, installDir, "ate-system-namespace.yaml"); err != nil {
		return nil, err
	}
	if b.CRDs, err = r.dir(installDir, "generated"); err != nil {
		return nil, err
	}
	if b.PodCertificate, err = r.podCertificate(); err != nil {
		return nil, err
	}
	sandbox, err := r.file(false, installDir, "sandboxconfig-validation.yaml")
	if err != nil {
		return nil, err
	}
	gvisor, err := r.file(false, installDir, "sandboxconfig-gvisor.yaml")
	if err != nil {
		return nil, err
	}
	b.SandboxConfig = slices.Concat(sandbox, gvisor)
	if bundledPostgres(spec) {
		if b.Postgres, err = r.postgres(); err != nil {
			return nil, err
		}
	}
	if b.System, b.CreateOnly, err = r.system(in); err != nil {
		return nil, err
	}

	for _, stage := range [][]*Object{b.Namespace, b.CRDs, b.PodCertificate, b.SandboxConfig, b.Postgres, b.System} {
		if err := stampConfigHashes(stage); err != nil {
			return nil, err
		}
		label(stage, true)
	}
	label(b.CreateOnly, false)
	return b, nil
}

func validate(spec *v1alpha1.SubstrateInstallSpec) error {
	if spec.Storage.Backend == v1alpha1.StorageBackendS3 && spec.Storage.S3 == nil {
		return fmt.Errorf("storage.backend s3 needs storage.s3")
	}
	if _, err := provider(spec); err != nil {
		return err
	}
	return nil
}

// provider resolves spec.atenet.credentialProvider.
func provider(spec *v1alpha1.SubstrateInstallSpec) (credentialProvider, error) {
	p := spec.Atenet.CredentialProvider
	if p.Enabled != nil && !*p.Enabled {
		return credentialProvider{}, nil
	}
	if p.Name == "" {
		return credentialProvider{}, fmt.Errorf("atenet.credentialProvider needs a name, or enabled: false")
	}
	address := p.Address
	if address == "" && p.Name == k8sCredentialProviderName {
		address = k8sCredentialProviderAddress
	}
	if address == "" {
		return credentialProvider{}, fmt.Errorf("credential provider %s needs an address", p.Name)
	}
	return credentialProvider{name: p.Name, address: address}, nil
}

// bundledPostgres reports whether the install deploys its own PostgreSQL.
func bundledPostgres(spec *v1alpha1.SubstrateInstallSpec) bool {
	return spec.Postgres.ConnectionSecretName == "" && spec.Postgres.CloudSQL == nil
}

type renderer struct {
	spec   *v1alpha1.SubstrateInstallSpec
	k      *kustomizer
	images images
}

// finish layers the cordon-control-plane component over a manifest when
// asked, as upstream renders every control plane apply, resolves its images,
// and decodes it.
func (r *renderer) finish(manifest []byte, cordon bool) ([]*Object, error) {
	if cordon && r.spec.CordonControlPlane {
		var err error
		manifest, err = r.k.compose(manifest, upstreamPath(installDir, "components", "cordon-control-plane"))
		if err != nil {
			return nil, err
		}
	}
	resolved, err := r.images.resolve(manifest)
	if err != nil {
		return nil, err
	}
	return decode(resolved)
}

// file renders one upstream manifest file.
func (r *renderer) file(cordon bool, rel ...string) ([]*Object, error) {
	raw, err := readUpstream(rel...)
	if err != nil {
		return nil, err
	}
	return r.finish(raw, cordon)
}

// dir renders every manifest file in an upstream directory.
func (r *renderer) dir(rel ...string) ([]*Object, error) {
	entries, err := fs.ReadDir(upstream.FS(), path.Join(rel...))
	if err != nil {
		return nil, err
	}
	var objs []*Object
	for _, e := range entries {
		got, err := r.file(false, append(slices.Clone(rel), e.Name())...)
		if err != nil {
			return nil, err
		}
		objs = append(objs, got...)
	}
	return objs, nil
}

// build renders an upstream kustomization directory.
func (r *renderer) build(cordon bool, rel ...string) ([]*Object, error) {
	raw, err := r.k.build(upstreamPath(rel...))
	if err != nil {
		return nil, err
	}
	return r.finish(raw, cordon)
}

// podCertificate renders the podcertificate-controller, through the size10
// overlay on size10 clusters, with WORKERS_PER_SIGNER set when asked.
func (r *renderer) podCertificate() ([]*Object, error) {
	var objs []*Object
	var err error
	if r.spec.ClusterSize == v1alpha1.ClusterSize10 {
		objs, err = r.build(true, installDir, "podcert-size10")
	} else {
		objs, err = r.file(true, installDir, "pod-certificate-controller.yaml")
	}
	if err != nil {
		return nil, err
	}
	if workers := r.spec.PodCertificate.WorkersPerSigner; workers > 0 {
		dep, err := find(objs, "Deployment", "podcertificate-controller")
		if err != nil {
			return nil, err
		}
		if err := mutateContainer(dep, "containers", "controller", func(c map[string]any) error {
			setEnv(c, "WORKERS_PER_SIGNER", fmt.Sprint(workers))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// The size10 PostgreSQL container, which upstream's
// postgres-size10/postgres-config-patch.yaml is tuned to.
const (
	size10PostgresCPURequest = "80"
	size10PostgresMemory     = "140Gi"
)

// postgres renders the bundled PostgreSQL: the kind overlay trims it to a
// laptop, and size10 resizes it for a dedicated node.
func (r *renderer) postgres() ([]*Object, error) {
	var objs []*Object
	var err error
	if r.spec.Platform == v1alpha1.PlatformKind {
		objs, err = r.build(true, installDir, "kind", "postgres")
	} else {
		objs, err = r.file(true, installDir, "postgres", "postgres.yaml")
	}
	if err != nil {
		return nil, err
	}
	sts, err := find(objs, "StatefulSet", "postgres")
	if err != nil {
		return nil, err
	}
	if class := r.spec.Postgres.StorageClassName; class != "" {
		claims, _, _ := unstructured.NestedSlice(sts.Object, "spec", "volumeClaimTemplates")
		found := false
		for i, c := range claims {
			claim, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(claim, "metadata", "name"); name != "data" {
				continue
			}
			if err := unstructured.SetNestedField(claim, class, "spec", "storageClassName"); err != nil {
				return nil, err
			}
			claims[i] = claim
			found = true
		}
		if !found {
			return nil, fmt.Errorf("%s has no data volume claim to set the StorageClass on", describe(sts))
		}
		if err := unstructured.SetNestedSlice(sts.Object, claims, "spec", "volumeClaimTemplates"); err != nil {
			return nil, err
		}
	}
	if r.spec.ClusterSize == v1alpha1.ClusterSize10 {
		if err := r.postgresSize10(objs, sts); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// postgresSize10 replaces postgresql.conf with the size10 tuning and gives
// the container the size10 resources. There is deliberately no CPU limit.
func (r *renderer) postgresSize10(objs []*Object, sts *Object) error {
	raw, err := readUpstream(installDir, "postgres-size10", "postgres-config-patch.yaml")
	if err != nil {
		return err
	}
	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		return fmt.Errorf("while parsing the size10 postgres config patch: %w", err)
	}
	cm, err := find(objs, "ConfigMap", "postgres-config")
	if err != nil {
		return err
	}
	for key, value := range patch.Data {
		if err := unstructured.SetNestedField(cm.Object, value, "data", key); err != nil {
			return err
		}
	}
	return mutateContainer(sts, "containers", "postgres", func(c map[string]any) error {
		unstructured.RemoveNestedField(c, "resources", "limits", "cpu")
		for _, f := range []struct {
			path  []string
			value string
		}{
			{[]string{"resources", "requests", "cpu"}, size10PostgresCPURequest},
			{[]string{"resources", "requests", "memory"}, size10PostgresMemory},
			{[]string{"resources", "limits", "memory"}, size10PostgresMemory},
		} {
			if err := unstructured.SetNestedField(c, f.value, f.path...); err != nil {
				return err
			}
		}
		return nil
	})
}

// system renders the control plane bundle, the API server configuration,
// the bundled credential provider when selected, and the egress gateway.
func (r *renderer) system(in Inputs) (objs, createOnly []*Object, err error) {
	overlay := "base"
	if r.spec.Platform == v1alpha1.PlatformKind {
		overlay = "kind"
	}
	if objs, err = r.build(true, installDir, overlay); err != nil {
		return nil, nil, err
	}

	config, err := apiServerConfig(r.spec, in.Issuer, in.ExternalPostgres)
	if err != nil {
		return nil, nil, err
	}
	objs = append(config, objs...)

	p, err := provider(r.spec)
	if err != nil {
		return nil, nil, err
	}
	if p.name == k8sCredentialProviderName {
		cp, err := r.file(true, "manifests", "egress-credential-injection", "k8s-credential-provider.yaml")
		if err != nil {
			return nil, nil, err
		}
		objs = append(objs, cp...)
		if createOnly, err = r.file(false, "manifests", "egress-credential-injection", "namespace-policy.yaml"); err != nil {
			return nil, nil, err
		}
	}

	raw, err := readUpstream(installDir, "atenet-egress.yaml")
	if err != nil {
		return nil, nil, err
	}
	if raw, err = patchEgress(raw, r.spec.Atenet.AdditionalEgressExtprocService, p); err != nil {
		return nil, nil, err
	}
	egress, err := r.finish(raw, true)
	if err != nil {
		return nil, nil, err
	}
	objs = append(objs, egress...)

	if err := r.platform(objs); err != nil {
		return nil, nil, err
	}
	return objs, createOnly, nil
}
