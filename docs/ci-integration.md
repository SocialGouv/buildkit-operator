# CI integration (agnostic) & public exposure

buildkit-operator is **not** tied to any CI system. The entire integration is: *ask the control plane where
to build, then point `docker buildx` there over mTLS.* Anything that can run `docker buildx` and
reach the buildkit-operator endpoints works the same — a GitHub-hosted runner, a GitLab runner, Jenkins, or a
laptop. There is **nothing** GitHub/ARC-specific.

## GitHub Action (recommended path)

On GitHub, the integration is one step — `setup-buildx`, routing, mTLS, the warm cache, and the S3
cold cache are all handled by the Action:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest        # stock hosted runner — no self-hosted runner, no ARC
    steps:
      - uses: actions/checkout@v4
      - uses: socialgouv/buildkit-operator@v1
        with:
          buildd-url: ${{ vars.BUILDKIT_OPERATOR_BUILDD_URL }}
          ca:   ${{ secrets.BUILDKIT_OPERATOR_CA }}
          cert: ${{ secrets.BUILDKIT_OPERATOR_CERT }}
          key:  ${{ secrets.BUILDKIT_OPERATOR_KEY }}
          tags: ghcr.io/org/app:${{ github.sha }}
          push: "true"
```

Inputs:

| Input | Meaning |
|---|---|
| `buildd-url` | external buildd `/route` endpoint (LoadBalancer/Ingress) — **required** |
| `ca` / `cert` / `key` | client mTLS material, PEM — **required** |
| `tags` | image tag(s), whitespace-separated — **required** |
| `repo` | project identity = the cache key (default: the GitHub repository) |
| `name` | optional monorepo component (per-image daemon + cache; empty = whole repo) |
| `arch` | `amd64` \| `arm64` (default `amd64`) |
| `context` / `file` / `target` | build context, Dockerfile path, target stage |
| `push` | push the result to the registry (default `false`) |

The cold cache needs **no** client config: it is a project policy on buildd, returned by `/route`
and applied automatically (see [the S3 section below](#s3-from-ci--zero-client-config) and
[storage-and-cold-cache.md](storage-and-cold-cache.md)).

## The CI-agnostic core: `scripts/build.sh`

The Action is a thin wrapper around `scripts/build.sh` — a ~40-line POSIX script that any CI able to
run `docker buildx` + `curl` + `jq` can call directly:

```sh
# 1. route: ask buildd for this project's daemon endpoint
endpoint=$(curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL/route" \
  -H 'content-type: application/json' \
  -d "{\"repo\":\"$REPO\",\"name\":\"$NAME\",\"arch\":\"$ARCH\"}" | jq -r .endpoint)

# 2. point buildx at it over mTLS (cert files written from BUILDKIT_OPERATOR_CA/CERT/KEY)
docker buildx create --name buildkit-operator --driver remote \
  --driver-opt "cacert=$certs/ca.pem,cert=$certs/cert.pem,key=$certs/key.pem" "$endpoint" --use

# 3. build — the S3 cold cache (if any) comes back in the /route response and is applied automatically
exec docker buildx build --builder buildkit-operator …
```

It reads its inputs from the environment (the Action maps its inputs to these):

| Var | Meaning |
|---|---|
| `BUILDKIT_OPERATOR_BUILDD_URL` | external buildd `/route` endpoint (LB/Ingress) |
| `BUILDKIT_OPERATOR_CA` / `_CERT` / `_KEY` | client mTLS material, PEM |
| `REPO` / `ARCH` | project identity (defaults: git origin / `amd64`) |
| `NAME` | optional monorepo component (segments the repo into per-image daemons; empty = whole repo) |
| `TAGS` / `PUSH` | image tag(s), whitespace-separated / push the result |
| `BUILD_CONTEXT` / `DOCKERFILE` / `TARGET` | build context, Dockerfile path, target stage |

## CI-agnostic by construction

The same `scripts/build.sh` runs unchanged on a GitLab runner, Jenkins, or a laptop — only the way
the mTLS material reaches the job differs. The
[`socialgouv/buildkit-operator-example`](https://github.com/SocialGouv/buildkit-operator-example)
repo demonstrates both a stock GitHub-hosted `ubuntu-latest` job and a `.gitlab-ci.yml` calling the
same script, routing to the example daemon and exercising the S3 cache.

## Public exposure (why and how)

A hosted runner is **outside** the cluster, so buildkit-operator must be reachable over the internet — exactly
like `buildkit-service` (public LB + mTLS). **Two** LoadBalancers are exposed, and only two
regardless of how many projects exist:

| Endpoint | Service | Purpose |
|---|---|---|
| `BUILDKIT_OPERATOR_BUILDD_URL` | `buildkit-operator-buildd` LoadBalancer `:8080` | the `/route` API |
| `tcp://<daemon>.<gateway-host>:1234` | the **shared SNI gateway** LoadBalancer `:1234` | the build, over mTLS, to any daemon |

Daemons themselves stay **`ClusterIP`**. buildd is started with `--gateway-host <domain>` (Helm
`gateway.host`), so `/route` returns the deterministic hostname `tcp://<daemon>.<gateway-host>:1234`;
the single gateway LB peeks the TLS SNI and pipes to the daemon's ClusterIP Service — mTLS stays
end-to-end. This needs a **wildcard DNS** record `*.<gateway-host>` → the gateway LB (see
[architecture.md](architecture.md#the-shared-sni-gateway-off-cluster-ci)).

### The certificate SAN requirement

mTLS validates the daemon's hostname, so the **daemon certificate's SAN must cover the address the
runner dials** — which, through the gateway, is `<daemon>.<gateway-host>`. The cert script
([`deploy/cert/create-certs.sh`](../deploy/cert)) bakes in `*.buildkit-operator.svc`; for public exposure run
it with `GATEWAY_HOST=<gateway-host>` so it also adds the **wildcard SAN `*.<gateway-host>`** (one
cert validates every daemon's SNI hostname). If the SAN is wrong you get TLS validation failures or
`context deadline exceeded`. (The gateway terminates no TLS, so it needs no cert of its own — it only
peeks the SNI; the trust stays end-to-end between the client cert and the daemon cert.)

### S3 from CI — zero client config

The cold cache is a **buildd policy**, so a CI caller configures **no** S3 at all (no flags, no env,
no secrets). When buildd is set up with a bucket (`--s3-bucket …`), `/route` returns the per-project
cache reference (bucket/region/endpoint, prefix = the project key, **no credentials**) and the
client adds `--cache-from/--cache-to type=s3` automatically. The **daemon** performs the S3 I/O, so:

- the endpoint can be **in-cluster** (`http://minio.buildkit-operator.svc:9000`) — unreachable from the
  runner, yet it works, because the in-cluster daemon connects;
- the AWS creds live on the **daemon pods** (a k8s Secret via `--s3-creds-secret`), never on the
  runner and never on the wire.

The example CI log shows the daemon doing it: `importing cache manifest from s3:…`. See
[storage-and-cold-cache.md](storage-and-cold-cache.md).

## Endpoint shape

A deployment exposes exactly two endpoints, regardless of project count:

- buildd `/route`: the `buildkit-operator-buildd` LoadBalancer on `:8080` (set as
  `BUILDKIT_OPERATOR_BUILDD_URL`).
- shared SNI gateway LB on `:1234` — fronts every daemon; `/route` returns
  `tcp://<daemon>.<gateway-host>:1234` (e.g. `buildkitd-pa081c22c974da132.<gateway-host>` for
  `SocialGouv/buildkit-operator-example` → key `pa081c22c974da132`).

The S3 cold cache, when enabled, is a buildd-side policy pointing at an S3-compatible endpoint (OVH
Object Storage in production); CI callers never address it.
