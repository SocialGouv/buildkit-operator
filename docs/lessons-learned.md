# Lessons learned (transferable)

Non-obvious, technology-agnostic findings gathered while building and operating buildkit-operator on
OVH Managed Kubernetes. Nothing platform-specific here — just the gotchas that bit us in **Kata**,
**Kubernetes/Helm**, **BuildKit**, **OVH Object Storage**, and the **release toolchain**, and how to
avoid them.

## Kata Containers

- **Deleting the `kata-deploy` DaemonSet tears the node down — even with no `preStop` hook.** kata-deploy
  traps `SIGTERM` in its PID 1 and runs the node *cleanup* on pod termination (removes `/opt/kata`,
  reverts the containerd config, drops the `katacontainers.io/kata-runtime` node label, restarts
  containerd). This is **not** a Kubernetes `lifecycle.preStop` hook, so inspecting
  `.spec.template.spec.containers[].lifecycle` shows `NONE` and lulls you into thinking a `kubectl
  delete ds` is harmless. It is not — it fully de-configures Kata on the node.
- **Moving kata-deploy between namespaces = a full teardown + reinstall** (two containerd reconfigures),
  precisely because of the SIGTERM cleanup above. Plan it like a node reconfigure, in a maintenance
  window, scoped to a dedicated nodepool.
- **Uninstall cleanly; don't orphan the release.** kata-deploy creates **cluster-scoped** objects
  (a `ClusterRole`, `ClusterRoleBinding`, a `ServiceAccount`, and **~24 `RuntimeClass`es** — clh/qemu/fc
  variants). If you delete the Helm release *secrets* first, those resources are orphaned and the next
  `helm install` fails with `invalid ownership metadata ... release-namespace must equal X`. Either
  `helm uninstall` properly, or adopt the resources (patch `meta.helm.sh/release-*` + `app.kubernetes.io/managed-by: Helm`).
- **RuntimeClasses are cluster-scoped and outlive the node config.** The node-level cleanup (label,
  `/opt/kata`, containerd) is independent of the `RuntimeClass` objects — they can persist while the node
  is de-configured, which is misleading when checking "is Kata still set up?".
- **A containerd restart does NOT kill running containers.** Only the CRI control-plane blips for a few
  seconds. So co-located workloads (other build daemons, anything on the node) survive a kata-deploy
  install/reconfigure — verify after, but don't expect an outage.
- **Under nested virtualization, use cloud-hypervisor (`kata-clh`), not qemu.** qemu boots too slowly;
  the kata-agent misses containerd's CRI `get state` deadline (`context deadline exceeded`) and the
  kubelet kills the sandbox. And give the guest **≥ 4 vCPUs** — with the default 1 vCPU the agent is
  still too slow and the VM restart-loops. Kata reads its config **per-sandbox**, so bumping
  `default_vcpus` needs **no containerd restart** (a sidecar/DaemonSet that `sed`s the clh config is
  enough).
- **Smoke-test the runtime in one command:** a pod with `runtimeClassName: kata-clh` running `uname -r`
  shows the **guest** kernel (e.g. 6.18) which differs from the **host** kernel (e.g. 5.15) — proof the
  workload is in a real microVM, not the host kernel.
- **kata-deploy's Helm chart has a node-feature-discovery dependency.** Disable it
  (`node-feature-discovery.enabled=false`) and **vendor the NFD chart `.tgz` in `charts/`** so you don't
  need network access to the NFD Helm repo at install time.

## Kubernetes & Helm

- **`helm upgrade --reuse-values` does not merge newly-added values keys.** It reuses only the *last
  release's* computed values; any value key you added to the chart since is **absent**. Templates that
  reference it must be nil-safe, and on Helm 3.14+ `--reset-then-reuse-values` merges new chart defaults.
- **Nil-safe Helm helpers: use nested `with`.** `.Values.a.b` nil-pointers when `.Values.a` is absent.
  `and .Values.a .Values.a.b` does **not** help — Go templates' `and` is **not short-circuit** (it
  evaluates every argument). `dig "a" "b" def .Values` fails too — `dig` rejects Helm's `common.Values`
  type (`interface conversion: interface {} is common.Values, not map[string]interface {}`). What works:
  ```gotemplate
  {{- $v := "default" -}}
  {{- with .Values.a }}{{- with .b }}{{- $v = . }}{{- end }}{{- end -}}
  {{- $v -}}
  ```
- **Don't render a `Namespace` object for the Helm release namespace.** `helm install --create-namespace`
  already creates it; a chart-rendered Namespace for the same name collides on adoption (`invalid
  ownership metadata`). Render only the *extra* namespaces.
- **Changing a resource's namespace across an upgrade = delete-old + create-new.** Namespace is part of
  a resource's identity, so Helm removes it from the old namespace and creates it in the new one (fine,
  but expect the old one to disappear).
- **A controller's leader-election Lease should live in the namespace it RUNS in**, not the namespace it
  manages. Decouple them by feeding the pod's own namespace via the downward API:
  ```yaml
  env: [{ name: POD_NAMESPACE, valueFrom: { fieldRef: { fieldPath: metadata.namespace } } }]
  ```
  and use `POD_NAMESPACE` for `LeaderElectionNamespace`, while the `--namespace` flag points at the
  managed namespace. (controller-runtime watches all namespaces by default; cross-namespace owner
  references are invalid, so co-locate a CR with the objects it owns.)
- **Kyverno `excludedNamespaces` in per-cluster values usually REPLACES, not merges.** Layered Helm
  value files override list-typed values wholesale — so a per-cluster exclude list must repeat the base
  entries, and a cluster-wide addition goes in the *common* values to avoid dropping the others.
- **A mutate policy that sets `allowPrivilegeEscalation: false` makes a `privileged: true` container
  invalid** (the API rejects the combination). So a *privileged* node agent (hostPath installer) needs
  the **securityContext** exemption too, not only the hostPath one — exempt it like `kube-system` is.
- **Argo "OutOfSync + Missing" + no `argocd.argoproj.io/tracking-id` annotation on a resource** means the
  resource isn't actually being reconciled by that app — manual drift will persist. Don't assume a
  merged GitOps change is live; verify the live object.
- **Sealed Secrets without writing cleartext to disk:**
  ```bash
  kubectl create secret generic NAME -n NS --from-literal=K="$V" --dry-run=client -o yaml \
    | kubeseal --cert <controller-cert-url> -o yaml --scope cluster-wide > NAME.sealedsecret.yaml
  ```
  `--scope cluster-wide` lets the SealedSecret be unsealed in any namespace; the in-cluster controller
  decrypts it into a normal `Secret`. To copy a Secret between namespaces without printing its values,
  pipe `kubectl get -o json | <edit metadata.namespace> | kubectl apply -f -` (values stay in the pipe).

- **A controller-runtime client `Get` right after its own `Create` can return `NotFound`.** The default
  client reads from the **informer cache**, which lags etcd by a beat — so a "create then immediately
  Get-modify-Update" sequence intermittently drops the update (and `RetryOnConflict` won't save you: it
  only retries on `Conflict`, not `NotFound`). Two fixes, use both: mutate the object **returned by
  `Create`** (it already carries its ResourceVersion — no Get needed), and make follow-up touch loops
  retry on `NotFound` **and** `Conflict`. This bit us as a cold-start flake: a freshly-created
  BuildProject's warm-up `Status` stamp was dropped, so the daemon silently never scaled up.

- **A controller that reaps idle ephemeral children can reap one in its own birth window.** If "create
  the child, then mark it active (status/owner stamp) a beat later" is split across two API calls, the
  controller's informer can fire on the freshly-created child *before* the active mark lands — see it as
  idle (replicas 0) and delete it, so the work it was created for never runs. Here untrusted fork
  daemons were reaped microseconds after creation and every untrusted build hung. Guard the reaper with
  a **birth-window grace** keyed on `CreationTimestamp` (don't reap a child younger than N), and
  requeue-after so it's still reaped once genuinely idle.

## BuildKit

- **Cache import/export is best-effort — a broken cache backend does NOT fail the build.** BuildKit logs
  a warning and builds from scratch. So "the build went green" does **not** prove the cache worked. To
  verify, grep the build log for the real evidence:
  ```
  importing cache manifest from s3:<key>
  exporting cache to Amazon S3 ... sending cache export 2.2s done
  ```
- **The S3/registry cache uses the daemon's AWS credential chain** (`AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` as env on the buildkitd container). Clients carry **no** cache credentials —
  the daemon does the object-store I/O. Mount the creds with `envFrom: [{ secretRef: ... }]`.
- **Two cache layers, different lifetimes:** the hot local layer + `RUN --mount=type=cache` store (a
  retained PVC per daemon) vs. the cold remote cache (S3/registry). A daemon shares **layers** across
  daemons via the remote cache; `cache` mounts stay per-daemon. See
  [storage-and-cold-cache.md](storage-and-cold-cache.md).

## OVH (Object Storage & MKS)

- **S3-compatible Object Storage endpoint (GRA):** `https://s3.gra.io.cloud.ovh.net`, region `gra`.
  Create an **S3-type** container (not Swift) and generate **S3 user credentials** (`accessKey` /
  `secretKey`) — that JSON is just the user creds; the bucket name + endpoint are configured separately.
- **Storage class for a *cache*: prefer 1-AZ same-region over 3-AZ.** A build cache is **regenerable**,
  so paying for 3-AZ durability is wasted — 1-AZ gives lower cost and same-region latency, and an AZ
  outage just degrades to cold rebuilds (BuildKit's best-effort cache), not data loss. Pick 3-AZ only if
  *availability of the cache during an AZ incident* is a real requirement. Local-zone storage only helps
  if the cluster sits in that local zone.
- **MKS b2 nodes expose nested virtualization** (`/dev/kvm`, CPU `vmx`), so Kata microVMs run — with the
  clh + ≥4-vCPU caveats above. The guest kernel differs from the host kernel.

## Networking, DNS & egress-proxy CI

- **external-dns `service` source needs node RBAC — adding the arg alone crash-loops it.** The Helm chart
  derives the ClusterRole from `sources`, so enabling `--source=service` *via the chart* also grants
  `nodes` (+ `services`/`endpoints`/`pods`) `list,watch`. If you flip the arg with a **live patch** (or
  any path that doesn't re-render RBAC), external-dns dies with `failed to sync *v1.Node: context
  deadline exceeded` and **all DNS reconciliation stops** (shared component!). Patch the ClusterRole in
  the same change, or sync via the chart — never the arg alone.
- **external-dns + Azure DNS + a wildcard record = the TXT registry record is rejected.** A wildcard
  `*.foo` A record is fine, but the ownership TXT becomes `external-dns.*.foo`, which Azure rejects
  (`record set relative name '...' is invalid`, HTTP 400) — every reconcile re-errors. The **A record
  still resolves** (it's re-upserted; `policy=sync` only deletes records it *owns* via TXT, and an
  untracked one is left alone), so it's benign-but-noisy. The proper fix is `--txt-wildcard-replacement`
  (substitutes `*` in TXT names) — but it's a **global** flag affecting every wildcard that instance
  manages, so coordinate it platform-wide rather than patching it in for one record.
- **A blocking control-plane call dies against a CONNECT-proxy's idle timeout.** Egress-proxy-only CI
  (e.g. an HTTP `CONNECT` proxy on 443) caps how long an idle tunnel stays open (~50s observed). A
  control endpoint that *blocks* server-side (here `/route` waits for a cold daemon, ~1–2 min) is dropped
  mid-wait with `OpenSSL SSL_read: ... unexpected eof` (curl 56) / a timeout (curl 28). Fix on the
  **client**: poll in **bounded attempts** (each `--max-time` < the proxy's tunnel timeout) until ready,
  instead of one long request. Keep the single blocking call only for direct (non-proxied) clients.
- **nginx Ingress `proxy-read-timeout` defaults to 60s** — too low for an endpoint that *legitimately*
  blocks (cold-start). Raise it (`nginx.ingress.kubernetes.io/proxy-read-timeout: "300"`) — but note this
  only covers the *Ingress* hop; an upstream CONNECT proxy has its **own**, separate timeout (above).
- **GitLab `include: remote:` is fetched by the GitLab *server*, behind an allow-list.** On a locked-down
  instance the server denies it (`Remote file could not be fetched because URL is blocked: ... not on the
  Allow List`) and the pipeline fails with **0 jobs and no YAML error** — confirm with `POST
  /projects/:id/ci/lint`. Deliver a reusable CI brick to such platforms as a **vendored/`local` include**
  or a **CI/CD Catalog component** (mirror the repo into that GitLab), not a GitHub remote include.
  Runtime fetches (the runner pulling a script) ride the *runner's* proxy instead — a different egress
  path with different rules; vendor those too if the runner's proxy doesn't allow the source host.

## Release toolchain

- **conventional-changelog bumps a `feat!`/breaking commit to MAJOR even pre-1.0** (→ `1.0.0`). For a
  0.x project, semver treats breaking changes as a **minor** bump — force it (`release-it --increment
  minor`) unless you actually intend 1.0.
- **golangci-lint must be built with a Go ≥ the `go` directive in `go.mod`.** A prebuilt binary built
  with an older Go refuses: *"the Go language version (goX) used to build golangci-lint is lower than
  the targeted Go version"*. Run it from source so it compiles with the project toolchain:
  `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@vX run ./...`.
- **OCI references must be lowercase.** A mixed-case repo owner (e.g. `MyOrg`) breaks `cosign sign`
  ("could not parse reference"); lowercase it in-shell: `cosign sign "ghcr.io/${OWNER,,}/img@${DIGEST}"`.
- **Keyless cosign needs `id-token: write`** in the workflow permissions, and signs by **digest**
  (`steps.build.outputs.digest`) — one signature covers all tags on that digest.
