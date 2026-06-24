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

Default Helm values worth knowing: `replicaCount: 2`, `leaderElection: true`, `service.type:
ClusterIP` (daemons are always ClusterIP; off-cluster CI uses the SNI gateway — see below),
`snapshotClassName: csi-cinder-snapclass-in-use-v1`, `maxColdStarts: 8`, `s3.bucket: ""` (cold cache
off), `gateway.host: ""` (gateway off), images `ghcr.io/socialgouv/buildcat-{buildd,companion,gateway}:dev`.
Images are built and pushed by the [`images`](../.github/workflows/images.yml) workflow; a private
registry needs a pull secret on the `default` and `buildcat-buildd` ServiceAccounts.

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
  --set service.type=LoadBalancer \              # buildd /route reachable externally
  --set gateway.host=builds.example.com          # shared SNI gateway: one LB fronts every daemon
```

This renders the gateway Deployment + its single LoadBalancer Service, and makes buildd return
`tcp://<daemon>.builds.example.com:1234` from `/route`. Daemons stay `ClusterIP`. Two more steps:

1. **Wildcard DNS** — point `*.builds.example.com` at the gateway LB's external IP.
2. **Daemon cert SAN** — regenerate the daemon cert covering `*.builds.example.com` and re-apply the
   Secret, or daemons fail mTLS validation from outside:
   ```bash
   GATEWAY_HOST=builds.example.com deploy/cert/create-certs.sh buildcat
   kubectl -n buildcat apply -f deploy/cert/.certs/*-secret.yaml
   ```

Details: [ci-integration.md](ci-integration.md#the-certificate-san-requirement). Leave
`gateway.host` unset if your runners are in-cluster — then there is no public daemon surface at all
(see [security.md](security.md#honest-tradeoffs)).

## HA — verify and test

```bash
kubectl -n buildcat get deploy buildcat-buildd          # want 2/2
kubectl -n buildcat get lease buildcat-buildd.buildcat.dev -o jsonpath='{.spec.holderIdentity}'
kubectl -n buildcat delete pod <leader-pod>             # follower takes the Lease; /route keeps serving
```

The reconciler runs on the leader only; `/route` is served by both replicas.

## Observe

Prometheus metrics on `--metrics-addr` (`:8081`): `buildcat_routes_total`,
`buildcat_route_duration_seconds`, `buildcat_coldstart_seconds`, `buildcat_coldstarts_inflight`,
`buildcat_scale_events_total`, `buildcat_snapshots_total`. Useful signals: rising
`coldstarts_inflight` near `--max-cold-starts` means you're throttling wake-ups (consider
warm-pool/idle-timeout tuning); `buildcat_coldstart_seconds` isolates the cold-daemon wait
(provision + Cinder attach) from warm route latency — the bench B/C signal — while
`route_duration_seconds` covers all routes.

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

## S3 cold cache (optional, external) — a buildd policy

buildcat does **not** deploy an object store; point it at OVH Object Storage (prod) or any
S3-compatible endpoint. The cold cache is now configured **once on buildd**, not per build job:

```bash
# the bucket Secret (AWS creds) the DAEMONS use for the s3 backend
kubectl -n buildcat create secret generic buildcat-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=… --from-literal=AWS_SECRET_ACCESS_KEY=…

helm upgrade buildcat deploy/helm/buildcat -n buildcat \
  --set s3.bucket=buildcache \
  --set s3.region=gra \
  --set s3.endpoint=https://s3.gra.io.cloud.ovh.net \
  --set s3.credsSecret=buildcat-s3
```

`/route` then returns the per-project cache reference (bucket/region/endpoint, prefix = the project
key, **no credentials**) and the client applies it automatically — CI callers configure **zero** S3.
The daemons do the S3 I/O and read the AWS creds from `credsSecret` (mounted as env). For a
self-hosted proof backend you can run MinIO in-cluster (Deployment + PVC + a `buildcache` bucket) —
that is what the validation used; it is not part of the chart. See
[storage-and-cold-cache.md](storage-and-cold-cache.md).

## Tear down cleanly (shared cluster hygiene)

```bash
kubectl -n buildcat delete buildproject --all          # cascades StatefulSets/(ClusterIP)Services/PVCs
kubectl -n buildcat delete pvc -l app.kubernetes.io/name=buildcat   # if any PVCs linger
helm uninstall buildcat -n buildcat
# verify no orphans:
kubectl get pv | grep buildcat
kubectl -n buildcat get volumesnapshotcontent 2>/dev/null
```

Daemon Services are `ClusterIP`, so deleting `BuildProject`s frees no LoadBalancers. The only public
LBs are chart-level (the buildd `/route` Service and the shared gateway when exposed); `helm
uninstall` removes them. Afterwards check `kubectl -n buildcat get svc` shows no stray `LoadBalancer`
(public IPs cost money and surface).
