// Package v1alpha1 holds the SubstrateInstall API.
//
// +kubebuilder:object:generate=true
// +groupName=substrate.samueltorres.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the SubstrateInstall API.
	GroupVersion = schema.GroupVersion{Group: "substrate.samueltorres.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers the API types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &SubstrateInstall{}, &SubstrateInstallList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
