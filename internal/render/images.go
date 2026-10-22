package render

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// koModulePath is the upstream module path the embedded manifests name images
// by, as ko:// import paths.
const koModulePath = "github.com/agent-substrate/substrate"

// components are the packages upstream publishes images for at the embedded
// release (cmd/ate-setup/internal/images.Components). A reference outside it
// fails the render rather than guessing at an image that was never pushed.
var components = []string{
	"cmd/ateapi",
	"cmd/atecontroller",
	"cmd/atelet",
	"cmd/atenet",
	"cmd/ateom-gvisor",
	"cmd/ateom-microvm",
	"cmd/credential-provider/kubernetes-secrets",
	"cmd/objectstore-plugin",
	"cmd/podcertcontroller",
}

// envoyDataplaneImage is built from a Dockerfile rather than with ko, so the
// manifests name it by placeholder. Upstream publishes it beside the rest.
const (
	envoyDataplaneImage       = "envoy-dataplane"
	envoyDataplanePlaceholder = "${ENVOY_DATAPLANE_IMAGE}"
)

var koRef = regexp.MustCompile(`ko://[^\s"',\]}]+`)

// images rewrites image references to the published release.
type images struct {
	repository string
	tag        string
}

// ref is the reference for an image name, the last element of its package
// path, which is how ko publishes with --base-import-paths.
func (i images) ref(name string) string {
	return strings.TrimSuffix(i.repository, "/") + "/" + name + ":" + i.tag
}

// resolve replaces every ko:// reference and the envoy dataplane placeholder.
// Substitution is textual, as upstream's Prebuilt resolver does: references
// also appear outside pod specs.
func (i images) resolve(manifest []byte) ([]byte, error) {
	var unknown []string
	out := koRef.ReplaceAllFunc(manifest, func(match []byte) []byte {
		pkg, ok := strings.CutPrefix(string(match), "ko://"+koModulePath+"/")
		if !ok || !slices.Contains(components, pkg) {
			unknown = append(unknown, string(match))
			return match
		}
		return []byte(i.ref(path.Base(pkg)))
	})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no published image for %s", strings.Join(slices.Compact(unknown), ", "))
	}
	return bytes.ReplaceAll(out, []byte(envoyDataplanePlaceholder), []byte(i.ref(envoyDataplaneImage))), nil
}
