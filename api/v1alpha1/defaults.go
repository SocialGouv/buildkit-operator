package v1alpha1

// Default values for BuildProjectSpec. These MUST equal the +kubebuilder:default markers in
// buildproject_types.go (markers only accept literals, so the two can't share a symbol). The two are
// kept in lockstep by TestDefaultsMatchMarkers, which parses the markers and asserts ApplyDefaults
// reproduces them — so a marker change without a constant change (or vice-versa) fails the build.
const (
	DefaultStorageClass    = "csi-cinder-high-speed-gen2"
	DefaultCacheVolumeGi   = 60
	DefaultTier            = TierWarm
	DefaultIdleTimeoutSec  = 900
	DefaultSecurityProfile = ProfileRootless
)

// ApplyDefaults fills unset spec fields with their defaults. The apiserver applies the
// +kubebuilder:default markers for objects created through the API, so this is the safety net for
// objects that never round-trip through it: the fake client in tests and specs built in-process.
// Without it, an undefaulted warm project would scale to zero immediately after every build
// (desiredReplicas skips the LastBuildTime window when IdleTimeoutSec==0).
func (bp *BuildProject) ApplyDefaults() {
	s := &bp.Spec
	if s.StorageClass == "" {
		s.StorageClass = DefaultStorageClass
	}
	if s.CacheVolumeGi == 0 {
		s.CacheVolumeGi = DefaultCacheVolumeGi
	}
	if s.Tier == "" {
		s.Tier = DefaultTier
	}
	if s.IdleTimeoutSec == 0 {
		s.IdleTimeoutSec = DefaultIdleTimeoutSec
	}
	if s.SecurityProfile == "" {
		s.SecurityProfile = DefaultSecurityProfile
	}
}
