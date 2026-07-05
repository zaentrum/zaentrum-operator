// Package v1alpha1 contains the API types for the zaentrum.io/v1alpha1 group.
//
// +kubebuilder:object:generate=true
// +groupName=zaentrum.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "zaentrum.io", Version: "v1alpha1"}

	// SchemeBuilder registers the Zaentrum kinds into a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Zaentrum{}, &ZaentrumList{})
}
