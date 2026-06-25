# Operations runbook

How to deploy, expose, secure, observe, and tear down buildkit-operator. Commands assume the namespace
`buildkit-operator` and a kubeconfig context pinned per call (on a shared cluster, **always** pass
`--context` so you never touch the wrong cluster).

## Deploy

```bash
# 1. CRDs
make manifests && kubectl apply -f deploy/crd

# 2. mTLS material (daemon + client cert Secrets)
deploy/cert/create-certs.sh buildkit-operator
kubectl -n buildkit-operator apply -f deploy/cert/.certs/*-secret.yaml

# 3. control plane (buildd Deployment + RBAC + buildkitd.toml ConfigMap)
helm upgrade --install buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --create-namespace

# 4. (optional) warm node-pool headroom so wake-ups don't trigger node autoscaling
kubectl apply -f deploy/warm-pool.yaml
```

Default Helm values worth knowing: `replicaCount: 2`, `leaderElection: true`, `service.type:
ClusterIP` (daemons are always ClusterIP; off-cluster CI uses the SNI gateway — see below),
`snapshotClassName: csi-cinder-snapclass-in-use-v1`, `maxColdStarts: 8`, `s3.bucket: ""` (cold cache
off), `gateway.host: ""` (gateway off). Image tags default to the chart **appVersion** (an immutable release
tag) — override `image.tag` / `companion.image.tag` / `gateway.image.tag` only for local dev (e.g.
`dev`); never ship a floating tag to prod. Images are built and pushed by the
[`images`](../.github/workflows/images.yml) workflow; a private registry needs a pull secret on the
`default` and `buildkit-operator-buildd` ServiceAccounts.

### mTLS via cert-manager (instead of mkcert)

To have **cert-manager** issue and auto-renew the mTLS material instead of `create-certs.sh` (step 2),
set `certManager.enabled=true`. The chart renders the daemon + client `Certificate`s into the same
`certs.{daemon,client}SecretName` Secrets, and buildd is started with `--cert-manager-certs` so it
remaps cert-manager's `tls.crt`/`tls.key`/`ca.crt` onto the `cert.pem`/`key.pem`/`ca.pem` filenames the
daemon reads (no daemon change). With no PKI, `certManager.ca.create=true` bootstraps a self-signed CA
(a namespaced Issuer) in the operator namespace; otherwise point `certManager.issuerRef` at your own CA
issuer. The daemon cert covers `*.<namespace>.svc` + (when set) `*.<gateway.host>`. Distribute the
generated **client** Secret's `tls.crt`/`tls.key`/`ca.crt` to CI (the Action's `cert`/`key`/`ca`).

## Kyverno exemption

On a platform that mutates pods to `allowPrivilegeEscalation: false` (fabrique's Kyverno
`add-custom-mas-securitycontext`), rootless buildkit crash-loops. Exempt the namespace — the
precedented pattern (see `arc-runners`). **Do this via GitOps**, not a live edit:

```yaml
# in the ClusterPolicy's rule match/exclude — add the buildkit-operator namespace
exclude:
  any:
    - resources:
        namespaces: [buildkit-operator]
```

Rationale and alternatives in [security.md](security.md#admission-policy-kyverno--restricted-pss).

## Expose publicly (for external CI runners)

```bash
# the /route bearer token (REQUIRED once /route is on a public LB) — no trailing newline, so the
# client and the Secret compare byte-for-byte (a stray "\n" is a silent 401).
kubectl -n buildkit-operator create secret generic buildkit-operator-auth \
  --from-literal=token="$(openssl rand -hex 32 | tr -d '\n')"

helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator \
  --set service.type=LoadBalancer \                 # buildd /route reachable externally
  --set gateway.host=builds.example.com \           # shared SNI gateway: one LB fronts every daemon
  --set auth.tokenSecret=buildkit-operator-auth      # bearer-token auth on /route (Secret key: token)
```

This renders the gateway Deployment + its single LoadBalancer Service, and makes buildd return
`tcp://<daemon>.builds.example.com:1234` from `/route`. Daemons stay `ClusterIP`. Then:

1. **Bearer token** — hand the `token` value to CI (Action input `token` / env
   `BUILDKIT_OPERATOR_TOKEN`). Without auth on a public `/route`, anyone can spin up daemons.
2. **Daemon cert SAN** — regenerate the daemon cert covering `*.builds.example.com` and re-apply the
   Secret, or daemons fail mTLS validation from outside:
   ```bash
   GATEWAY_HOST=builds.example.com deploy/cert/create-certs.sh buildkit-operator
   kubectl -n buildkit-operator apply -f deploy/cert/.certs/*-secret.yaml
   ```
3. **Wildcard DNS** — point `*.builds.example.com` at the gateway LB IP; or, until that record
   exists, pass the runner the gateway IP via the Action's `gateway-ip` input (escape hatch).
   The OVH/OpenStack LB also needs its idle timeout raised — the chart does this; see
   [platform-ovh-mks.md](platform-ovh-mks.md#loadbalancer-idle-timeout).

Details: [ci-integration.md](ci-integration.md#the-certificate-san-requirement). Leave
`gateway.host` unset if your runners are in-cluster — then there is no public daemon surface at all
(see [security.md](security.md#honest-tradeoffs)).

## HA — verify and test

```bash
kubectl -n buildkit-operator get deploy buildkit-operator-buildd          # want 2/2
kubectl -n buildkit-operator get lease buildkit-operator-buildd.buildkit-operator.socialgouv.github.io -o jsonpath='{.spec.holderIdentity}'
kubectl -n buildkit-operator delete pod <leader-pod>             # follower takes the Lease; /route keeps serving
```

The reconciler runs on the leader only; `/route` is served by both replicas.

## Upgrade

Pinned image tags follow the chart `appVersion`, so an upgrade is a chart bump:

```bash
# re-apply CRDs first (they are NOT upgraded by `helm upgrade` — chart CRDs install once)
make manifests && kubectl apply -f deploy/crd

helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --reuse-values
kubectl -n buildkit-operator rollout status deploy/buildkit-operator-buildd
```

buildd rolls with leader election (the follower keeps `/route` serving). Per-project daemons are
**not** restarted by a control-plane upgrade; a changed daemon pod template (buildkit image bump, S3
creds) is rolled by the reconciler via the StatefulSet template hash, and the retained PVC survives the
restart. Roll back with `helm rollback buildkit-operator -n buildkit-operator`.

## Certificate rotation

- **cert-manager** (`certManager.enabled=true`): leaf certs auto-renew at `renewBefore` (default 30d
  before a 1y expiry); nothing to do. Daemons pick up the rotated Secret on their next restart.
- **mkcert / openssl** (`create-certs.sh`): no auto-renewal — regenerate and re-apply before expiry,
  then restart daemons so they re-read the cert:
  ```bash
  GATEWAY_HOST=builds.example.com deploy/cert/create-certs.sh buildkit-operator   # GATEWAY_HOST optional
  kubectl -n buildkit-operator apply -f deploy/cert/.certs/*-secret.yaml
  kubectl -n buildkit-operator rollout restart statefulset -l app.kubernetes.io/component=buildkitd
  ```
  Redistribute the **client** Secret to CI if the CA changed. Keep `renewBefore < duration` when using
  cert-manager (a `renewBefore` ≥ `duration` never renews).

## Observe

Prometheus metrics on `--metrics-addr` (`:8081`): `buildkit_operator_routes_total`,
`buildkit_operator_route_duration_seconds`, `buildkit_operator_coldstart_seconds`, `buildkit_operator_coldstarts_inflight`,
`buildkit_operator_scale_events_total`, `buildkit_operator_snapshots_total`. Useful signals: rising
`coldstarts_inflight` near `--max-cold-starts` means you're throttling wake-ups (consider
warm-pool/idle-timeout tuning); `buildkit_operator_coldstart_seconds` isolates the cold-daemon wait
(provision + Cinder attach) from warm route latency — the bench B/C signal — while
`route_duration_seconds` covers all routes.

```bash
kubectl -n buildkit-operator get buildproject          # PHASE (Warm/Idle/...), REPLICAS, ENDPOINT per project
kubectl -n buildkit-operator get volumesnapshot        # durability snapshots
kubectl -n buildkit-operator logs deploy/buildkit-operator-buildd -f
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

## Troubleshooting — common failure modes

| Symptom | Likely cause | What to do |
|---|---|---|
| `BuildProject` PHASE stuck **`Failed`** | The daemon pod is wedged: `CrashLoopBackOff`, image pull error, OOMKilled, or pod `Failed`. The reconciler promotes a not-ready daemon to `Failed` (and keeps re-checking) so it surfaces instead of sitting in `Scaling`. | `kubectl -n buildkit-operator describe bp <key>` — the `Ready` condition message names the container + reason (e.g. `buildkitd: CrashLoopBackOff`). Then `kubectl -n buildkit-operator logs <pod> -c buildkitd --previous`. OOMKilled ⇒ raise `spec.resources`; ImagePullBackOff ⇒ check the image tag / pull secret. It self-heals to `Warm` once the pod recovers. |
| PHASE stays **`Scaling`** for a while on first build | Normal cold start: provision + Cinder attach (~30 s, bench B). Not `Failed` because the pod is still legitimately starting. | Wait; watch `kubectl -n buildkit-operator get pod -l <project-key-label>`. If it never goes Ready and isn't `Failed`, inspect events — likely scheduling (no node matches `daemonScheduling`) or a stuck PVC attach. |
| `/route` returns **504** to CI | The daemon didn't become Ready within `--route-wait` (cold start slower than the client/route timeout), or cold-start backpressure (`--max-cold-starts`) queued it. | Raise the client route timeout (Action `route-wait`) and/or `--max-cold-starts`; pre-warm on push (`/prewarm`) to hide attach latency. Check `buildkit_operator_coldstarts_inflight` near the cap. |
| `/route` or `/prewarm` returns **429** | The routing-API rate limit tripped (`--api-rate-limit` / `--api-rate-burst`). A genuine CI burst or a misbehaving/compromised caller. | If legitimate, raise `--api-rate-limit`. If not, the audit log identifies the caller (below). Set `--api-rate-limit=0` to disable (not recommended on a public LB). |
| Off-cluster builds fail TLS (cert error) right after enabling the gateway | The daemon cert has no `*.<gateway.host>` SAN. buildd logs a **`WARNING: daemon cert has no SAN covering the gateway domain`** at startup when `gateway.host` is set. | Regenerate the cert with the SAN and re-apply (see [Expose publicly](#expose-publicly-for-external-ci-runners) step 2), then `rollout restart` the daemons. Confirm the boot warning is gone. |
| Want to know **who** built / called `/route` | Each `/route` logs the resolved key, repo, `untrusted` flag and caller IP (`X-Forwarded-For` first hop behind the LB, else peer); auth failures log as `unauthorized`. The bearer token is never logged. | `kubectl -n buildkit-operator logs deploy/buildkit-operator-buildd | grep -E '"route"|unauthorized'`. |

## S3 cold cache (optional, external) — a buildd policy

buildkit-operator does **not** deploy an object store; point it at OVH Object Storage (prod) or any
S3-compatible endpoint. The cold cache is configured **once on buildd**, not per build job:

```bash
# the bucket Secret (AWS creds) the DAEMONS use for the s3 backend
kubectl -n buildkit-operator create secret generic buildkit-operator-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=… --from-literal=AWS_SECRET_ACCESS_KEY=…

helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator \
  --set s3.bucket=buildcache \
  --set s3.region=gra \
  --set s3.endpoint=https://s3.gra.io.cloud.ovh.net \
  --set s3.credsSecret=buildkit-operator-s3
```

`/route` then returns the per-project cache reference (bucket/region/endpoint, prefix = the project
key, **no credentials**) and the client applies it automatically — CI callers configure **zero** S3.
The daemons do the S3 I/O and read the AWS creds from `credsSecret` (mounted as env). For a
self-hosted test backend you can run MinIO in-cluster (Deployment + PVC + a `buildcache` bucket); it
is not part of the chart. See [storage-and-cold-cache.md](storage-and-cold-cache.md).

## Tear down cleanly (shared cluster hygiene)

```bash
kubectl -n buildkit-operator delete buildproject --all          # cascades StatefulSets/(ClusterIP)Services/PVCs
kubectl -n buildkit-operator delete pvc -l app.kubernetes.io/name=buildkit-operator   # if any PVCs linger
helm uninstall buildkit-operator -n buildkit-operator
# verify no orphans:
kubectl get pv | grep buildkit-operator
kubectl -n buildkit-operator get volumesnapshotcontent 2>/dev/null
```

Daemon Services are `ClusterIP`, so deleting `BuildProject`s frees no LoadBalancers. The only public
LBs are chart-level (the buildd `/route` Service and the shared gateway when exposed); `helm
uninstall` removes them. Afterwards check `kubectl -n buildkit-operator get svc` shows no stray `LoadBalancer`
(public IPs cost money and surface).

### Namespace stuck `Terminating` on a VolumeSnapshot

If you delete the whole namespace while durability snapshots exist, it can hang in `Terminating`: a
`VolumeSnapshot` keeps a `snapshot.storage.kubernetes.io/volumesnapshot-bound-protection` finalizer
until the snapshotter releases it, and a wedged Cinder backend deletion stalls that. Prefer deleting
`BuildProject`s first (above) so the operator reaps the cache. To unblock a namespace that is already
stuck on a **test** snapshot (this reclaims the backend snapshot via the content's `Delete` policy):

```bash
kubectl --context <ctx> -n <ns> get volumesnapshot                    # find the holder
kubectl --context <ctx> -n <ns> delete volumesnapshot <name> --wait=false
# if the finalizer still hangs after the content is gone (test debris only — may orphan a backend snap):
kubectl --context <ctx> -n <ns> patch volumesnapshot <name> --type=merge -p '{"metadata":{"finalizers":null}}'
```
