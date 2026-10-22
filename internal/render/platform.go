package render

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
)

// objectstoreUsers are the workloads whose objectstore-plugin sidecar talks
// to snapshot storage, and the service accounts they run as.
var objectstoreUsers = []struct{ kind, name string }{
	{"Deployment", "ate-api-server"},
	{"DaemonSet", "atelet"},
}

// workloadIdentityAnnotation links the ate-api-server service account to the
// Google service account the Cloud SQL proxy authenticates as.
const workloadIdentityAnnotation = "iam.gke.io/gcp-service-account"

// platform applies what the upstream manifests leave to the installer or to
// an overlay: snapshot storage, the GKE-only atelet mounts, service account
// annotations, the Cloud SQL proxy, and the telemetry endpoint.
func (r *renderer) platform(objs []*Object) error {
	// The kind overlay already points storage at its rustfs.
	if r.spec.Platform != v1alpha1.PlatformKind {
		for _, u := range objectstoreUsers {
			obj, err := find(objs, u.kind, u.name)
			if err != nil {
				return err
			}
			if err := mutateContainer(obj, "initContainers", "objectstore-plugin", r.storageEnv); err != nil {
				return err
			}
			sa, err := find(objs, "ServiceAccount", u.name)
			if err != nil {
				return err
			}
			setAnnotations(sa, r.spec.Storage.ServiceAccountAnnotations)
		}
	}

	// atelet runs the node's kubelet image credential provider from GKE's
	// host paths. EKS nodes keep theirs elsewhere. On other nodes those paths
	// hold nothing, as on kind, whose overlay drops them the same way.
	switch r.spec.Platform {
	case v1alpha1.PlatformGeneric:
		atelet, err := find(objs, "DaemonSet", "atelet")
		if err != nil {
			return err
		}
		if err := dropImageCredentialProvider(atelet); err != nil {
			return err
		}
	case v1alpha1.PlatformEKS:
		atelet, err := find(objs, "DaemonSet", "atelet")
		if err != nil {
			return err
		}
		if err := setHostPaths(atelet, eksImageCredentialProvider); err != nil {
			return err
		}
	}

	if c := r.spec.Postgres.CloudSQL; c != nil {
		if err := addCloudSQLProxy(objs, c); err != nil {
			return err
		}
	}

	if endpoint := r.spec.OTLPEndpoint; endpoint != "" {
		cm, err := find(objs, "ConfigMap", "ate-otel-config")
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(cm.Object, endpoint, "data", "OTEL_EXPORTER_OTLP_ENDPOINT"); err != nil {
			return err
		}
	}
	return nil
}

// storageEnv points an objectstore-plugin container at the configured
// backend. Unset or "gcs" selects GCS with ambient credentials; "s3" reads
// the AWS_* variables.
func (r *renderer) storageEnv(c map[string]any) error {
	setEnv(c, "ATE_STORAGE_BACKEND", string(r.spec.Storage.Backend))
	if r.spec.Storage.Backend != v1alpha1.StorageBackendS3 {
		return nil
	}
	s3 := r.spec.Storage.S3
	setEnv(c, "AWS_REGION", s3.Region)
	if s3.Endpoint != "" {
		setEnv(c, "AWS_ENDPOINT_URL", s3.Endpoint)
	}
	if s3.UsePathStyle {
		setEnv(c, "AWS_S3_USE_PATH_STYLE", strconv.FormatBool(true))
	}
	if s3.CredentialsSecretName != "" {
		addEnvFromSecret(c, s3.CredentialsSecretName)
	}
	return nil
}

// imageCredentialProviderVolumes are atelet's GKE host path volumes.
var imageCredentialProviderVolumes = []string{"image-credential-provider-bin", "image-credential-provider-config"}

func dropImageCredentialProvider(atelet *Object) error {
	if err := mutateContainer(atelet, "containers", "atelet", func(c map[string]any) error {
		args, _ := c["args"].([]any)
		kept := args[:0]
		for _, a := range args {
			if s, ok := a.(string); ok && strings.HasPrefix(s, "--image-credential-provider-") {
				continue
			}
			kept = append(kept, a)
		}
		c["args"] = kept
		removeByName(c, "volumeMounts", imageCredentialProviderVolumes...)
		return nil
	}); err != nil {
		return err
	}
	podSpec, _, err := unstructured.NestedMap(atelet.Object, podSpecPath...)
	if err != nil {
		return err
	}
	removeByName(podSpec, "volumes", imageCredentialProviderVolumes...)
	return unstructured.SetNestedMap(atelet.Object, podSpec, podSpecPath...)
}

// hostPath is where a hostPath volume points, and what must be there.
type hostPath struct{ path, kind string }

// eksImageCredentialProvider is where nodeadm installs the ECR credential
// provider and writes the kubelet's CredentialProviderConfig on Amazon Linux
// 2023 nodes. The volumes require both to exist, so a node without them fails
// atelet's mount rather than gaining empty stand-ins.
var eksImageCredentialProvider = map[string]hostPath{
	"image-credential-provider-bin":    {"/etc/eks/image-credential-provider", "Directory"},
	"image-credential-provider-config": {"/etc/eks/image-credential-provider/config.json", "File"},
}

// setHostPaths repoints a workload's named hostPath volumes.
func setHostPaths(obj *Object, paths map[string]hostPath) error {
	path := append(append([]string{}, podSpecPath...), "volumes")
	volumes, _, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil {
		return fmt.Errorf("%s: %w", describe(obj), err)
	}
	set := 0
	for i, item := range volumes {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := v["name"].(string)
		p, ok := paths[name]
		if !ok {
			continue
		}
		v["hostPath"] = map[string]any{"path": p.path, "type": p.kind}
		volumes[i] = v
		set++
	}
	if set != len(paths) {
		return fmt.Errorf("%s has %d of the %d hostPath volumes to repoint", describe(obj), set, len(paths))
	}
	return unstructured.SetNestedSlice(obj.Object, volumes, path...)
}

// addCloudSQLProxy adds upstream's Cloud SQL Auth Proxy native sidecar to
// ate-api-server, and the Workload Identity annotation it authenticates
// through. The sidecar reads its settings from ate-api-server-envvars.
func addCloudSQLProxy(objs []*Object, c *v1alpha1.CloudSQLSpec) error {
	raw, err := readUpstream(installDir, "cloudsql", "proxy-sidecar-patch.yaml")
	if err != nil {
		return err
	}
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []map[string]any `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		return fmt.Errorf("while parsing the Cloud SQL proxy sidecar patch: %w", err)
	}
	sidecars := patch.Spec.Template.Spec.InitContainers
	if len(sidecars) != 1 {
		return fmt.Errorf("the Cloud SQL proxy sidecar patch has %d initContainers, want 1", len(sidecars))
	}

	dep, err := find(objs, "Deployment", "ate-api-server")
	if err != nil {
		return err
	}
	path := append(append([]string{}, podSpecPath...), "initContainers")
	inits, _, err := unstructured.NestedSlice(dep.Object, path...)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedSlice(dep.Object, append(inits, sidecars[0]), path...); err != nil {
		return err
	}

	if c.ServiceAccount != "" {
		sa, err := find(objs, "ServiceAccount", "ate-api-server")
		if err != nil {
			return err
		}
		setAnnotations(sa, map[string]string{workloadIdentityAnnotation: c.ServiceAccount})
	}
	return nil
}
