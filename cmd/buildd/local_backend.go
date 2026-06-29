package main

import (
	"errors"
	"time"

	"github.com/go-logr/logr"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/identity"
	"github.com/socialgouv/buildkit-operator/internal/provisioner/local"
	"golang.org/x/time/rate"
	ctrl "sigs.k8s.io/controller-runtime"
)

// localParams bundles the buildd knobs the single-host (Incus + ZFS) backend needs. It is a plain struct
// so main() can pass the parsed flags without a long argument list.
type localParams struct {
	cfg              builder.Config // Port + Namespace (Namespace is informational on /route responses here)
	apiListen        string
	routeWait        time.Duration
	maxCold          int
	apiRateLimit     float64
	apiRateBurst     int
	s3Bucket         string
	s3Region         string
	s3Endpoint       string
	pool             string // ZFS parent dataset
	image            string // buildkitd Incus image
	vmImage          string // VM image for untrusted forks (empty = image)
	mountPath        string // buildkitd data dir the cache dataset mounts at
	idleTimeout      time.Duration
	snapshotEvery    time.Duration
	keepSnapshots    int
	maxBuildSeconds  int
	forkEgressStrict bool
	endpointDomain   string
	certsPath        string
	runtime          string // "incus" (default) | "docker"
}

// runLocalBackend wires the local provisioner + the shared routing API and serves until SIGTERM. It is
// the single-host analogue of main()'s Kubernetes manager path: a reconcile goroutine (scale-to-zero)
// plus the HTTP /route server, sharing this run's OIDC verifier and auth tokens.
func runLocalBackend(p localParams, verifier *identity.Verifier, authToken, adminToken string, log logr.Logger) error {
	if p.pool == "" || p.image == "" {
		return errors.New("--backend local requires --incus-pool and --incus-image")
	}

	// Pick the instance runtime: Incus (production, ZFS + VM forks) or Docker (dev/local, host dirs, no
	// VM isolation). The provisioner logic is identical — only the HostOps seam differs.
	var ops local.HostOps
	switch p.runtime {
	case "", "incus":
		ops = local.NewCLI()
	case "docker":
		ops = local.NewDocker(p.cfg.Port, p.certsPath)
	default:
		return errors.New("--local-runtime must be incus or docker")
	}

	prov := local.New(ops, local.Config{
		Pool:             p.pool,
		Image:            p.image,
		VMImage:          p.vmImage,
		MountPath:        p.mountPath,
		Port:             p.cfg.Port,
		Wait:             p.routeWait,
		IdleTimeout:      p.idleTimeout,
		SnapshotEvery:    p.snapshotEvery,
		KeepSnapshots:    p.keepSnapshots,
		MaxBuildSeconds:  p.maxBuildSeconds,
		ForkEgressStrict: p.forkEgressStrict,
		EndpointDomain:   p.endpointDomain,
		CertsHostPath:    p.certsPath,
	}, log.WithName("provisioner"))

	var limiter *rate.Limiter
	if p.apiRateLimit > 0 {
		limiter = rate.NewLimiter(rate.Limit(p.apiRateLimit), p.apiRateBurst)
	}

	rs := &routeServer{
		prov: prov, cfg: p.cfg, addr: p.apiListen, wait: p.routeWait,
		coldStartSem: make(chan struct{}, p.maxCold),
		s3Bucket:     p.s3Bucket, s3Region: p.s3Region, s3Endpoint: p.s3Endpoint,
		verifier: verifier, authToken: authToken, adminToken: adminToken,
		limiter: limiter, log: log.WithName("route"),
	}

	ctx := ctrl.SetupSignalHandler()
	log.Info("starting buildd", "backend", "local", "api", p.apiListen, "pool", p.pool)

	// Run the scale-to-zero loop and the HTTP API together; return when the first stops (graceful
	// shutdown on SIGTERM returns nil from both).
	errCh := make(chan error, 2)
	go func() { errCh <- prov.Run(ctx) }()
	go func() { errCh <- rs.Start(ctx) }()
	return <-errCh
}
