#!/usr/bin/env bash
# Builds the Agent Substrate release the operator embeds and pushes its images
# to a registry, where a SubstrateInstall's images.repository points. Upstream
# publishes no images, so an install needs this once per release. It uses
# upstream's own `ate-setup publish release-images`, which needs Go and Docker.
#
#   hack/publish-substrate-images.sh <upstream checkout> <registry/repository> [platforms]
#
#   hack/publish-substrate-images.sh ~/src/github.com/agent-substrate/substrate \
#     ghcr.io/me/substrate linux/amd64,linux/arm64
#
# TAG overrides the tag the images get (default: the embedded release), as CI
# does to push one tag per architecture before merging them. REFS_FILE names a
# file to write the pushed references to, one per line.
#
# The envoy-dataplane image is pushed by docker buildx from the active builder.
# A docker-container builder cannot reach a registry on the host's localhost;
# use the default builder for one.

set -o errexit -o nounset -o pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: $0 <upstream checkout> <registry/repository> [platforms]" >&2
  exit 2
fi
upstream="$(cd "$1" && pwd)"
repo="$2"
platforms="${3:-linux/amd64,linux/arm64}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(cat "${root}/internal/upstream/files/VERSION")"
tag="${TAG:-${version}}"

src="$(mktemp -d)"
out="$(mktemp)"
trap 'rm -rf "${src}" "${out}"' EXIT

# A clean tree at the release tag, so local changes in the checkout cannot
# reach the images.
git -C "${upstream}" archive "${version}" | tar -x -C "${src}"

(
  cd "${src}"
  VERSION="${tag}" go run ./cmd/ate-setup --no-dev-env publish release-images \
    --ko-docker-repo "${repo}" --ko-default-platforms "${platforms}"
) | tee "${out}"

if [[ -n "${REFS_FILE:-}" ]]; then
  # ate-setup ends with the pushed references, one per line.
  grep "^${repo}/" "${out}" >"${REFS_FILE}"
fi
