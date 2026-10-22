package render

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
)

func TestManagedKindsCoverTheRender(t *testing.T) {
	kinds, err := ManagedKinds()
	if err != nil {
		t.Fatal(err)
	}
	managed := map[schema.GroupVersionKind]bool{}
	for _, k := range kinds {
		managed[k] = true
	}
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformGeneric, v1alpha1.PlatformKind} {
		spec := baseSpec()
		spec.Platform = platform
		if platform == v1alpha1.PlatformKind {
			spec.Storage = v1alpha1.StorageSpec{}
		}
		b := mustRender(t, spec, nil)
		for _, obj := range append(b.All(), b.CreateOnly...) {
			switch obj.GetKind() {
			case "Namespace", "CustomResourceDefinition", "PersistentVolumeClaim":
				continue
			}
			if !managed[obj.GroupVersionKind()] {
				t.Errorf("%s (%s) is rendered but not in ManagedKinds", describe(obj), obj.GroupVersionKind())
			}
		}
	}
}
