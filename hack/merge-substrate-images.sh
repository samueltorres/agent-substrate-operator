#!/usr/bin/env bash
# Merges per-architecture Substrate images into one multi-architecture tag.
#
#   hack/merge-substrate-images.sh <registry/repository> <tag> <refs file>...
#
# Each refs file lists the references one architecture's
# hack/publish-substrate-images.sh run pushed, e.g.
# ghcr.io/me/substrate/ateapi:v0.5.0-amd64@sha256:.... Every image named in
# any file becomes <registry/repository>/<image>:<tag>, an index over each
# architecture's digest. Its annotations name the upstream tag it was built
# from, and this repository as the source, which ghcr links the package to. The merged references are printed with their digests, one per line,
# for signing.

set -o errexit -o nounset -o pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <registry/repository> <tag> <refs file>..." >&2
  exit 2
fi
repo="$1"
tag="$2"
shift 2

# One "<image> <digest>" line per reference, from every file.
pairs="$(
  for file in "$@"; do
    while IFS= read -r ref; do
      [[ -n "${ref}" ]] || continue
      name="${ref#"${repo}/"}"
      echo "${name%%[:@]*} ${ref##*@}"
    done <"${file}"
  done | sort -u
)"

if [[ -z "${pairs}" ]]; then
  echo "no image references found in $*" >&2
  exit 1
fi

for name in $(cut -d' ' -f1 <<<"${pairs}" | uniq); do
  sources=()
  while read -r image digest; do
    if [[ "${image}" == "${name}" ]]; then
      sources+=("${repo}/${name}@${digest}")
    fi
  done <<<"${pairs}"
  target="${repo}/${name}:${tag}"
  docker buildx imagetools create --tag "${target}" \
    --annotation "index:org.opencontainers.image.source=https://github.com/samueltorres/agent-substrate-operator" \
    --annotation "index:org.opencontainers.image.url=https://github.com/agent-substrate/substrate/tree/${tag}" \
    --annotation "index:org.opencontainers.image.version=${tag}" \
    --annotation "index:org.opencontainers.image.licenses=Apache-2.0" \
    --annotation "index:org.opencontainers.image.description=Agent Substrate ${tag} ${name}, rebuilt from the upstream tag by agent-substrate-operator" \
    "${sources[@]}" >&2
  echo "${repo}/${name}@$(docker buildx imagetools inspect "${target}" --format '{{.Manifest.Digest}}')"
done
