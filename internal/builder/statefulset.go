// Package builder renders the Kubernetes objects for one hot vanilla buildkitd
// per (project, arch): a StatefulSet-of-1 + Service. The ONLY material change
// versus upstream's examples/kubernetes is that the rootless data dir is backed
// by a retained Cinder gen2 PVC (the warm cache) instead of an emptyDir.
package builder

import (
	"fmt"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Config holds the cluster-wide defaults injected by buildd.
type Config struct {
	Namespace          string // namespace the daemons run in
	BuildkitImage      string // rootless image, e.g. moby/buildkit:v0.18.2-rootless
	CompanionImage     string // our sidecar image (must bundle buildctl)
	DaemonCertsSecret  string // mTLS server certs (wildcard SAN) shared by all daemons
	BuildkitdConfigMap string // ConfigMap holding buildkitd.toml (gc config)
	SnapshotClass      string // VolumeSnapshotClass for durability snapshots (M3)
	Port               int32  // TCP mTLS port (1234)
	HealthPort         int32  // companion health port (8080)
	Companion          bool   // include the companion sidecar (default true; off needs no custom image)
	S3CredsSecret      string // Secret with AWS_ACCESS_KEY_ID/SECRET for the s3 cold cache (env on the daemon)
}

const (
	rootlessDataDir = "/home/user/.local/share/buildkit"
	rootlessConfig  = "/home/user/.config/buildkit"
	rootlessSock    = "unix:///run/user/1000/buildkit/buildkitd.sock"
	rootlessRunDir  = "/run/user/1000"
	privilegedData  = "/var/lib/buildkit"
	privilegedSock  = "unix:///run/buildkit/buildkitd.sock"
	cacheVolName    = "cache"
)

// Labels returns the canonical label set for a project's objects.
func Labels(bp *bkov1.BuildProject) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":           "buildkit-operator",
		"app.kubernetes.io/component":      "buildkitd",
		"buildkit-operator.socialgouv.github.io/project-key": bp.Spec.Key,
		"buildkit-operator.socialgouv.github.io/arch":        router.NormalizeArch(bp.Spec.Arch),
	}
}

// Service is the per-project ClusterIP Service exposing the daemon over mTLS. Off-cluster CI reaches
// daemons through the single shared SNI gateway (cmd/gateway), not a public LB per daemon.
func Service(bp *bkov1.BuildProject, cfg Config) *corev1.Service {
	l := Labels(bp)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      router.DaemonName(bp.Spec.Key),
			Namespace: cfg.Namespace,
			Labels:    l,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: l,
			Ports: []corev1.ServicePort{{
				Name:       "buildkit",
				Port:       cfg.Port,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(cfg.Port),
			}},
		},
	}
}

// StatefulSet renders the daemon StatefulSet-of-1 with a retained gen2 PVC.
func StatefulSet(bp *bkov1.BuildProject, cfg Config) *appsv1.StatefulSet {
	l := Labels(bp)
	name := router.DaemonName(bp.Spec.Key)
	replicas := int32(1)

	dataDir, sockAddr := rootlessDataDir, rootlessSock
	if bp.Spec.SecurityProfile == bkov1.ProfilePrivileged {
		dataDir, sockAddr = privilegedData, privilegedSock
	}

	podSec, daemonSec := securityContexts(bp.Spec.SecurityProfile)

	daemon := corev1.Container{
		Name:            "buildkitd",
		Image:           cfg.BuildkitImage,
		Args:            buildkitdArgs(bp.Spec.SecurityProfile, cfg.Port),
		SecurityContext: daemonSec,
		Ports:           []corev1.ContainerPort{{Name: "buildkit", ContainerPort: cfg.Port, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:  buildctlProbe(sockAddr, 5, 10),
		LivenessProbe:   buildctlProbe(sockAddr, 15, 30),
		Resources:       bp.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: cacheVolName, MountPath: dataDir},
			{Name: "certs", MountPath: "/certs", ReadOnly: true},
			{Name: "run", MountPath: rootlessRunDir},
		},
	}
	if bp.Spec.SecurityProfile != bkov1.ProfilePrivileged {
		daemon.VolumeMounts = append(daemon.VolumeMounts,
			corev1.VolumeMount{Name: "config", MountPath: rootlessConfig})
	}
	if cfg.S3CredsSecret != "" {
		// AWS creds for the s3 cold cache live on the DAEMON, not in every CI caller's secrets:
		// buildkit's s3 backend falls back to the AWS env chain when the client passes no creds.
		daemon.EnvFrom = append(daemon.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: cfg.S3CredsSecret}},
		})
	}

	containers := []corev1.Container{daemon}
	if cfg.Companion {
		companion := corev1.Container{
			Name:  "companion",
			Image: cfg.CompanionImage,
			Args: []string{
				"--buildkit-addr=" + sockAddr,
				"--cache-dir=" + dataDir,
				fmt.Sprintf("--listen=:%d", cfg.HealthPort),
			},
			SecurityContext: daemonSec,
			Ports:           []corev1.ContainerPort{{Name: "health", ContainerPort: cfg.HealthPort, Protocol: corev1.ProtocolTCP}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(cfg.HealthPort)}},
				InitialDelaySeconds: 5, PeriodSeconds: 15,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: cacheVolName, MountPath: dataDir, ReadOnly: true}, // statfs for inode backstop
				{Name: "run", MountPath: rootlessRunDir},
			},
		}
		containers = append(containers, companion)
	}

	volumes := []corev1.Volume{
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: cfg.DaemonCertsSecret}}},
	}
	if bp.Spec.SecurityProfile != bkov1.ProfilePrivileged {
		volumes = append(volumes, corev1.Volume{
			Name: "config",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cfg.BuildkitdConfigMap},
				Items:                []corev1.KeyToPath{{Key: "buildkitd.toml", Path: "buildkitd.toml"}},
			}},
		})
	}

	sc := bp.Spec.StorageClass
	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		StorageClassName: &sc,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", bp.Spec.CacheVolumeGi))},
		},
	}
	if bp.Spec.RestoreFromSnapshot != "" { // DR / seed: provision the warm cache from a snapshot
		pvcSpec.DataSource = &corev1.TypedLocalObjectReference{
			APIGroup: ptr("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: bp.Spec.RestoreFromSnapshot,
		}
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace, Labels: l},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         name,
			Replicas:            &replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: l},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: corev1.PodSpec{
					SecurityContext:               podSec,
					Containers:                    containers,
					TerminationGracePeriodSeconds: ptr[int64](120),
					Volumes:                       volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: cacheVolName},
				Spec:       pvcSpec,
			}},
		},
	}
}

// buildkitdArgs returns the daemon flags. Vanilla buildkitd — no custom plugins.
func buildkitdArgs(profile string, port int32) []string {
	args := []string{
		"--addr", privilegedSock,
		"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", port),
		"--tlscacert", "/certs/ca.pem",
		"--tlscert", "/certs/cert.pem",
		"--tlskey", "/certs/key.pem",
	}
	if profile == bkov1.ProfilePrivileged {
		return args
	}
	// rootless / userns
	args[1] = rootlessSock
	args = append(args,
		"--oci-worker-no-process-sandbox",
		"--config", rootlessConfig+"/buildkitd.toml",
	)
	return args
}

// buildctlProbe runs `buildctl ... debug workers` as a health check (buildctl ships in the image).
func buildctlProbe(addr string, initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"buildctl", "--addr", addr, "debug", "workers"},
		}},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		FailureThreshold:    3,
	}
}

// securityContexts returns (pod, container) security contexts for a profile.
// rootless wants Unconfined seccomp/apparmor (the Kyverno tension we spike).
// fsGroup + OnRootMismatch lets uid 1000 own the cache WITHOUT a recursive chown
// of the whole volume on every (re)attach — the lesson from the Cinder bench.
func securityContexts(profile string) (*corev1.PodSecurityContext, *corev1.SecurityContext) {
	switch profile {
	case bkov1.ProfilePrivileged:
		return &corev1.PodSecurityContext{},
			&corev1.SecurityContext{Privileged: ptr(true)}
	default: // rootless (and, for now, userns)
		onRootMismatch := corev1.FSGroupChangeOnRootMismatch
		pod := &corev1.PodSecurityContext{
			RunAsNonRoot:        ptr(true),
			RunAsUser:           ptr[int64](1000),
			RunAsGroup:          ptr[int64](1000),
			FSGroup:             ptr[int64](1000),
			FSGroupChangePolicy: &onRootMismatch,
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile:     &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}
		// allowPrivilegeEscalation is intentionally LEFT UNSET (not false): rootlesskit's
		// setuid `newuidmap` needs no_new_privs OFF to set up the user namespace. Setting it
		// false crash-loops rootless buildkitd ("newuidmap: Could not set caps") — verified on
		// OVH ovh-dev. The cluster's Kyverno mutate is the reason the daemon namespace must be
		// excluded (it would otherwise force this to false). See [[kyverno-buildkit-constraint]].
		ctr := &corev1.SecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr[int64](1000),
		}
		return pod, ctr
	}
}

func ptr[T any](v T) *T { return &v }
