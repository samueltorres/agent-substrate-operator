package render

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

// srcRoot is where the embedded upstream tree sits in the in-memory
// filesystem kustomize builds from.
const srcRoot = "/src"

// kustomizer builds upstream kustomizations from an in-memory copy of the
// embedded tree, standing in for upstream's
// `kubectl kustomize --load-restrictor LoadRestrictionsNone`.
type kustomizer struct {
	fs      filesys.FileSystem
	scratch int
}

func newKustomizer() (*kustomizer, error) {
	mem := filesys.MakeFsInMemory()
	err := fs.WalkDir(upstream.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(upstream.FS(), p)
		if err != nil {
			return err
		}
		return mem.WriteFile(path.Join(srcRoot, p), data)
	})
	if err != nil {
		return nil, fmt.Errorf("while staging the upstream manifests: %w", err)
	}
	return &kustomizer{fs: mem}, nil
}

// build renders a kustomization directory.
func (k *kustomizer) build(dir string) ([]byte, error) {
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone
	resMap, err := krusty.MakeKustomizer(opts).Run(k.fs, dir)
	if err != nil {
		return nil, fmt.Errorf("while building the kustomization at %s: %w", dir, err)
	}
	return resMap.AsYaml()
}

// upstreamPath is the in-memory path of a file or directory in the
// upstream tree.
func upstreamPath(rel ...string) string {
	return path.Join(append([]string{srcRoot}, rel...)...)
}

// compose renders manifest bytes with the given upstream component
// directories layered on top, as upstream's kustomize.ComposeBytes does.
func (k *kustomizer) compose(manifest []byte, components ...string) ([]byte, error) {
	k.scratch++
	dir := fmt.Sprintf("/scratch/%d", k.scratch)
	if err := k.fs.WriteFile(path.Join(dir, "manifest.yaml"), manifest); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manifest.yaml\ncomponents:\n")
	for _, c := range components {
		fmt.Fprintf(&b, "  - ../..%s\n", c)
	}
	if err := k.fs.WriteFile(path.Join(dir, "kustomization.yaml"), []byte(b.String())); err != nil {
		return nil, err
	}
	return k.build(dir)
}

// readUpstream returns an upstream file as committed.
func readUpstream(rel ...string) ([]byte, error) {
	return fs.ReadFile(upstream.FS(), path.Join(rel...))
}
