#!/usr/bin/env bash
# End-to-end test on kind: installs the operator through its chart, waits for
# the SubstrateInstall to become Ready, runs upstream's counter demo through
# suspend and resume, then deletes the SubstrateInstall and checks the
# teardown.
#
#   hack/e2e-kind.sh <upstream checkout>
#
# The cluster is made with upstream's hack/create-kind-cluster.sh at the
# embedded release, with the feature gates Substrate needs and a registry on
# localhost:5001. Substrate's images come from SUBSTRATE_IMAGES (default
# ghcr.io/samueltorres/substrate); if that repository has no images for the
# release, they are built into the kind registry instead.
#
# Environment:
#   KIND_CLUSTER_NAME  cluster to create (default substrate-e2e). It is
#                      replaced if it exists, and deleted at the end unless
#                      KEEP_CLUSTER=1.
#   SUBSTRATE_IMAGES   repository holding the Substrate images.

set -o errexit -o nounset -o pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <upstream checkout>" >&2
  exit 2
fi
upstream="$(cd "$1" && pwd)"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(cat "${root}/internal/upstream/files/VERSION")"
cluster="${KIND_CLUSTER_NAME:-substrate-e2e}"
context="kind-${cluster}"
images="${SUBSTRATE_IMAGES:-ghcr.io/samueltorres/substrate}"
registry="localhost:5001"
operator_image="${registry}/agent-substrate-operator:e2e"
arch="$(go env GOARCH)"
kubectl=(kubectl --context "${context}")

# A docker-container buildx builder cannot push to the kind registry on the
# host's localhost, so this script builds with the docker driver's builder,
# which is named after the current docker context.
BUILDX_BUILDER="${BUILDX_BUILDER:-$(docker context show)}"
export BUILDX_BUILDER

work="$(mktemp -d)"
port_forward=""
cleanup() {
  status=$?
  [[ -n "${port_forward}" ]] && kill "${port_forward}" 2>/dev/null
  if [[ ${status} -ne 0 ]]; then
    diagnostics || true
  fi
  if [[ "${KEEP_CLUSTER:-}" != "1" ]]; then
    (cd "${work}/upstream" && KIND_CLUSTER_NAME="${cluster}" hack/delete-kind-cluster.sh) >/dev/null 2>&1 || true
  fi
  rm -rf "${work}"
  exit "${status}"
}
trap cleanup EXIT

step() { printf '\n==> %s\n' "$*"; }

diagnostics() {
  step "diagnostics"
  "${kubectl[@]}" get substrateinstall substrate -o yaml
  "${kubectl[@]}" get pods -A -o wide
  "${kubectl[@]}" -n substrate-operator logs deploy/substrate-operator --tail=200
  for pod in $("${kubectl[@]}" -n ate-system get pods -o name); do
    echo "--- ${pod}"
    "${kubectl[@]}" -n ate-system logs "${pod}" --all-containers --tail=50
  done
}

step "extracting upstream ${version}"
mkdir "${work}/upstream"
git -C "${upstream}" archive "${version}" | tar -x -C "${work}/upstream"
# Upstream's hack/kind.sh finds its tools from the git root.
git -C "${work}/upstream" init -q

step "creating kind cluster ${cluster}"
(cd "${work}/upstream" && KIND_CLUSTER_NAME="${cluster}" hack/create-kind-cluster.sh)

if ! docker buildx imagetools inspect "${images}/ateapi:${version}" >/dev/null 2>&1; then
  step "no ${version} images in ${images}; building them into the kind registry"
  images="${registry}/substrate"
  "${root}/hack/publish-substrate-images.sh" "${upstream}" "${images}" "linux/${arch}"
fi

step "building the operator"
docker buildx build --platform "linux/${arch}" --load -t "${operator_image}" "${root}"
docker push "${operator_image}"

step "installing the chart"
cat >"${work}/values.yaml" <<EOF
image:
  repository: ${operator_image%:*}
  tag: ${operator_image##*:}
  pullPolicy: Always
substrateInstall:
  enabled: true
  spec:
    platform: kind
    images:
      repository: ${images}
    atenet:
      credentialProvider:
        name: k8s.io
    deletionPolicy: Delete
EOF
helm --kube-context "${context}" upgrade --install substrate-operator "${root}/charts/agent-substrate-operator" \
  --namespace substrate-operator --create-namespace --values "${work}/values.yaml" --wait

step "waiting for the SubstrateInstall to become Ready"
"${kubectl[@]}" wait substrateinstall/substrate --for=condition=Ready --timeout=20m

step "deploying the counter demo"
(cd "${work}/upstream" && go run ./cmd/ate-setup --no-dev-env --kind --kind-cluster-name "${cluster}" \
  --image-repo "${images}" --image-tag "${version}" --rollout-timeout 5m deploy demo counter)
(cd "${work}/upstream" && go build -o "${work}/kubectl-ate" ./cmd/kubectl-ate)
kubectl_ate=("${work}/kubectl-ate" --context "${context}")
"${kubectl_ate[@]}" create actor e2e -a ate-demo-counter --template counter

"${kubectl[@]}" -n ate-system port-forward svc/atenet-router 18000:80 >/dev/null 2>&1 &
port_forward=$!
sleep 3

# expect_count sends one request and checks the counter the actor reports.
expect_count() {
  local got
  got="$(curl -sS --fail -m 120 -X POST -H "ate-target-actor: ate-demo-counter/e2e" http://localhost:18000/)"
  echo "${got}"
  if [[ "${got}" != *"preserved memory count: $1 |"* || "${got}" != *"preserved rootfs counter: $1"* ]]; then
    echo "want count $1" >&2
    return 1
  fi
}

step "resuming the actor on request"
expect_count 1
expect_count 2

step "suspending the actor and resuming it from its snapshot"
"${kubectl_ate[@]}" suspend actor e2e -a ate-demo-counter
expect_count 3

kill "${port_forward}"
wait "${port_forward}" 2>/dev/null || true
port_forward=""

step "deleting the SubstrateInstall"
"${kubectl[@]}" delete substrateinstall substrate --timeout=10m
for ns in ate-system podcertificate-controller-system; do
  if "${kubectl[@]}" get namespace "${ns}" >/dev/null 2>&1; then
    echo "namespace ${ns} survived the teardown" >&2
    exit 1
  fi
done
if "${kubectl[@]}" get crd workerpools.ate.dev >/dev/null 2>&1; then
  echo "CRD workerpools.ate.dev survived the teardown" >&2
  exit 1
fi

step "e2e passed"
