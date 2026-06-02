// Package v1alpha1 contains the API types for the stube.io/v1alpha1 group.
//
// +kubebuilder:object:generate=true
// +groupName=stube.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "stube.io", Version: "v1alpha1"}

	// SchemeBuilder registers the Stube kinds into a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Stube{}, &StubeList{})
}
