// Package v1alpha1 contains the buildcat API types.
//
// buildcat runs one hot, vanilla buildkitd per (project, arch) on Kubernetes.
// A BuildProject is the cache identity + lifecycle handle for one such daemon.
//
// +kubebuilder:object:generate=true
// +groupName=buildcat.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "buildcat.dev", Version: "v1alpha1"}

var (
	// SchemeBuilder registers the API types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the API types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
