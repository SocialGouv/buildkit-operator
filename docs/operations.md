# Operations runbook

How to deploy, expose, secure, observe, and tear down buildcat. Commands assume the namespace
`buildcat` and a kubeconfig context pinned per call (on a shared cluster, **always** pass
`--context` so you never touch the wrong cluster).

## Deploy

```bash
# 1. CRDs
make manifests && kubectl apply -f deploy/crd

# 2. mTLS material (daemon + client cert Secrets)
deploy/cert/create-certs.sh buildcat
kubectl -n buildcat apply -f deploy/cert/.certs/*-secret.yaml

# 3. control plane (buildd Deployment + RBAC + buildkitd.toml ConfigMap)
helm upgrade --install buildcat deploy/helm/buildcat -n buildcat --create-namespace

# 4. (optional) warm node-pool headroom so wake-ups don't trigger node autoscaling
kubectl apply -f deploy/warm-pool.yaml
```

Default Helm values worth knowing: `replicaCount: 2`, `leaderElection: true`,
`daemonServiceType: ClusterIP`, `snapshotClassName: csi-cinder-snapclass-in-use-v1`,
`maxColdStarts: 8`, images `ghcr.io/socialgouv/buildcat-{buildd,companion}:dev`. Images are built and
pushed by the [`images`](../.github/workflows/images.yml) workflow; a private registry needs a pull
secret on the `default` and `buildcat-buildd` ServiceAccounts.

## Kyverno exemption

On a platform that mutates pods to `allowPrivilegeEscalation: false` (fabrique's Kyverno
`add-custom-mas-securitycontext`), rootless buildkit crash-loops. Exempt the namespace — the
precedented pattern (see `arc-runners`). **Do this via GitOps**, not a live edit:

```yaml
# in the ClusterPolicy's rule match/exclude — add the buildcat namespace
exclude:
  any:
    - resources:
        namespaces: [buildcat]
```

Rationale and alternatives in [security.md](security.md#admission-policy-kyverno--restricted-pss).

## Expose publicly (for external CI runners)

```bash
helm upgrade buildcat deploy/helm/buildcat -n buildcat \
  --set service.type=LoadBalancer \        # buildd /route reachable externally
  --set daemonServiceType=LoadBalancer     # gateway mode: daemons get LBs, /route returns LB IPs
```

Then **regenerate the daemon cert with the public address in its SAN** (LB IP/DNS) and re-apply the
Secret, or daemons will fail mTLS validation from outside. Details:
[ci-integration.md](ci-integration.md#the-certificate-san-requirement). Keep `ClusterIP` if your
runners are in-cluster — fewer public endpoints (see [security.md](security.md#honest-tradeoffs)).

## HA — verify and test

```bash
kubectl -n buildcat get deploy buildcat-buildd          # want 2/2
kubectl -n buildcat get lease buildcat-buildd.buildcat.dev -o jsonpath='{.spec.holderIdentity}'
kubectl -n buildcat delete pod <leader-pod>             # follower takes the Lease; /route keeps serving
```

The reconciler runs on the leader only; `/route` is served by both replicas.

## Observe

Prometheus metrics on `--metrics-addr` (`:8081`): `buildcat_routes_total`,
`buildcat_route_duration_seconds`, `buildcat_coldstarts_inflight`, `buildcat_scale_events_total`,
`buildcat_snapshots_total`. Useful signals: rising `coldstarts_inflight` near `--max-cold-starts`
means you're throttling wake-ups (consider warm-pool/idle-timeout tuning);
`route_duration_seconds` p50 tracks cold-start health.

```bash
kubectl -n buildcat get buildproject          # PHASE (Warm/Idle/...), REPLICAS, ENDPOINT per project
kubectl -n buildcat get volumesnapshot        # durability snapshots (M3)
kubectl -n buildcat logs deploy/buildcat-buildd -f
```

## Lifecycle behaviours to expect

- **Scale-to-zero** keeps the PVC. Waking a project is a ~30 s reattach (bench B), not a rebuild.
  `hot` tier never scales to zero.
- **Cold-start throttling** — a burst of new daemons is rate-limited (`--max-cold-starts`); excess
  routes wait rather than stampede (bench C).
- **Snapshots** run in-use on the `snapshotEverySec` cadence and prune to `--keep-snapshots`.
- **Restore / DR** — set `spec.restoreFromSnapshot` to seed a new daemon's PVC from a snapshot (new
  cluster / migration). S3 cold cache covers the rebuild-avoidance side
  ([storage-and-cold-cache.md](storage-and-cold-cache.md)).

## S3 cold cache (optional, external)

buildcat does **not** deploy an object store. Point builds at OVH Object Storage (prod) or any
S3-compatible endpoint via `BUILDCAT_S3_*` on the build job (not on buildd). The daemon does the I/O.
For a self-hosted proof backend you can run MinIO in-cluster (Deployment + PVC + a `buildcache`
bucket) — that is what the validation used; it is not part of the chart.

## Tear down cleanly (shared cluster hygiene)

```bash
kubectl -n buildcat delete buildproject --all          # cascades StatefulSets/Services/(LBs)/PVCs
kubectl -n buildcat delete pvc -l app.kubernetes.io/name=buildcat   # if any PVCs linger
helm uninstall buildcat -n buildcat
# verify no orphans:
kubectl get pv | grep buildcat
kubectl -n buildcat get volumesnapshotcontent 2>/dev/null
```

On a shared cluster, deleting `BuildProject`s also **releases their LoadBalancers** — check
`kubectl -n buildcat get svc` shows no stray `LoadBalancer` afterwards (public IPs cost money and
surface).
