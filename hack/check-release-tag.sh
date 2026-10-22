#!/usr/bin/env bash
# Fails unless a release tag matches the chart version. The chart defaults the
# operator image to its version, so a mismatched tag would publish an image the
# chart never installs. CI runs this first on a tag, before publishing anything.
#
#   hack/check-release-tag.sh v0.1.0

set -o errexit -o nounset -o pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <tag>" >&2
  exit 2
fi
tag="$1"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(sed -n 's/^version: *//p' "${root}/charts/agent-substrate-operator/Chart.yaml")"
if [[ "${tag}" != "v${version}" ]]; then
  echo "tag ${tag} does not match the chart version ${version}; set version in charts/agent-substrate-operator/Chart.yaml and tag v${version}" >&2
  exit 1
fi
