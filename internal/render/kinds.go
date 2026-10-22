package render

import (
	"io/fs"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

// ManagedKinds lists every kind of object any variant of the install can
// render, so pruning and teardown can find what an earlier spec applied. It
// leaves out Namespaces and CRDs, which go by name, and PersistentVolumeClaims,
// which hold data.
func ManagedKinds() ([]schema.GroupVersionKind, error) {
	seen := map[schema.GroupVersionKind]bool{
		{Version: "v1", Kind: "ConfigMap"}: true,
		{Version: "v1", Kind: "Secret"}:    true,
	}
	err := fs.WalkDir(upstream.FS(), "manifests", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".yaml" {
			return err
		}
		raw, err := fs.ReadFile(upstream.FS(), p)
		if err != nil {
			return err
		}
		objs, err := decode(raw)
		if err != nil {
			return err
		}
		for _, obj := range objs {
			gvk := obj.GroupVersionKind()
			if gvk.Kind == "" || strings.HasPrefix(gvk.Group, "kustomize.config.k8s.io") {
				continue
			}
			switch gvk.Kind {
			case "Namespace", "CustomResourceDefinition", "PersistentVolumeClaim":
				continue
			}
			seen[gvk] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	kinds := make([]schema.GroupVersionKind, 0, len(seen))
	for gvk := range seen {
		kinds = append(kinds, gvk)
	}
	slices.SortFunc(kinds, func(a, b schema.GroupVersionKind) int { return strings.Compare(a.String(), b.String()) })
	return kinds, nil
}
