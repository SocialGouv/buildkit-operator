package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
)

func TestPhaseFrom(t *testing.T) {
	cases := []struct {
		desired, ready int32
		want           string
	}{
		{0, 0, "Idle"},
		{0, 1, "Idle"}, // desired 0 wins even if a stale ready replica lingers
		{1, 1, "Warm"},
		{1, 0, "Scaling"},
	}
	for _, c := range cases {
		if got := phaseFrom(c.desired, c.ready); got != c.want {
			t.Errorf("phaseFrom(%d,%d) = %q, want %q", c.desired, c.ready, got, c.want)
		}
	}
}

func TestIdleRecheckInterval(t *testing.T) {
	// Small idle window: the /6 value is below the 30s floor, so it clamps to 30s.
	small := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{IdleTimeoutSec: 60}}
	if got := idleRecheckInterval(small); got != 30*time.Second {
		t.Errorf("small idle = %v, want 30s floor", got)
	}
	// Large idle window: idle/6 dominates.
	large := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{IdleTimeoutSec: 600}}
	if got := idleRecheckInterval(large); got != 100*time.Second {
		t.Errorf("large idle = %v, want 100s", got)
	}
}

func TestMaxBuildAge(t *testing.T) {
	if got := (&BuildProjectReconciler{}).maxBuildAge(); got != 2*time.Hour {
		t.Errorf("default maxBuildAge = %v, want 2h", got)
	}
	if got := (&BuildProjectReconciler{MaxBuildSeconds: 60}).maxBuildAge(); got != time.Minute {
		t.Errorf("maxBuildAge(60) = %v, want 1m", got)
	}
}

func TestBoolToCond(t *testing.T) {
	if boolToCond(true) != metav1.ConditionTrue {
		t.Error("boolToCond(true) != ConditionTrue")
	}
	if boolToCond(false) != metav1.ConditionFalse {
		t.Error("boolToCond(false) != ConditionFalse")
	}
}

func TestNewestSnapshot(t *testing.T) {
	if newestSnapshot(nil) != nil {
		t.Error("newestSnapshot(nil) should be nil")
	}
	mk := func(name string, ageMin int) volumesnapshotv1.VolumeSnapshot {
		return volumesnapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				CreationTimestamp: metav1.Time{Time: time.Unix(int64(1000-ageMin*60), 0)},
			},
		}
	}
	items := []volumesnapshotv1.VolumeSnapshot{mk("old", 10), mk("newest", 0), mk("mid", 5)}
	if got := newestSnapshot(items); got == nil || got.Name != "newest" {
		t.Errorf("newestSnapshot picked %v, want newest", got)
	}
}

func TestPruneSnapshots_KeepsNewest(t *testing.T) {
	s := testScheme(t)
	mk := func(name string, ageMin int) *volumesnapshotv1.VolumeSnapshot {
		return &volumesnapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns",
				CreationTimestamp: metav1.Time{Time: time.Unix(int64(1000-ageMin*60), 0)},
			},
		}
	}
	objs := []client.Object{mk("s0", 0), mk("s1", 1), mk("s2", 2), mk("s3", 3), mk("s4", 4)}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, KeepSnapshots: 3, Cfg: builder.Config{Namespace: "ns"}}

	items := make([]volumesnapshotv1.VolumeSnapshot, 0, 5)
	for _, o := range objs {
		items = append(items, *o.(*volumesnapshotv1.VolumeSnapshot))
	}
	r.pruneSnapshots(context.Background(), items)

	var remaining volumesnapshotv1.VolumeSnapshotList
	if err := c.List(context.Background(), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 3 {
		t.Fatalf("kept %d snapshots, want 3", len(remaining.Items))
	}
	kept := map[string]bool{}
	for _, it := range remaining.Items {
		kept[it.Name] = true
	}
	for _, want := range []string{"s0", "s1", "s2"} {
		if !kept[want] {
			t.Errorf("expected newest %q to be kept; remaining=%v", want, kept)
		}
	}
}

// Below the keep count, pruneSnapshots is a no-op.
func TestPruneSnapshots_BelowKeepNoop(t *testing.T) {
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: "ns"}}
	r.pruneSnapshots(context.Background(), []volumesnapshotv1.VolumeSnapshot{{}}) // 1 item, default keep 3
}

func TestDaemonFailure(t *testing.T) {
	const ns, key = "ns", "pkey"
	mkPod := func(name string, mutate func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Labels: map[string]string{projectKeyLabel: key},
		}}
		mutate(p)
		return p
	}

	tests := []struct {
		name    string
		pod     *corev1.Pod
		wantSub string // "" => expect no failure
	}{
		{"healthy", mkPod("ok", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }), ""},
		{"failed phase", mkPod("dead", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), "phase=Failed"},
		{"crashloop", mkPod("crash", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "buildkitd", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}
		}), "CrashLoopBackOff"},
		{"oomkilled", mkPod("oom", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:                 "buildkitd",
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
			}}
		}), "OOMKilled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.pod).Build()
			r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns}}
			got := r.daemonFailure(context.Background(), key)
			if tt.wantSub == "" {
				if got != "" {
					t.Errorf("daemonFailure = %q, want empty", got)
				}
				return
			}
			if got == "" || !strings.Contains(got, tt.wantSub) {
				t.Errorf("daemonFailure = %q, want substring %q", got, tt.wantSub)
			}
		})
	}
}
