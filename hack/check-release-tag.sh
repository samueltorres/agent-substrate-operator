#!/usr/bin/env bash
# Fails unless a release tag is v followed by a semantic version. The release
# packages the chart with that version, and the chart defaults the operator
# image to it, so a tag Helm would reject must fail before anything is
# published. CI runs this first on a tag.
#
#   hack/check-release-tag.sh v0.0.1

set -o errexit -o nounset -o pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <tag>" >&2
  exit 2
fi
tag="$1"

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
if [[ ! "${tag}" =~ ${semver} ]]; then
  echo "tag ${tag} is not v followed by a semantic version, e.g. v0.0.1" >&2
  exit 1
fi
