#!/usr/bin/env bash
# Copies the upstream Agent Substrate install manifests and PostgreSQL setup
# script at a release tag into internal/upstream, where the operator embeds
# them.
#
#   hack/sync-upstream.sh ~/src/github.com/agent-substrate/substrate v0.5.0

set -o errexit -o nounset -o pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <upstream checkout> <tag>" >&2
  exit 2
fi
upstream="$1"
tag="$2"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest="${root}/internal/upstream/files"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

git -C "${upstream}" archive "${tag}" \
  manifests/ate-install \
  manifests/egress-credential-injection \
  pkg/postgressetup/setup.sql \
  LICENSE |
  tar -x -C "${tmp}"

rm -rf "${dest}"
mkdir -p "${dest}"
cp -R "${tmp}/manifests" "${dest}/manifests"
cp "${tmp}/pkg/postgressetup/setup.sql" "${dest}/setup.sql"
cp "${tmp}/LICENSE" "${dest}/LICENSE"
echo "${tag}" >"${dest}/VERSION"

echo "synced ${tag} into ${dest}"
