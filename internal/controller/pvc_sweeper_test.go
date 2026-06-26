package controller

import (
	"context"
	"testing"
	"time"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func cachePVC(key string, age time.Duration) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              router.CachePVCName(key),
			Namespace:         "ns",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
	}
}

func TestPVCSweeper_ReapsOrphansOnly(t *testing.T) {
	s := testScheme(t)
	live := &bkov1.BuildProject{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "ns"}, Spec: bkov1.BuildProjectSpec{Key: "plive"}}
	// A non-cache PVC must be ignored entirely (name lacks the cache-buildkitd- prefix).
	nonCache := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-something-0", Namespace: "ns", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}}
	objs := []client.Object{
		live,
		cachePVC("plive", time.Hour),    // live project → keep
		cachePVC("porphan", time.Hour),  // no project, old → reap
		cachePVC("pyoung", time.Minute), // no project but just created → keep (grace)
		nonCache,
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	sw := &PVCSweeper{Client: c, Namespace: "ns", Grace: 10 * time.Minute}
	sw.sweep(context.Background())

	get := func(key string) error {
		return c.Get(context.Background(), types.NamespacedName{Name: router.CachePVCName(key), Namespace: "ns"}, &corev1.PersistentVolumeClaim{})
	}
	if err := get("porphan"); !apierrors.IsNotFound(err) {
		t.Errorf("orphan PVC should have been reaped, got err=%v", err)
	}
	if err := get("plive"); err != nil {
		t.Errorf("live project's PVC must be kept: %v", err)
	}
	if err := get("pyoung"); err != nil {
		t.Errorf("young PVC must be kept (grace): %v", err)
	}
	var other corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Name: "data-something-0", Namespace: "ns"}, &other); err != nil {
		t.Errorf("non-cache PVC must be ignored, but it's gone: %v", err)
	}
}

func TestPVCSweeper_Defaults(t *testing.T) {
	sw := &PVCSweeper{}
	if sw.interval() != 15*time.Minute {
		t.Errorf("default interval = %v, want 15m", sw.interval())
	}
	if sw.grace() != 10*time.Minute {
		t.Errorf("default grace = %v, want 10m", sw.grace())
	}
	if !sw.NeedLeaderElection() {
		t.Error("sweeper must be leader-only")
	}
}
