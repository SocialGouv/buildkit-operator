# Operations runbook

How to deploy, expose, secure, observe, and tear down buildkit-operator. Pin a kubeconfig context per
call (on a shared cluster, **always** pass `--context` so you never touch the wrong cluster).

## Namespaces

buildkit-operator uses **three** namespaces, split by trust/role so each carries only the admission
exemption it needs ([ADR 0006](adr/0006-namespace-topology.md)):

| Namespace | Holds | Kyverno exemption |
|---|---|---|
| `buildkit-operator` | control plane: buildd + gateway (Deployments, Service, SA, RBAC, Lease, PDBs) | **none** |
| `buildkit-builds` | per-project daemons + forks, their certs/config/mirror, the `BuildProject` CRs | `securityContextPolicy` |
| `buildkit-system` | Kata node plumbing (kata-deploy + vcpu-tune), only when sandboxing forks | `securityContextPolicy` + `disallow-host-path` |

The chart creates the **builds** namespace (`createNamespaces: true`) and places each resource
accordingly; the **operator** namespace is the Helm release namespace (`--create-namespace`), and
`buildkit-system` is created with the Kata install ([deploy/kata/](../deploy/kata/)). Override names via
`namespaces.{operator,builds}`.

## Deploy

```bash
# 1. CRDs
task manifests && kubectl apply -f deploy/crd

# 2. mTLS material — the daemons mount it, so it goes in the BUILDS namespace
deploy/cert/create-certs.sh buildkit-builds
kubectl -n buildkit-builds apply -f deploy/cert/.certs/*-secret.yaml

# 3. control plane (chart creates the buildkit-builds ns; operator ns = release ns via --create-namespace)
helm upgrade --install buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator --create-namespace

# 4. (optional) warm node-pool headroom so wake-ups don't trigger node autoscaling
kubectl apply -f deploy/warm-pool.yaml
```

> Step 2 must run **after** step 3 the first time if `createNamespaces` makes the builds namespace
> (or create it first: `kubectl create namespace buildkit-builds`). With cert-manager the chart mints
> the certs into the builds namespace for you — skip step 2.

Default Helm values worth knowing: `replicaCount: 2`, `leaderElection: true`, `service.type:
ClusterIP` (daemons are always ClusterIP; off-cluster CI uses the SNI gateway — see below),
`defaultStorageClass: ""` (cache PVCs on the cluster's default StorageClass), `snapshotClassName: ""`
(durability snapshots off), `maxColdStarts: 8`, `s3.bucket: ""` (cold cache off), `gateway.host: ""`
(gateway off). The cloud-specific values — storage/snapshot classes and the LoadBalancer annotations
under `service` / `gateway.service` — have no default: declare the ones your cloud honours (OVH/OpenStack
presets are documented inline in `values.yaml`). Image tags default to the chart **appVersion** (an immutable release
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
(a namespaced Issuer) in the **builds** namespace (where the daemons mount the certs); otherwise point
`certManager.issuerRef` at your own CA issuer. The daemon cert covers `*.<builds-namespace>.svc` + (when
set) `*.<gateway.host>`. Distribute the generated **client** Secret's `tls.crt`/`tls.key`/`ca.crt` to CI
(the Action's `cert`/`key`/`ca`).

## Kyverno exemption

On a platform that mutates pods to `allowPrivilegeEscalation: false` (fabrique's Kyverno
`add-custom-mas-securitycontext`), rootless buildkit crash-loops. Exempt the **builds** namespace (the
control-plane namespace needs nothing). **Do this via GitOps**, not a live edit. In the platform Kyverno
values (`apps-infra`, `kyverno/ovh-prod.values.yaml`) — the list **replaces**, not merges:

```yaml
kyverno:
  securityContextPolicy:
    excludedNamespaces:
      - kube-system
      - prometheus-operator
      - buildkit-builds    # per-project daemons (rootless) + privileged Kata forks
      # - buildkit-system  # only if sandboxing forks (Kata) — see deploy/kata/README.md
```

When sandboxing untrusted forks, `buildkit-system` additionally needs the `disallow-host-path`
exemption — full matrix in [deploy/kata/README.md](../deploy/kata/README.md). Rationale and alternatives
in [security.md](security.md#admission-policy-kyverno--restricted-pss) and
[ADR 0006](adr/0006-namespace-topology.md).

## Expose publicly (for external CI runners)

On a public `/route`, prefer **OIDC identity verification**: buildd verifies the forge-signed id_token
the CI client mints natively (GitHub Action / GitLab component — no shared secret to distribute), then
**overwrites** the request's `repo` with the verified claim (GitHub `repository`, GitLab `project_path`)
and derives `untrusted` server-side, so a caller cannot impersonate another repo or poison its cache.

```bash
# break-glass admin token (bypasses OIDC, trusts the request's repo/untrusted) — for the manual
# `build` CLI and in-cluster ops. No trailing newline (a stray "\n" compares unequal → silent 401).
kubectl -n buildkit-operator create secret generic buildkit-operator-admin \
  --from-literal=token="$(openssl rand -hex 32 | tr -d '\n')"

helm upgrade buildkit-operator deploy/helm/buildkit-operator -n buildkit-operator \
  --set service.type=LoadBalancer \                 # buildd /route reachable externally
  --set gateway.host=builds.example.com \           # shared SNI gateway: one LB fronts every daemon
  --set oidc.providers[0].type=github \             # verify GitHub OIDC id_tokens (also: type=gitlab)
  --set oidc.providers[1].type=gitlab \
  --set 'oidc.repoAllowlist[0]=github.com/socialgouv/*' \   # optional hard org gate (unlisted → 403)
  --set oidc.adminTokenSecret=buildkit-operator-admin       # break-glass header X-Buildkit-Operator-Admin-Token
```

This renders the gateway Deployment + its single LoadBalancer Service, and makes buildd return
`tcp://<daemon>.builds.example.com:1234` from `/route`. Daemons stay `ClusterIP`. Then:

1. **OIDC on `/route`** — `oidc.providers` is the auth: each CI client mints its own id_token (the
   Action needs `permissions: id-token: write`; the GitLab component declares an `id_tokens:` entry),
   buildd checks signature against the issuer JWKS + audience + expiry, and the verified claim sets
   `repo`/`untrusted`. Add `oidc.repoAllowlist` (globs like `github.com/socialgouv/*`) for a hard org
   gate — a verified-but-unlisted repo gets HTTP 403. The `oidc.adminTokenSecret` token bypasses OIDC
   for break-glass/CLI (`X-Buildkit-Operator-Admin-Token`); `oidc.disable: true` is the admin-only
   switch to turn verification off. **Legacy fallback**: with `oidc.providers` empty, `/route` falls
   back to the shared `auth.tokenSecret` bearer (env `BUILDKIT_OPERATOR_TOKEN`) or open in-cluster use —
   keep that only for fully in-cluster deployments. Without any auth on a public `/route`, anyone can
   spin up daemons and poison caches.
2. **Daemon cert SAN** — regenerate the daemon cert covering `*.builds.example.com` and re-apply the
   Secret, or daemons fail mTLS validation from outside:
   ```bash
   GATEWAY_HOST=builds.example.com deploy/cert/create-certs.sh buildkit-builds
   kubectl -n buildkit-builds apply -f deploy/cert/.certs/*-secret.yaml
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
# re-apply CRDs first (they are NOT upgraded by `helm upgrade` — chart CRDs install once).
# Skipping this is SILENT breakage: fields the new buildd writes that the stored (old) CRD
# doesn't know are pruned by the apiserver — e.g. status.lastCacheExportGrant never persists
# (S3 cache exports run on every build instead of the cadence) and spec.s3CachePolicy seeded
# by projectDefaults rules is dropped (a `never` project keeps exporting).
task manifests && kubectl apply -f deploy/crd

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
  GATEWAY_HOST=builds.example.com deploy/cert/create-certs.sh buildkit-builds   # GATEWAY_HOST optional
  kubectl -n buildkit-builds apply -f deploy/cert/.certs/*-secret.yaml
  kubectl -n buildkit-builds rollout restart statefulset -l app.kubernetes.io/component=buildkitd
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

Upgrading the chart changes the rendered daemon template (a new companion tag is enough), which
REPLACES every daemon pod. Two things keep that from killing builds:

- `/route` stops advertising a daemon whose StatefulSet is mid-roll (its ready replica is still the
  OUTGOING pod), so a build routed during an upgrade waits for the new pod instead of being handed an
  endpoint that disappears seconds later.
- The reconciler holds a daemon's pending template while `.status.inflight` is non-empty, so a build
  already running is not severed. **Every build gets at least an hour** before a forced roll can cut
  it; the roll lands as soon as the last build is released, or once even the youngest build on that
  daemon has had its hour. A project building back-to-back keeps its youngest build young forever, so
  an absolute cap of `maxBuildSeconds` applies on top — past that a build has already outlived what
  the operator promises it, and an image nobody can update is the worse failure.

`buildkit_operator_daemon_rolls_held` counts the daemons currently withholding a template, and the
StatefulSet carries `buildkit-operator.socialgouv.github.io/roll-held-since`. A daemon wedged in
CrashLoopBackOff is rolled regardless — the roll is usually the repair.

The other ways a daemon can go away under a build are guarded too:

- **Node drains** — a daemon carries a PodDisruptionBudget (`minAvailable: 1`) exactly while it is
  serving builds; once idle the budget is deleted, not set to zero. A drain therefore waits for the
  builds on that node and no longer, on every tier — including `hot`, which never scales to zero and is
  the most likely to be sitting on a node someone drains. (A `minAvailable: 0` budget would look
  permissive and is not: for an *unhealthy* pod the disruption controller refuses the eviction, so a
  crash-looping daemon would block drains forever.) A leaked in-flight entry holds the budget until it
  expires at `maxBuildSeconds`, so a stuck drain is worth checking `.status.inflight` for.
- **Scale-to-zero and fork reaping** re-read the in-flight set from the API server before acting, not
  from the informer cache.
- **Lowering `fanout`** leaves a clone that is still serving builds for a later reconcile.
- **Gateway rollouts** stop accepting and keep proxying open builds for `gateway.drainSeconds`
  (default 1h; the pod's grace period sits 60s above it).

`/complete` authorizes in two ways, because a forge's OIDC token is minted when the job starts and
lives minutes (GitHub's, about two) while a build runs for as long as a build runs:

- a caller with a **live verified identity** may only release builds of its own repo (403 otherwise);
- a caller naming a **live buildId** is authorized by that alone — the server minted those 8 random
  bytes and handed them to exactly one caller, and the id can only release the one build it names.

That second path is what keeps a release from being rejected simply because the build outlasted its own
token, which used to leak an inflight entry on every build longer than ~2 minutes — pinning the daemon
warm, holding its roll, and keeping a disruption budget that blocks node drains. An id that matches no
live build proves nothing and is refused (401), and nothing is written. The Action re-mints its token
before releasing when the forge allows it, so releases stay attributable where possible.

`api.requireBuildId` refuses a release that carries no id at all (it would otherwise retire the
project's oldest build). Every client has sent the id since v0.17.0, so it can be turned on once no
consumer predates that.

A project that stays warm with no build running: read `.status.inflight`. Each routed build holds one
timestamped entry, released by the client's `/complete`. An entry left behind by a build whose client
never released it (a cancelled CI job kills the runner before its cleanup) expires on its OWN clock
past `--max-build-seconds` (default 2h) — the reconciler logs `expired inflight builds` when it sweeps
one. Entries never expire as a group, so a build that legitimately runs for hours keeps its daemon.

```bash
kubectl -n buildkit-builds   get buildproject          # PHASE (Warm/Idle/...), REPLICAS, ENDPOINT per project
kubectl -n buildkit-builds   get buildproject <key> -o jsonpath='{.status.inflight}'  # who is holding the daemon warm
kubectl -n buildkit-builds   get volumesnapshot        # durability snapshots
kubectl -n buildkit-operator logs deploy/buildkit-operator-buildd -f   # buildd runs in the operator ns
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
| Want to know **who** built / called `/route` | Each `/route` logs the resolved key, repo (the OIDC-verified claim when verification is on), `untrusted` flag and caller IP (`X-Forwarded-For` first hop behind the LB, else peer); OIDC/allowlist rejections log as `denied`, other auth failures as `unauthorized`, and break-glass calls as `admin-token used`. No token (OIDC id_token, bearer, or admin) is ever logged. | `kubectl -n buildkit-operator logs deploy/buildkit-operator-buildd | grep -E '"route"|denied|unauthorized|admin-token used'`. |

## S3 cold cache (optional, external) — a buildd policy

buildkit-operator does **not** deploy an object store; point it at OVH Object Storage (prod) or any
S3-compatible endpoint. The cold cache is configured **once on buildd**, not per build job:

```bash
# the bucket Secret (AWS creds) the DAEMONS use for the s3 backend — in the BUILDS namespace
kubectl -n buildkit-builds create secret generic buildkit-operator-s3 \
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

**Bucket GC** — buildkit's s3 cache exporter never deletes anything, so the chart also applies a
bucket lifecycle configuration through a post-install/post-upgrade hook Job (`s3.lifecycle`, on by
default when `s3.bucket` is set): objects expire after 60 days and incomplete multipart uploads are
aborted after 7 days. Expiry is safe by construction — a cold-cache miss is a rebuild, never an
error. Tune `s3.lifecycle.{expireDays,abortMultipartDays}` or set `s3.lifecycle.enabled: false` to
manage the bucket out-of-band.

## Tear down cleanly (shared cluster hygiene)

```bash
kubectl -n buildkit-builds   delete buildproject --all          # cascades StatefulSets/(ClusterIP)Services/PVCs
kubectl -n buildkit-builds   delete pvc -l app.kubernetes.io/name=buildkit-operator   # if any PVCs linger
helm uninstall buildkit-operator -n buildkit-operator            # removes the control plane + chart-made namespaces
# verify no orphans:
kubectl get pv | grep buildkit-operator
kubectl get volumesnapshotcontent 2>/dev/null | grep buildkit || true
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
