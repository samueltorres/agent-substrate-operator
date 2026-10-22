package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// stampConfigHashes sets AnnotationConfigHash on every workload's pod
// template, digesting the rendered ConfigMaps and Secrets it reads through
// envFrom or volumes. Kubernetes rolls no pods when those change, and
// upstream's installer restarts workloads by hand for the same reason.
// Sources the render does not produce, such as the pool Secrets, are left
// out: the operator never rewrites them.
func stampConfigHashes(objs []*Object) error {
	sources := map[string]*Object{}
	for _, obj := range objs {
		if k := obj.GetKind(); k == "ConfigMap" || k == "Secret" {
			sources[k+"/"+obj.GetName()] = obj
		}
	}
	for _, obj := range objs {
		if !isWorkload(obj) {
			continue
		}
		refs, err := configRefs(obj)
		if err != nil {
			return err
		}
		h := sha256.New()
		found := false
		for _, ref := range refs {
			src, ok := sources[ref]
			if !ok {
				continue
			}
			found = true
			data, err := json.Marshal([]any{src.Object["data"], src.Object["binaryData"]})
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s=%s\n", ref, data)
		}
		if !found {
			continue
		}
		if err := setPodAnnotation(obj, AnnotationConfigHash, hex.EncodeToString(h.Sum(nil))); err != nil {
			return fmt.Errorf("%s: %w", describe(obj), err)
		}
	}
	return nil
}

// configRefs lists the "Kind/name" of each ConfigMap and Secret a workload's
// pods read, sorted.
func configRefs(obj *Object) ([]string, error) {
	podSpec, _, err := unstructured.NestedMap(obj.Object, podSpecPath...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe(obj), err)
	}
	refs := map[string]bool{}
	add := func(kind string, m any, key string) {
		if mm, ok := m.(map[string]any); ok {
			if name, ok := mm[key].(string); ok {
				refs[kind+"/"+name] = true
			}
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		list, _ := podSpec[field].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			envFrom, _ := cm["envFrom"].([]any)
			for _, src := range envFrom {
				s, _ := src.(map[string]any)
				add("ConfigMap", s["configMapRef"], "name")
				add("Secret", s["secretRef"], "name")
			}
		}
	}
	volumes, _ := podSpec["volumes"].([]any)
	for _, v := range volumes {
		vm, _ := v.(map[string]any)
		add("ConfigMap", vm["configMap"], "name")
		add("Secret", vm["secret"], "secretName")
		if projected, ok := vm["projected"].(map[string]any); ok {
			sources, _ := projected["sources"].([]any)
			for _, s := range sources {
				sm, _ := s.(map[string]any)
				add("ConfigMap", sm["configMap"], "name")
				add("Secret", sm["secret"], "name")
			}
		}
	}
	return slices.Sorted(maps.Keys(refs)), nil
}

// label marks objects as managed by the operator. Only installed objects get
// LabelInstall, which pruning and teardown select on.
func label(objs []*Object, install bool) {
	for _, obj := range objs {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelManagedBy] = ManagedBy
		if install {
			labels[LabelInstall] = "substrate"
		}
		obj.SetLabels(labels)
	}
}
