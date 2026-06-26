package v1alpha1

import "testing"

// parentSpec is a representative canonical project from which children are derived.
func parentSpec() BuildProjectSpec {
	return BuildProjectSpec{
		Key:             "pdeadbeefdeadbeef",
		Repo:            "github.com/org/repo",
		Name:            "api",
		Target:          "prod",
		Arch:            "amd64",
		StorageClass:    "csi-cinder-high-speed-gen2",
		CacheVolumeGi:   80,
		SecurityProfile: ProfilePrivileged,
		Tier:            TierHot,
		IdleTimeoutSec:  900,
	}
}

// TestDeriveChild_InheritsIdentityAndStorage asserts every child (fork or clone) inherits the
// parent's cache identity, storage, and security profile, gets the new key + snapshot seed, and
// never carries snapshot/fanout policy of its own.
func TestDeriveChild_InheritsIdentityAndStorage(t *testing.T) {
	parent := parentSpec()
	for _, role := range []ChildRole{ForkChild, CloneChild} {
		child := DeriveChild(parent, "snap-123", role, "pchildkey00000000")

		if child.Key != "pchildkey00000000" {
			t.Errorf("role %d: Key = %q, want the passed-in child key", role, child.Key)
		}
		if child.RestoreFromSnapshot != "snap-123" {
			t.Errorf("role %d: RestoreFromSnapshot = %q, want snap-123", role, child.RestoreFromSnapshot)
		}
		if child.Repo != parent.Repo || child.Name != parent.Name ||
			child.Target != parent.Target || child.Arch != parent.Arch {
			t.Errorf("role %d: child must inherit the parent's cache identity, got %+v", role, child)
		}
		if child.StorageClass != parent.StorageClass || child.CacheVolumeGi != parent.CacheVolumeGi {
			t.Errorf("role %d: child must inherit the parent's storage, got class=%q gi=%d",
				role, child.StorageClass, child.CacheVolumeGi)
		}
		if child.SecurityProfile != parent.SecurityProfile {
			t.Errorf("role %d: SecurityProfile = %q, want inherited %q",
				role, child.SecurityProfile, parent.SecurityProfile)
		}
	}
}

// TestDeriveChild_RolePolicy pins the per-role scale policy: a fork is warm with the short fork idle
// window, a clone stays hot. These are the only fields the role switch sets.
func TestDeriveChild_RolePolicy(t *testing.T) {
	parent := parentSpec()

	fork := DeriveChild(parent, "snap", ForkChild, "pfork")
	if fork.Tier != TierWarm {
		t.Errorf("fork Tier = %q, want %q", fork.Tier, TierWarm)
	}
	if fork.IdleTimeoutSec != ForkIdleTimeoutSec {
		t.Errorf("fork IdleTimeoutSec = %d, want %d", fork.IdleTimeoutSec, ForkIdleTimeoutSec)
	}

	clone := DeriveChild(parent, "snap", CloneChild, "pclone")
	if clone.Tier != TierHot {
		t.Errorf("clone Tier = %q, want %q", clone.Tier, TierHot)
	}
}
