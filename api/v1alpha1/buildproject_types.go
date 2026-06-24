package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tier controls the scale-to-zero policy of a project's daemon.
const (
	TierHot  = "hot"  // never scaled to zero
	TierWarm = "warm" // scaled to zero after IdleTimeoutSec
	TierCold = "cold" // long tail; aggressive scale-to-zero + S3 seed
)

// SecurityProfile selects how the buildkitd pod is hardened. The default is
// rootless; the spike on the target cluster decides whether the cluster's
// admission policy (e.g. Kyverno) forces userns/privileged instead.
const (
	ProfileRootless   = "rootless"
	ProfileUserns     = "userns"
	ProfilePrivileged = "privileged"
)

// BuildProjectSpec is the cache identity and daemon lifecycle of one
// (project, arch). All builds that must share a cache MUST resolve to the same
// Key — see internal/router for the normalization that guarantees this.
type BuildProjectSpec struct {
	// Key is the stable cache identity: a truncated sha256 of the normalized
	// (repo, target, arch). Set by the router at creation; immutable.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Repo is the normalized source repository (lowercased, scheme/.git stripped).
	// Informational; the Key is what routes.
	Repo string `json:"repo,omitempty"`

	// Target is the Dockerfile target stage this daemon serves ("" => default).
	Target string `json:"target,omitempty"`

	// Arch is the build architecture.
	// +kubebuilder:validation:Enum=amd64;arm64
	Arch string `json:"arch"`

	// StorageClass for the cache PVC. On OVH MKS this is the gen2 high-speed class.
	// +kubebuilder:default="csi-cinder-high-speed-gen2"
	StorageClass string `json:"storageClass,omitempty"`

	// CacheVolumeGi sizes the cache volume. On gen2, throughput scales with size
	// (~30 IOPS/Gi + 0.5 MB/s/Gi), so size for bandwidth, not just capacity.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	CacheVolumeGi int32 `json:"cacheVolumeGi,omitempty"`

	// Tier controls scale-to-zero. hot is never scaled to zero.
	// +kubebuilder:validation:Enum=hot;warm;cold
	// +kubebuilder:default=warm
	Tier string `json:"tier,omitempty"`

	// IdleTimeoutSec is the idle window before scale-to-zero (ignored on hot).
	// Default seeded from bench B (reattach cycle ~31s p50 => generous timeout).
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=0
	IdleTimeoutSec int32 `json:"idleTimeoutSec,omitempty"`

	// SnapshotEverySec is the durability/seed snapshot cadence (0 = never). OVH supports
	// in-use snapshots, so this does not require scale-to-zero.
	// +kubebuilder:validation:Minimum=0
	SnapshotEverySec int32 `json:"snapshotEverySec,omitempty"`

	// RestoreFromSnapshot, if set, seeds the cache PVC from this VolumeSnapshot on creation
	// (DR / new cluster / lost volume). Ignored once the PVC exists.
	RestoreFromSnapshot string `json:"restoreFromSnapshot,omitempty"`

	// Fanout is the number of additional warm clone daemons for a saturated project (M5,
	// conditional — vertical first). Each clone is a sibling BuildProject seeded (CoW) from the
	// latest snapshot; layers converge via shared S3. 0 = no fan-out.
	// +kubebuilder:validation:Minimum=0
	Fanout int32 `json:"fanout,omitempty"`

	// SecurityProfile hardens the buildkitd pod.
	// +kubebuilder:validation:Enum=rootless;userns;privileged
	// +kubebuilder:default=rootless
	SecurityProfile string `json:"securityProfile,omitempty"`

	// Resources for the buildkitd container (CPU/RAM of the daemon).
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// BuildProjectStatus reflects the observed daemon state.
type BuildProjectStatus struct {
	// Phase is a coarse summary: Pending | Warm | Idle | Scaling | Failed.
	Phase string `json:"phase,omitempty"`

	// Replicas is the current desired/observed replica count (0 or 1).
	Replicas int32 `json:"replicas"`

	// Endpoint is the mTLS address clients dial (tcp://svc.ns.svc:1234).
	Endpoint string `json:"endpoint,omitempty"`

	// VolumeGen is bumped on promote (M5). The canonical cache lineage.
	VolumeGen int32 `json:"volumeGen,omitempty"`

	// LastBuildTime is when a build last touched this daemon (drives idle).
	LastBuildTime *metav1.Time `json:"lastBuildTime,omitempty"`

	// LastSnapshot is the name of the most recent VolumeSnapshot (M3).
	LastSnapshot string `json:"lastSnapshot,omitempty"`

	// InflightBuilds is the number of builds currently routed to this daemon.
	InflightBuilds int32 `json:"inflightBuilds"`

	// Conditions follow the standard k8s condition convention (Ready, Degraded).
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bp;buildproj
// +kubebuilder:printcolumn:name="Arch",type=string,JSONPath=`.spec.arch`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BuildProject is the cache identity + lifecycle of one hot buildkitd.
type BuildProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildProjectSpec   `json:"spec,omitempty"`
	Status BuildProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BuildProjectList contains a list of BuildProject.
type BuildProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BuildProject `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BuildProject{}, &BuildProjectList{})
}
