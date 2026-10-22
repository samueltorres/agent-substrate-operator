package render

import (
	"encoding/base64"
	"fmt"
	"maps"
	"strings"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
)

// Names of the ate-api-server configuration objects, as upstream's installer
// writes them and the manifests consume them.
const (
	configMapAPIEnvVars    = "ate-api-server-envvars"
	secretAPIEnvVars       = "ate-api-server-secret-envvars"
	secretPostgresServerCA = "postgres-server-ca"
	configMapAPIAuthn      = "ate-api-authentication"
)

// Keys of the Secret spec.postgres.connectionSecretName names.
const (
	KeyReadWriteConnectionString = "readWriteConnectionString"
	KeyOwnerConnectionString     = "ownerConnectionString"
	KeyServerCA                  = "serverCA"
)

// Bundled PostgreSQL identities and schema, from upstream pkg/postgressetup.
const (
	postgresSchema        = "substrate"
	postgresOwnerRole     = "substrate_owner"
	postgresOwnerUser     = "substrate_owner_user"
	postgresReadWriteRole = "substrate_readwrite"
	postgresReadWriteUser = "substrate_readwrite_user"
)

// bundledPostgresHost is where ate-api-server reaches the bundled PostgreSQL.
const bundledPostgresHost = "postgres.ate-system.svc:5432"

// size10PostgresPoolParams is appended to the bundled read/write connection
// on size10 clusters.
const size10PostgresPoolParams = "&pool_max_conns=64&pool_min_conns=4"

// bundledPostgresDSN is the connection for a bundled login user, which
// presents a pod certificate for that user.
func bundledPostgresDSN(user string) string {
	bundle := "/run/postgres.podcert.ate.dev/" + user + ".pem"
	return fmt.Sprintf("postgresql://%s@%s/atepg?sslmode=verify-full&sslrootcert=/run/servicedns.podcert.ate.dev/trust-bundle.pem&sslcert=%s&sslkey=%s",
		user, bundledPostgresHost, bundle, bundle)
}

// apiServerConfig renders the ConfigMap and Secrets ate-api-server reads its
// store and identity settings from, as upstream's CreateAPIServerEnvVars and
// CreateAPIAuthenticationConfig write them.
func apiServerConfig(spec *v1alpha1.SubstrateInstallSpec, issuer string, external map[string][]byte) ([]*Object, error) {
	pg := spec.Postgres
	var readWriteDSN, ownerDSN string
	var serverCA []byte
	switch {
	case pg.ConnectionSecretName != "":
		readWriteDSN = string(external[KeyReadWriteConnectionString])
		if readWriteDSN == "" {
			return nil, fmt.Errorf("secret %s has no %s", pg.ConnectionSecretName, KeyReadWriteConnectionString)
		}
		ownerDSN = string(external[KeyOwnerConnectionString])
		serverCA = external[KeyServerCA]
	case pg.CloudSQL != nil:
		dsn, err := cloudSQLDSN(pg.CloudSQL)
		if err != nil {
			return nil, err
		}
		readWriteDSN = dsn
	default:
		readWriteDSN = bundledPostgresDSN(postgresReadWriteUser)
		if spec.ClusterSize == v1alpha1.ClusterSize10 {
			readWriteDSN += size10PostgresPoolParams
		}
		ownerDSN = bundledPostgresDSN(postgresOwnerUser)
	}
	if ownerDSN == "" {
		ownerDSN = readWriteDSN
	}

	envVars := cloudSQLEnvVars(pg.CloudSQL)
	envVars["ATE_API_POSTGRES_READ_WRITE_ROLE"] = orDefault(pg.ReadWriteRole, postgresReadWriteRole)
	envVars["ATE_API_POSTGRES_OWNER_ROLE"] = orDefault(pg.OwnerRole, postgresOwnerRole)
	if pg.PoolMaxConns != "" {
		envVars["ATE_API_POSTGRES_POOL_MAX_CONNS"] = pg.PoolMaxConns
	}
	envVars["ATE_API_ACTOR_JWT_ISSUER"] = spec.ActorJWT.Issuer

	authn := spec.APIServer.AuthenticationConfig
	if authn == "" {
		authn = authenticationConfig(issuer)
	}

	objs := []*Object{
		configMap(configMapAPIEnvVars, envVars),
		secret(secretAPIEnvVars, map[string][]byte{
			"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING": []byte(readWriteDSN),
			"ATE_API_POSTGRES_OWNER_CONNECTION_STRING":      []byte(ownerDSN),
			"ATE_API_POSTGRES_SCHEMA":                       []byte(orDefault(pg.Schema, postgresSchema)),
		}),
		configMap(configMapAPIAuthn, map[string]string{"authentication.yaml": authn}),
	}
	if len(serverCA) > 0 {
		objs = append(objs, secret(secretPostgresServerCA, map[string][]byte{"server-ca.pem": serverCA}))
	}
	return objs, nil
}

// InClusterIssuer is the issuer of a cluster that publishes no discoverable
// one.
const InClusterIssuer = "https://kubernetes.default.svc"

// GKEIssuer is the service account token issuer of a GKE cluster.
func GKEIssuer(gcp *v1alpha1.GCPSpec) string {
	return fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
		gcp.ProjectID, gcp.ClusterLocation, gcp.ClusterName)
}

// authenticationConfig renders authentication.yaml for the cluster's service
// account issuer. An in-cluster issuer is not reachable over public
// discovery, so ate-api-server is pointed at its own service account CA and
// token for the OIDC discovery handshake.
func authenticationConfig(issuer string) string {
	config := fmt.Sprintf(
		"actorIdentityJWTProvider: kubernetes\njwtProviders:\n- name: kubernetes\n  issuer: %s\n  audiences: [api.ate-system.svc]\n",
		issuer)
	switch issuer {
	case InClusterIssuer, InClusterIssuer + ".cluster.local":
		config += "  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\n" +
			"  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\n"
	}
	return strings.TrimRight(config, "\n")
}

// gsaEmailSuffix is trimmed off the Google service account email to get the
// Cloud SQL IAM database user.
const gsaEmailSuffix = ".gserviceaccount.com"

// cloudSQLDSN is the passwordless connection through the proxy sidecar, which
// owns TLS and IAM database authentication.
func cloudSQLDSN(c *v1alpha1.CloudSQLSpec) (string, error) {
	if c.IAMAuth != nil && !*c.IAMAuth {
		return "", fmt.Errorf("cloudSQL.iamAuth false needs postgres.connectionSecretName, since no passwordless connection exists (use host=127.0.0.1 to stay on the proxy)")
	}
	if c.ServiceAccount == "" {
		return "", fmt.Errorf("cloudSQL needs serviceAccount, or postgres.connectionSecretName")
	}
	user := strings.TrimSuffix(c.ServiceAccount, gsaEmailSuffix)
	return fmt.Sprintf("user=%s host=127.0.0.1 port=5432 dbname=atepg sslmode=disable", user), nil
}

// cloudSQLEnvVars is the Cloud SQL proxy sidecar's configuration. Its health
// checks listen on 9801 because ateapi's metrics and health servers own 9090
// and 9091.
func cloudSQLEnvVars(c *v1alpha1.CloudSQLSpec) map[string]string {
	if c == nil {
		return map[string]string{}
	}
	iamAuth := "true"
	if c.IAMAuth != nil && !*c.IAMAuth {
		iamAuth = "false"
	}
	data := map[string]string{
		"ATE_API_POSTGRES_CLOUDSQL_INSTANCE": c.Instance,
		"CSQL_PROXY_AUTO_IAM_AUTHN":          iamAuth,
		"CSQL_PROXY_PORT":                    "5432",
		"CSQL_PROXY_HEALTH_CHECK":            "true",
		"CSQL_PROXY_HTTP_ADDRESS":            "0.0.0.0",
		"CSQL_PROXY_HTTP_PORT":               "9801",
		"CSQL_PROXY_STRUCTURED_LOGS":         "true",
	}
	switch c.IPType {
	case "", "private":
		data["CSQL_PROXY_PRIVATE_IP"] = "true"
	case "psc":
		data["CSQL_PROXY_PSC"] = "true"
	}
	return data
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// configMap builds a ConfigMap in the control plane namespace.
func configMap(name string, data map[string]string) *Object {
	d := make(map[string]any, len(data))
	for k, v := range data {
		d[k] = v
	}
	return &Object{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": Namespace},
		"data":       d,
	}}
}

// secret builds an Opaque Secret in the control plane namespace.
func secret(name string, data map[string][]byte) *Object {
	d := make(map[string]any, len(data))
	for k := range maps.Keys(data) {
		d[k] = base64.StdEncoding.EncodeToString(data[k])
	}
	return &Object{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata":   map[string]any{"name": name, "namespace": Namespace},
		"data":       d,
	}}
}
