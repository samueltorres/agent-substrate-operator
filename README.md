# agent-substrate-operator

A Kubernetes operator that installs [Agent Substrate](https://github.com/agent-substrate/substrate),
and the Helm chart that installs the operator.

You describe the install in one cluster-scoped `SubstrateInstall` resource.
The operator applies the upstream manifests in the order upstream's installer
(`ate-setup deploy ate-system`) uses, waits on each stage, and reports progress
in the resource's status. It keeps the install in that state afterwards.

Each operator release embeds one Agent Substrate release. This one installs
**v0.5.0**.

## What the operator does

Upstream's installer is an imperative Go program. The operator does the same
work as a reconcile loop:

| Step | What happens |
|---|---|
| `Namespace`, `CRDs` | Applies `ate-system` and Substrate's CRDs, and waits until they are established. |
| `Secrets` | Creates the actor identity JWT and CA pools and the egress MITM CA pool, if missing. It never regenerates an existing pool. |
| `PodCertificate` | Creates the signer CA pools and the podcertificate-controller, and waits for its ClusterTrustBundles. |
| `SandboxConfig` | Applies the SandboxConfig admission policy and the default gVisor SandboxConfig. |
| `Postgres` | Applies the bundled PostgreSQL. Once it is ready, runs upstream's identity setup script in the pod. |
| `ControlPlane` | Applies ate-api-server, ate-controller, atelet, atenet router and egress, the credential provider, and their configuration. |
| `Prune` | Deletes objects an earlier spec installed that the current one no longer renders. |
| `Rollout` | Waits until every workload has rolled out, then sets `Ready`. |

Pods restart when their rendered ConfigMaps or Secrets change: each pod
template carries a hash of the configuration it reads. The operator re-applies
everything every 10 minutes, and whenever an installed workload is edited or
deleted.

## Prerequisites

- Kubernetes 1.36 or later, with the `ClusterTrustBundle`,
  `ClusterTrustBundleProjection` and `PodCertificateRequest` feature gates on,
  and `certificates.k8s.io/v1beta1` served. On GKE these can only be enabled
  when the cluster is created (see upstream's `tools/setup-gcp`).
- The Agent Substrate images. Upstream publishes none, so this project's CI
  rebuilds each embedded release from its upstream tag and publishes it,
  signed, to `ghcr.io/samueltorres/substrate`. The operator uses those by
  default. To use your own registry, build them yourself and set
  `images.repository`:

  ```bash
  hack/publish-substrate-images.sh ~/src/github.com/agent-substrate/substrate \
    registry.example.com/substrate linux/amd64,linux/arm64
  ```

  This runs upstream's `ate-setup publish release-images` on a clean copy of
  the embedded release tag. It needs Go and Docker.

## Install

```bash
helm install substrate-operator oci://ghcr.io/samueltorres/charts/agent-substrate-operator \
  --version 0.0.1 --namespace substrate-operator --create-namespace

kubectl apply -f examples/generic-s3.yaml
kubectl get substrateinstall substrate -w
```

From a checkout, install `charts/agent-substrate-operator` instead.

Don't install the operator into `ate-system`: tearing down the install deletes
that namespace.

The chart can create the `SubstrateInstall` itself, with
`--set substrateInstall.enabled=true` and the spec under
`substrateInstall.spec`.

To uninstall, delete the `SubstrateInstall` first and wait for it to go, then
uninstall the chart. The operator removes the resource's finalizer, so it has
to be running at that point. `helm uninstall` keeps a chart-created
`SubstrateInstall` for the same reason.

```bash
kubectl delete substrateinstall substrate
helm uninstall substrate-operator -n substrate-operator
```

## The SubstrateInstall

The resource must be named `substrate`, because the upstream manifests name
`ate-system` literally, so a cluster holds one install. Field names follow
upstream's `install.ate.dev/v1alpha1` install document where one exists. See
[`examples/`](examples) for kind, generic S3, and GKE with Cloud SQL.

| Field | Default | Meaning |
|---|---|---|
| `platform` | `generic` | `generic`: any cluster; drops atelet's GKE-only image credential provider mounts. `gke`: keeps them, and adds Google Managed Prometheus monitoring. `kind`: upstream's kind overlay with rustfs, an OpenTelemetry collector, Jaeger and Prometheus. |
| `images.repository` | `ghcr.io/samueltorres/substrate` | Registry path holding the component images. |
| `images.tag` | embedded release | Tag of every component image. |
| `storage.backend` | `gcs` on gke, else `s3` | Where actor snapshots go. Leave `storage` unset on kind. |
| `storage.s3` | | `region`, `endpoint`, `usePathStyle`, and `credentialsSecretName` (a Secret in `ate-system` with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`). |
| `storage.serviceAccountAnnotations` | | Set on the ate-api-server and atelet service accounts, for workload identity. |
| `postgres.connectionSecretName` | | Use an external database. Names a Secret in `ate-system` with `readWriteConnectionString`, and optionally `ownerConnectionString` and `serverCA`. |
| `postgres.cloudSQL` | | Add the Cloud SQL Auth Proxy sidecar: `instance`, `serviceAccount`, `iamAuth` (default true), `ipType` (`private`, `public`, `psc`). |
| `postgres.schema`, `readWriteRole`, `ownerRole`, `poolMaxConns` | upstream's | Settings for an external database. |
| `postgres.storageClassName` | cluster default | StorageClass of the bundled PostgreSQL volume. |
| `atenet.credentialProvider` | required | `{name: k8s.io}` deploys the bundled Kubernetes Secrets provider. `{enabled: false}` turns credential injection off. `{name, address}` points at a provider you deploy. |
| `atenet.additionalEgressExtprocService` | | `NAMESPACE/SERVICE:PORT` of an ext_proc authorization filter for the egress gateway. |
| `apiServer.expectedJWTIssuer` | discovered | Service account token issuer ate-api-server trusts. Without it the operator derives the issuer from `gcp`, else reads the cluster's OpenID discovery document, else uses `https://kubernetes.default.svc`. |
| `apiServer.authenticationConfig` | generated | Replaces `authentication.yaml`, for installs that trust more JWT providers. |
| `actorJWT.algorithm` | `ES256` | Signing algorithm of the actor identity JWT pool. It only applies when the pool is first created. |
| `actorJWT.issuer` | `https://idp.ate-system.svc` | `iss` claim of actor JWTs. Must be https, with no query or fragment. |
| `gcp` | | `projectID`, `clusterLocation`, `clusterName` of a GKE cluster. |
| `otlpEndpoint` | platform's | Where control plane telemetry goes. |
| `clusterSize` | `size0` | `size10` resizes the bundled PostgreSQL for a dedicated node and raises the podcertificate-controller's API rate limits. |
| `cordonControlPlane` | `false` | Pin the control plane to nodes labeled and tainted `ate.dev/workloadType=ate-control-plane`, and PostgreSQL to `ate.dev/workloadType=ate-postgres`. |
| `podCertificate.workersPerSigner` | controller's | Concurrent workers per signer. |
| `deletionPolicy` | `Retain` | What deleting the `SubstrateInstall` does. `Retain` leaves everything in place. `Delete` tears the install down as `ate-setup delete ate-system` does: the CRDs (so every WorkerPool), the namespaces (so the bundled PostgreSQL data and the key pools), and the trust bundles. |

Only the Envoy dataplane is supported. agentgateway is not yet.

### Status

```console
$ kubectl get substrateinstall
NAME        VERSION   READY   STEP       AGE
substrate   v0.5.0    True    Ready      6m
```

While it works, `status.step` names the step and the `Progressing` condition
says what it waits on. A failed step sets `Degraded` with the error. The
operator retries with backoff, except when the spec itself is invalid.

## Releases and images

CI publishes everything to ghcr, signed with cosign keyless signing:

| Artifact | Where | Tags |
|---|---|---|
| Operator image | `ghcr.io/samueltorres/agent-substrate-operator` | `X.Y.Z` per release, `main` and `sha-…` per push to main |
| Helm chart | `oci://ghcr.io/samueltorres/charts/agent-substrate-operator` | `X.Y.Z` per release |
| Substrate images | `ghcr.io/samueltorres/substrate/<component>` | the upstream release, e.g. `v0.5.0` |

The Substrate images are not official upstream builds. They are rebuilds of
the upstream release tag, made by upstream's own `ate-setup publish
release-images` on a native runner per architecture
([`substrate-images.yaml`](.github/workflows/substrate-images.yaml)). They
are published once per release and never overwritten. The operator image
carries SBOM and provenance attestations.

Verify a signature:

```bash
cosign verify ghcr.io/samueltorres/agent-substrate-operator:0.0.1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/samueltorres/agent-substrate-operator/\.github/workflows/'
```

### Cutting a release

Tag and push: `git tag v0.0.1 && git push origin v0.0.1`. CI tests, publishes
the image and chart, and creates the GitHub release. The tag sets the chart
version, and the chart defaults the operator image to that version, so the two
always match. `Chart.yaml` keeps `0.0.0` in the repository; when installing
the chart from a checkout, set `image.tag`.

### CI

[`ci.yaml`](.github/workflows/ci.yaml) runs on every pull request and push:

- `verify`: regenerates code, the CRD, and RBAC and fails if any is stale;
  golangci-lint, shellcheck, actionlint, helm lint; the unit and envtest
  suites; govulncheck.
- `e2e`: [`hack/e2e-kind.sh`](hack/e2e-kind.sh) on a kind cluster. It
  installs through the chart, waits for `Ready`, runs upstream's counter demo
  through suspend and resume, and checks the teardown. When ghcr has no
  Substrate images for the embedded release yet, it builds them into the kind
  registry first. Run it locally with
  `make e2e UPSTREAM=~/src/github.com/agent-substrate/substrate`.

Dependabot keeps the Go modules, the actions (pinned by commit), and the
Dockerfile's base images current.

## Development

```bash
make generate   # deepcopy, CRD, and RBAC from the API types
make test       # unit tests, and controller tests against an envtest API server
make lint       # go vet, gofmt, helm lint
make run        # run the operator against the current kubeconfig context
```

### Updating to a new Agent Substrate release

1. `hack/sync-upstream.sh <upstream checkout> <tag>` copies the release's
   manifests and PostgreSQL setup script into `internal/upstream/files`.
2. Diff upstream's `cmd/ate-setup/internal/steps` between the two tags, and
   port any install step that changed. `internal/render` and
   `internal/controller` name the upstream function each part reproduces.
3. Update `components` in `internal/render/images.go` if the published image
   set changed, and `appVersion` in the chart.
4. `make test`.

### Layout

```
api/v1alpha1/         SubstrateInstall types
cmd/operator/         operator entry point
internal/upstream/    the embedded upstream release (manifests, setup.sql, LICENSE)
internal/render/      spec -> objects: kustomize builds, image rewriting, platform transforms
internal/pki/         CA and JWT pool generation, copied from upstream
internal/controller/  the reconciler: apply order, waits, Postgres setup, prune, teardown
charts/               the operator's Helm chart and CRD
examples/             SubstrateInstall examples
hack/                 upstream sync and image publishing
```

Code copied from upstream keeps its Apache-2.0 header. Upstream's LICENSE is
at `internal/upstream/files/LICENSE`.
