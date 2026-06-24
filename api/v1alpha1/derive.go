package v1alpha1

// ForkIdleTimeoutSec is the short idle window for ephemeral fork-PR daemons: they serve a one-off
// untrusted build, not a warm project, so they scale to zero fast (vs the canonical default).
const ForkIdleTimeoutSec = 300

// ChildRole is the kind of derived child daemon a project can spawn.
type ChildRole int

const (
	// ForkChild is an ephemeral untrusted fork-PR daemon: seeded read-only from the parent's
	// snapshot, no write-back, short idle window (anti cache-poisoning).
	ForkChild ChildRole = iota
	// CloneChild is a fan-out CoW clone for a saturated project: stays hot, seeded from the snapshot.
	CloneChild
)

// DeriveChild builds the spec for a child daemon derived from a parent project under the given key,
// seeded from the parent's latest snapshot. It is the single source of the "spawn a child daemon"
// policy so the fork path (buildd /route) and the clone path (the reconciler) can never drift.
// Children never snapshot or fan out themselves; they inherit the parent's storage + security profile.
func DeriveChild(parent BuildProjectSpec, parentSnapshot string, role ChildRole, key string) BuildProjectSpec {
	child := BuildProjectSpec{
		Key:                 key,
		Repo:                parent.Repo,
		Target:              parent.Target,
		Arch:                parent.Arch,
		StorageClass:        parent.StorageClass,
		CacheVolumeGi:       parent.CacheVolumeGi,
		SecurityProfile:     parent.SecurityProfile,
		RestoreFromSnapshot: parentSnapshot,
	}
	switch role {
	case ForkChild:
		child.Tier = TierWarm
		child.IdleTimeoutSec = ForkIdleTimeoutSec
	case CloneChild:
		child.Tier = TierHot
	}
	return child
}
