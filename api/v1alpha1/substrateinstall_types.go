package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SingletonName is the only name a SubstrateInstall may have. The upstream
// manifests name the ate-system namespace literally, so a cluster holds one
// install.
const SingletonName = "substrate"

// Platform selects the provider-specific parts of the install.
// +kubebuilder:validation:Enum=generic;gke;eks;kind
type Platform string

const (
	// PlatformGeneric is any conformant cluster. Snapshots go to S3 or GCS.
	PlatformGeneric Platform = "generic"
	// PlatformGKE keeps the GKE parts of the upstream manifests: the node's
	// image credential provider, GCS, and Google Managed Prometheus.
	PlatformGKE Platform = "gke"
	// PlatformEKS points atelet at the ECR credential provider EKS installs
	// on Amazon Linux 2023 nodes, so actor images pull from private ECR.
	PlatformEKS Platform = "eks"
	// PlatformKind is the upstream kind overlay: in-cluster rustfs for
	// snapshots, an OpenTelemetry collector, Jaeger, and Prometheus.
	PlatformKind Platform = "kind"
)

// StorageBackend is the object store the objectstore plugin talks to.
// +kubebuilder:validation:Enum=gcs;s3
type StorageBackend string

const (
	StorageBackendGCS StorageBackend = "gcs"
	StorageBackendS3  StorageBackend = "s3"
)

// ClusterSize is upstream's coarse footprint profile.
// +kubebuilder:validation:Enum=size0;size10
type ClusterSize string

const (
	ClusterSize0 ClusterSize = "size0"
	// ClusterSize10 assumes a dedicated PostgreSQL node: it resizes the
	// bundled database, pins ate-api-server's connection pool, and raises the
	// podcertificate-controller's API rate limits.
	ClusterSize10 ClusterSize = "size10"
)

// DeletionPolicy says what deleting the SubstrateInstall does to the cluster.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves every installed object in place.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete tears the install down as `ate-setup delete
	// ate-system` does, including the CRDs and so every WorkerPool,
	// and the bundled PostgreSQL with its data.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// SubstrateInstallSpec is the desired Agent Substrate installation. Field
// names follow upstream's install.ate.dev/v1alpha1 SubstrateInstall document
// where one exists.
//
// +kubebuilder:validation:XValidation:rule="self.platform != 'kind' || !has(self.storage)",message="kind always uses the bundled rustfs; leave storage unset"
type SubstrateInstallSpec struct {
	// Platform selects the provider-specific parts of the install.
	// +kubebuilder:default=generic
	// +optional
	Platform Platform `json:"platform,omitempty"`

	// Images says where the Substrate component images are published.
	// +optional
	Images ImagesSpec `json:"images,omitzero"`

	// Storage configures where actor snapshots are stored.
	// +optional
	Storage StorageSpec `json:"storage,omitzero"`

	// Postgres configures ate-api-server's store. With neither
	// connectionSecretName nor cloudSQL set, the operator deploys the
	// bundled single-replica PostgreSQL.
	// +optional
	Postgres PostgresSpec `json:"postgres,omitzero"`

	// Atenet configures the ingress router and egress gateway.
	Atenet AtenetSpec `json:"atenet"`

	// APIServer configures ate-api-server.
	// +optional
	APIServer APIServerSpec `json:"apiServer,omitzero"`

	// ActorJWT configures the actor identity JWTs the egress gateway mints.
	// +optional
	ActorJWT ActorJWTSpec `json:"actorJWT,omitzero"`

	// GCP names the GKE cluster, from which the service account token issuer
	// is derived.
	// +optional
	GCP *GCPSpec `json:"gcp,omitempty"`

	// OTLPEndpoint sends control plane telemetry somewhere other than the
	// platform default.
	// +optional
	OTLPEndpoint string `json:"otlpEndpoint,omitempty"`

	// ClusterSize is the footprint profile.
	// +kubebuilder:default=size0
	// +optional
	ClusterSize ClusterSize `json:"clusterSize,omitempty"`

	// CordonControlPlane keeps the control plane off the worker nodes. It
	// assumes a pool labeled and tainted
	// ate.dev/workloadType=ate-control-plane:NoSchedule, and a one-node pool
	// labeled and tainted ate.dev/workloadType=ate-postgres:NoSchedule.
	// +optional
	CordonControlPlane bool `json:"cordonControlPlane,omitempty"`

	// PodCertificate configures the podcertificate-controller.
	// +optional
	PodCertificate PodCertificateSpec `json:"podCertificate,omitzero"`

	// DeletionPolicy says what deleting this object does to the cluster.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// ImagesSpec says where the Substrate component images are published.
type ImagesSpec struct {
	// Repository holds every component image, named as upstream's
	// `ate-setup publish release-images` publishes them, e.g.
	// registry.example.com/substrate/ateapi. Defaults to
	// ghcr.io/samueltorres/substrate, where this project's CI publishes
	// rebuilds of each upstream release.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tag every component image carries. Defaults to the Substrate release
	// this operator embeds.
	// +optional
	Tag string `json:"tag,omitempty"`
}

// StorageSpec configures snapshot storage.
type StorageSpec struct {
	// Backend is the object store. Defaults to gcs on gke and s3 elsewhere.
	// +optional
	Backend StorageBackend `json:"backend,omitempty"`

	// S3 configures the S3 backend.
	// +optional
	S3 *S3Spec `json:"s3,omitempty"`

	// ServiceAccountAnnotations are set on the ate-api-server and atelet
	// service accounts, whose objectstore plugins reach the bucket. Use it
	// for workload identity, e.g. eks.amazonaws.com/role-arn.
	// +optional
	ServiceAccountAnnotations map[string]string `json:"serviceAccountAnnotations,omitempty"`
}

// S3Spec configures the S3 snapshot backend.
type S3Spec struct {
	// Region is the bucket's region.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// Endpoint overrides the S3 endpoint, for S3-compatible stores.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// UsePathStyle addresses buckets by path rather than by host.
	// +optional
	UsePathStyle bool `json:"usePathStyle,omitempty"`

	// CredentialsSecretName names a Secret in ate-system holding
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Without it the plugin
	// uses ambient credentials.
	// +optional
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`
}

// PostgresSpec configures ate-api-server's PostgreSQL store.
//
// +kubebuilder:validation:XValidation:rule="has(self.connectionSecretName) || has(self.cloudSQL) || (!has(self.schema) && !has(self.readWriteRole) && !has(self.ownerRole))",message="the bundled PostgreSQL uses fixed roles and schema; set schema and roles only for an external database"
type PostgresSpec struct {
	// ConnectionSecretName names a Secret in ate-system holding an external
	// database's connection strings: readWriteConnectionString, and
	// optionally ownerConnectionString (defaults to the read-write one) and
	// serverCA, a PEM for sslmode=verify-ca, mounted at
	// /run/postgres-server-ca/server-ca.pem.
	// +optional
	ConnectionSecretName string `json:"connectionSecretName,omitempty"`

	// CloudSQL adds the Cloud SQL Auth Proxy sidecar to ate-api-server.
	// Without connectionSecretName, the operator connects through it with
	// IAM database authentication and no password.
	// +optional
	CloudSQL *CloudSQLSpec `json:"cloudSQL,omitempty"`

	// Schema ate-api-server keeps its tables in. Defaults to substrate.
	// +optional
	Schema string `json:"schema,omitempty"`

	// ReadWriteRole is the role ate-api-server serves requests as.
	// Defaults to substrate_readwrite.
	// +optional
	ReadWriteRole string `json:"readWriteRole,omitempty"`

	// OwnerRole is the role that owns the schema and runs migrations.
	// Defaults to substrate_owner.
	// +optional
	OwnerRole string `json:"ownerRole,omitempty"`

	// PoolMaxConns caps ate-api-server's connection pool.
	// +optional
	PoolMaxConns string `json:"poolMaxConns,omitempty"`

	// StorageClassName is the bundled PostgreSQL volume's StorageClass.
	// Changing it on an existing install needs the StatefulSet and its claim
	// deleted first.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// CloudSQLIPType is how the Cloud SQL proxy reaches the instance.
// +kubebuilder:validation:Enum=private;public;psc
type CloudSQLIPType string

// CloudSQLSpec configures the Cloud SQL Auth Proxy sidecar.
type CloudSQLSpec struct {
	// Instance is the connection name, PROJECT:REGION:INSTANCE.
	// +kubebuilder:validation:MinLength=1
	Instance string `json:"instance"`

	// ServiceAccount is the Google service account ate-api-server runs as
	// through Workload Identity, and the IAM database user.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// IAMAuth turns on automatic IAM database authentication.
	// +kubebuilder:default=true
	// +optional
	IAMAuth *bool `json:"iamAuth,omitempty"`

	// IPType is how the proxy reaches the instance.
	// +kubebuilder:default=private
	// +optional
	IPType CloudSQLIPType `json:"ipType,omitempty"`
}

// AtenetSpec configures the ingress router and egress gateway. Only the
// Envoy dataplane is supported.
type AtenetSpec struct {
	// CredentialProvider is the provider the egress gateway asks for
	// credentials to inject.
	CredentialProvider CredentialProviderSpec `json:"credentialProvider"`

	// AdditionalEgressExtprocService adds an ext_proc authorization filter to
	// the egress gateway, as NAMESPACE/SERVICE:PORT.
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+/[a-z0-9-]+:[0-9]+$`
	// +optional
	AdditionalEgressExtprocService string `json:"additionalEgressExtprocService,omitempty"`
}

// CredentialProviderSpec is upstream's --credential-provider: name k8s.io
// deploys the bundled Kubernetes Secrets provider, enabled false turns
// injection off, and any other name with an address points at a provider you
// deploy.
//
// +kubebuilder:validation:XValidation:rule="(has(self.enabled) && !self.enabled) ? (!has(self.name) && !has(self.address)) : has(self.name)",message="set name (and address unless it is k8s.io), or enabled: false alone"
// +kubebuilder:validation:XValidation:rule="!has(self.name) || self.name == 'k8s.io' || has(self.address)",message="a provider other than k8s.io needs an address"
type CredentialProviderSpec struct {
	// Enabled turns egress credential injection on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Name is the host of the ate-secret:// URIs the provider serves.
	// +optional
	Name string `json:"name,omitempty"`

	// Address is the provider's host:port.
	// +optional
	Address string `json:"address,omitempty"`
}

// APIServerSpec configures ate-api-server.
type APIServerSpec struct {
	// ExpectedJWTIssuer is the service account token issuer ate-api-server
	// trusts. Defaults to the GKE issuer when gcp is set, then to the
	// cluster's OpenID discovery document, then to
	// https://kubernetes.default.svc.
	// +optional
	ExpectedJWTIssuer string `json:"expectedJWTIssuer,omitempty"`

	// AuthenticationConfig replaces the generated authentication.yaml, for
	// installs that trust more JWT providers. See upstream
	// docs/authentication.md.
	// +optional
	AuthenticationConfig string `json:"authenticationConfig,omitempty"`
}

// ActorJWTSpec configures actor identity JWTs.
type ActorJWTSpec struct {
	// Algorithm of the signing key generated on first install. Changing it
	// later does not rotate the key.
	// +kubebuilder:validation:Enum=ES256;RS256
	// +kubebuilder:default=ES256
	// +optional
	Algorithm string `json:"algorithm,omitempty"`

	// Issuer is the iss claim of the minted JWTs. Relying parties fetch
	// <issuer>/.well-known/openid-configuration to verify them. Defaults to
	// https://idp.ate-system.svc.
	// +kubebuilder:validation:Pattern=`^https://[^/?#@]+(/[^?#]*)?$`
	// +optional
	Issuer string `json:"issuer,omitempty"`
}

// GCPSpec names a GKE cluster.
type GCPSpec struct {
	// +kubebuilder:validation:MinLength=1
	ProjectID string `json:"projectID"`
	// +kubebuilder:validation:MinLength=1
	ClusterLocation string `json:"clusterLocation"`
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
}

// PodCertificateSpec configures the podcertificate-controller.
type PodCertificateSpec struct {
	// WorkersPerSigner is the number of concurrent workers per signer. Zero
	// keeps the controller's default.
	// +kubebuilder:validation:Minimum=0
	// +optional
	WorkersPerSigner int32 `json:"workersPerSigner,omitempty"`
}

// Condition types.
const (
	// ConditionReady is true once every component has rolled out.
	ConditionReady = "Ready"
	// ConditionProgressing is true while the operator applies or waits.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is true when the last reconcile failed.
	ConditionDegraded = "Degraded"
)

// SubstrateInstallStatus is the observed state of the install.
type SubstrateInstallStatus struct {
	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Version is the Substrate release the operator installs.
	// +optional
	Version string `json:"version,omitempty"`

	// Step is the install step the operator last worked on.
	// +optional
	Step string `json:"step,omitempty"`

	// PostgresSetupPodUID is the bundled PostgreSQL pod the identity setup
	// script last ran against.
	// +optional
	PostgresSetupPodUID string `json:"postgresSetupPodUID,omitempty"`

	// Conditions are Ready, Progressing, and Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SubstrateInstall installs Agent Substrate into the cluster. It is
// cluster-scoped, and the only one allowed is named "substrate".
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=subi
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=`.status.step`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'substrate'",message="the SubstrateInstall must be named substrate"
type SubstrateInstall struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubstrateInstallSpec   `json:"spec,omitempty"`
	Status SubstrateInstallStatus `json:"status,omitempty"`
}

// SubstrateInstallList is a list of SubstrateInstalls.
//
// +kubebuilder:object:root=true
type SubstrateInstallList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SubstrateInstall `json:"items"`
}
