package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildSpec is a single build run. Optional in M1 (the CLI can route + build
// without recording a Build), but it gives audit / async / observability from M2.
type BuildSpec struct {
	// ProjectKey is the BuildProject.Spec.Key this run routes to.
	// +kubebuilder:validation:MinLength=1
	ProjectKey string `json:"projectKey"`

	// Source is the build context reference (git URL, tarball URL, etc.).
	Source string `json:"source,omitempty"`

	// Dockerfile path within the context.
	Dockerfile string `json:"dockerfile,omitempty"`

	// Target Dockerfile stage.
	Target string `json:"target,omitempty"`

	// Platforms to build for (e.g. linux/amd64).
	Platforms []string `json:"platforms,omitempty"`

	// Output destination (e.g. push=registry/image:tag, or local).
	Output string `json:"output,omitempty"`

	// Untrusted marks fork-PR builds: routed to an ephemeral daemon seeded from a
	// read-only clone, with no write-back to the canonical cache (anti cache-poisoning, M4).
	Untrusted bool `json:"untrusted,omitempty"`
}

// BuildStatus reflects the observed build state.
type BuildStatus struct {
	// Phase: Pending | Routing | Building | Succeeded | Failed.
	Phase string `json:"phase,omitempty"`

	// Endpoint the build was routed to.
	Endpoint string `json:"endpoint,omitempty"`

	// ImageDigest of the produced image (when pushed).
	ImageDigest string `json:"imageDigest,omitempty"`

	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// CacheStats summarizes layer + cache-mount hit/miss for this run.
	CacheStats string `json:"cacheStats,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectKey`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Build is one build run routed to a BuildProject's daemon.
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BuildList contains a list of Build.
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Build{}, &BuildList{})
}
