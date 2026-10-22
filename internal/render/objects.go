package render

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Object is one rendered Kubernetes object.
type Object = unstructured.Unstructured

// decode splits a multi-document manifest into objects, skipping empty
// documents.
func decode(manifest []byte) ([]*Object, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var objs []*Object
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return objs, nil
			}
			return nil, fmt.Errorf("while decoding a manifest: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		objs = append(objs, &Object{Object: raw})
	}
}

// describe names an object for errors.
func describe(obj *Object) string {
	if ns := obj.GetNamespace(); ns != "" {
		return fmt.Sprintf("%s %s/%s", obj.GetKind(), ns, obj.GetName())
	}
	return fmt.Sprintf("%s %s", obj.GetKind(), obj.GetName())
}

// find returns the object of kind and name, or an error naming what is
// missing. Every transform targets an object the embedded release ships, so
// a miss means the manifests moved under it.
func find(objs []*Object, kind, name string) (*Object, error) {
	for _, obj := range objs {
		if obj.GetKind() == kind && obj.GetName() == name {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("the rendered manifests have no %s %s", kind, name)
}

// podSpecPath is where a workload keeps its pod spec.
var podSpecPath = []string{"spec", "template", "spec"}

// isWorkload reports whether obj carries a pod template that can roll. A Job's
// template is immutable, so it is not one.
func isWorkload(obj *Object) bool {
	switch obj.GetKind() {
	case "Deployment", "DaemonSet", "StatefulSet":
		return true
	}
	return false
}

// mutateContainer applies fn to the named container in the pod spec's list
// field ("containers" or "initContainers").
func mutateContainer(obj *Object, field, name string, fn func(c map[string]any) error) error {
	path := append(append([]string{}, podSpecPath...), field)
	list, _, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil {
		return fmt.Errorf("%s: %w", describe(obj), err)
	}
	for i, item := range list {
		c, ok := item.(map[string]any)
		if !ok || c["name"] != name {
			continue
		}
		if err := fn(c); err != nil {
			return fmt.Errorf("%s container %s: %w", describe(obj), name, err)
		}
		list[i] = c
		return unstructured.SetNestedSlice(obj.Object, list, path...)
	}
	return fmt.Errorf("%s has no %s %s", describe(obj), field, name)
}

// setEnv sets an environment variable on a container, replacing one of the
// same name.
func setEnv(c map[string]any, name, value string) {
	env, _ := c["env"].([]any)
	entry := map[string]any{"name": name, "value": value}
	for i, e := range env {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			env[i] = entry
			c["env"] = env
			return
		}
	}
	c["env"] = append(env, entry)
}

// addEnvFromSecret appends a secretRef envFrom source to a container.
func addEnvFromSecret(c map[string]any, secret string) {
	envFrom, _ := c["envFrom"].([]any)
	c["envFrom"] = append(envFrom, map[string]any{"secretRef": map[string]any{"name": secret}})
}

// removeByName drops the entries of a list field whose name is in names.
func removeByName(m map[string]any, field string, names ...string) {
	list, _ := m[field].([]any)
	kept := list[:0]
	for _, item := range list {
		if e, ok := item.(map[string]any); ok && containsString(names, e["name"]) {
			continue
		}
		kept = append(kept, item)
	}
	m[field] = kept
}

func containsString(list []string, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// setAnnotations merges annotations into an object's metadata.
func setAnnotations(obj *Object, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	merged := obj.GetAnnotations()
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range annotations {
		merged[k] = v
	}
	obj.SetAnnotations(merged)
}

// setPodAnnotation sets an annotation on a workload's pod template.
func setPodAnnotation(obj *Object, key, value string) error {
	return unstructured.SetNestedField(obj.Object, value, "spec", "template", "metadata", "annotations", key)
}
