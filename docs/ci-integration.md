# CI integration (agnostic) & public exposure

buildcat is **not** tied to any CI system. The entire integration is: *ask the control plane where
to build, then point `docker buildx` there over mTLS.* Anything that can run `docker buildx` and
reach the buildcat endpoints works the same — a GitHub-hosted runner, a GitLab runner, Jenkins, or a
laptop. There is **nothing** GitHub/ARC-specific.

## The contract: `build.sh`

The reference integration is a ~40-line POSIX script
([`buildcat-example/build.sh`](https://github.com/SocialGouv/buildcat-example)):

```sh
# 1. route: ask buildd for this project's daemon endpoint
endpoint=$(curl -fsS -XPOST "$BUILDCAT_BUILDD_URL/route" \
  -H 'content-type: application/json' \
  -d "{\"repo\":\"$REPO\",\"arch\":\"$ARCH\"}" | jq -r .endpoint)

# 2. point buildx at it over mTLS (absolute cert paths — buildx reads them at create time)
docker buildx create --name buildcat --driver remote \
  --driver-opt "cacert=$certs/ca.pem,cert=$certs/cert.pem,key=$certs/key.pem" "$endpoint" --use

# 3. build — the S3 cold cache (if any) comes back in the /route response and is applied automatically
exec docker buildx build --builder buildcat $extra "$@"
```

Environment:

| Var | Meaning |
|---|---|
| `BUILDCAT_BUILDD_URL` | external buildd `/route` endpoint (LB/Ingress) |
| `BUILDCAT_CERTS_DIR` | dir holding `ca.pem cert.pem key.pem` (client mTLS material) |
| `REPO` / `ARCH` | project identity (defaults: git origin / `amd64`) |
| `NAME` | optional monorepo component (segments the repo into per-image daemons; empty = whole repo) |

The cold cache needs **no** client config: it is a project policy on buildd, returned by `/route`
and applied automatically (see [the S3 section below](#s3-from-ci--zero-client-config) and
[storage-and-cold-cache.md](storage-and-cold-cache.md)).

## Worked example: GitHub-hosted runner

[`socialgouv/buildcat-example`](https://github.com/SocialGouv/buildcat-example) runs on a **stock
`ubuntu-latest` runner** — no self-hosted runner, no ARC:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      BUILDCAT_BUILDD_URL: ${{ vars.BUILDCAT_BUILDD_URL }}
      BUILDCAT_CERTS_DIR: certs
      REPO: ${{ github.repository }}
      # no S3 config here — the cold cache is a buildd policy, returned by /route and applied automatically
    steps:
      - uses: actions/checkout@v4
      - run: |                                   # client mTLS material from repo secrets (base64)
          mkdir -p certs
          printf '%s' "${{ secrets.BUILDCAT_CA }}"   | base64 -d > certs/ca.pem
          printf '%s' "${{ secrets.BUILDCAT_CERT }}" | base64 -d > certs/cert.pem
          printf '%s' "${{ secrets.BUILDCAT_KEY }}"  | base64 -d > certs/key.pem
      - uses: docker/setup-buildx-action@v3
      - run: sh build.sh -t buildcat-example:${{ github.sha }} .
```

The repo also carries a `.gitlab-ci.yml` calling the **same** `build.sh` — the proof the integration
is CI-agnostic. A green run on the hosted runner (`28126430796`) routed to the example daemon, built,
and exercised the S3 cache.

## Public exposure (why and how)

A hosted runner is **outside** the cluster, so buildcat must be reachable over the internet — exactly
like `buildkit-service` (public LB + mTLS). **Two** LoadBalancers are exposed, and only two
regardless of how many projects exist:

| Endpoint | Service | Purpose |
|---|---|---|
| `BUILDCAT_BUILDD_URL` | `buildcat-buildd` LoadBalancer `:8080` | the `/route` API |
| `tcp://<daemon>.<gateway-host>:1234` | the **shared SNI gateway** LoadBalancer `:1234` | the build, over mTLS, to any daemon |

Daemons themselves stay **`ClusterIP`**. buildd is started with `--gateway-host <domain>` (Helm
`gateway.host`), so `/route` returns the deterministic hostname `tcp://<daemon>.<gateway-host>:1234`;
the single gateway LB peeks the TLS SNI and pipes to the daemon's ClusterIP Service — mTLS stays
end-to-end. This needs a **wildcard DNS** record `*.<gateway-host>` → the gateway LB (see
[architecture.md](architecture.md#the-shared-sni-gateway-off-cluster-ci)).

### The certificate SAN requirement

mTLS validates the daemon's hostname, so the **daemon certificate's SAN must cover the address the
runner dials** — which, through the gateway, is `<daemon>.<gateway-host>`. The cert script
([`deploy/cert/create-certs.sh`](../deploy/cert)) bakes in `*.buildcat.svc`; for public exposure run
it with `GATEWAY_HOST=<gateway-host>` so it also adds the **wildcard SAN `*.<gateway-host>`** (one
cert validates every daemon's SNI hostname). If the SAN is wrong you get TLS validation failures or
`context deadline exceeded`. (The gateway terminates no TLS, so it needs no cert of its own — it only
peeks the SNI; the trust stays end-to-end between the client cert and the daemon cert.)

### S3 from CI — zero client config

The cold cache is a **buildd policy**, so a CI caller configures **no** S3 at all (no flags, no env,
no secrets). When buildd is set up with a bucket (`--s3-bucket …`), `/route` returns the per-project
cache reference (bucket/region/endpoint, prefix = the project key, **no credentials**) and the
client adds `--cache-from/--cache-to type=s3` automatically. The **daemon** performs the S3 I/O, so:

- the endpoint can be **in-cluster** (`http://minio.buildcat.svc:9000`) — unreachable from the
  runner, yet it works, because the in-cluster daemon connects;
- the AWS creds live on the **daemon pods** (a k8s Secret via `--s3-creds-secret`), never on the
  runner and never on the wire.

The example CI log shows the daemon doing it: `importing cache manifest from s3:…`. See
[storage-and-cold-cache.md](storage-and-cold-cache.md).

## Live endpoints (this deployment, `ovh-dev`)

- buildd `/route`: `http://57.128.55.102:8080`
- shared SNI gateway LB `:1234` — fronts every daemon; `/route` returns
  `tcp://<daemon>.<gateway-host>:1234` (e.g. `buildkitd-pa081c22c974da132.<gateway-host>` for
  `SocialGouv/buildcat-example` → key `pa081c22c974da132`)
- S3 (in-cluster MinIO, proof backend): `http://minio.buildcat.svc:9000`, bucket `buildcache`
