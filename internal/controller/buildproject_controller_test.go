package controller

import (
	"context"
	"testing"

	buildcatv1 "github.com/devthejo/buildcat/api/v1alpha1"
	"github.com/devthejo/buildcat/internal/builder"
	"github.com/devthejo/buildcat/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := buildcatv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Reconcile must turn a BuildProject into a StatefulSet-of-1 (gen2 PVC template) +
// Service, both owned by the BuildProject, and publish the mTLS endpoint in status.
func TestReconcile_CreatesDaemon(t *testing.T) {
	s := testScheme(t)
	key := router.ProjectKey("github.com/org/repo", "", "amd64")
	ns := "buildcat"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{
		Client: c, Scheme: s,
		Cfg: builder.Config{Namespace: ns, BuildkitImage: "img", CompanionImage: "comp", DaemonCertsSecret: "certs", BuildkitdConfigMap: "cfg", Port: 1234, HealthPort: 8080},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &sts); err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", sts.Spec.Replicas)
	}
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("volumeClaimTemplates = %d, want 1 (the gen2 cache PVC)", n)
	}
	if sc := sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "csi-cinder-high-speed-gen2" {
		t.Errorf("cache PVC storageClass = %v, want gen2 default", sc)
	}
	if len(sts.OwnerReferences) == 0 || sts.OwnerReferences[0].Name != key {
		t.Errorf("statefulset not owned by BuildProject")
	}

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}

	var got buildcatv1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: ns}, &got); err != nil {
		t.Fatal(err)
	}
	if want := router.Endpoint(key, ns, 1234); got.Status.Endpoint != want {
		t.Errorf("status.endpoint = %q, want %q", got.Status.Endpoint, want)
	}
	// No Ready replica in the fake => not yet Warm.
	if got.Status.Phase == "Warm" {
		t.Errorf("phase = Warm without a ready replica")
	}
}

// A second reconcile must be idempotent (no error, still one STS).
func TestReconcile_Idempotent(t *testing.T) {
	s := testScheme(t)
	key := router.ProjectKey("github.com/org/repo", "", "amd64")
	ns := "buildcat"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}
