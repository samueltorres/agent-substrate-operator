#!/usr/bin/env bash
# Packages the operator chart for a release tag and pushes it to an OCI
# registry. It refuses a tag that does not match the chart version (see
# hack/check-release-tag.sh).
#
#   hack/release-chart.sh <tag> <oci repository>
#
#   hack/release-chart.sh v0.0.1 oci://ghcr.io/samueltorres/charts
#
# The package is left in dist/, and the pushed reference is printed with its
# digest, for signing.

set -o errexit -o nounset -o pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <tag> <oci repository>" >&2
  exit 2
fi
tag="$1"
repo="$2"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root}/charts/agent-substrate-operator"
"${root}/hack/check-release-tag.sh" "${tag}"
version="${tag#v}"

mkdir -p "${root}/dist"
helm package "${chart}" --destination "${root}/dist" >&2
out="$(helm push "${root}/dist/agent-substrate-operator-${version}.tgz" "${repo}" 2>&1)"
echo "${out}" >&2
digest="$(sed -n 's/^Digest: *//p' <<<"${out}")"
if [[ -z "${digest}" ]]; then
  echo "helm push printed no digest" >&2
  exit 1
fi
echo "${repo#oci://}/agent-substrate-operator@${digest}"
