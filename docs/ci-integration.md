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

# 3. build — optionally with the S3 cold cache (see below)
exec docker buildx build --builder buildcat $extra "$@"
```

Environment:

| Var | Meaning |
|---|---|
| `BUILDCAT_BUILDD_URL` | external buildd `/route` endpoint (LB/Ingress) |
| `BUILDCAT_CERTS_DIR` | dir holding `ca.pem cert.pem key.pem` (client mTLS material) |
| `REPO` / `ARCH` | project identity (defaults: git origin / `amd64`) |
| `BUILDCAT_S3_*` | optional S3 cold cache (no-op if `BUILDCAT_S3_BUCKET` unset) |

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
      BUILDCAT_S3_BUCKET: ${{ vars.BUILDCAT_S3_BUCKET }}     # optional cold cache
      BUILDCAT_S3_ENDPOINT: ${{ vars.BUILDCAT_S3_ENDPOINT }}
      BUILDCAT_S3_KEY: ${{ secrets.BUILDCAT_S3_KEY }}
      BUILDCAT_S3_SECRET: ${{ secrets.BUILDCAT_S3_SECRET }}
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
is CI-agnostic. A green run on the hosted runner (`28126430796`) routed to
`tcp://135.125.57.6:1234`, built, and exercised the S3 cache.

## Public exposure (why and how)

A hosted runner is **outside** the cluster, so buildcat must be reachable over the internet — exactly
like `buildkit-service` (public LB + mTLS). Two endpoints are exposed:

| Endpoint | Service | Purpose |
|---|---|---|
| `BUILDCAT_BUILDD_URL` | `buildcat-buildd` LoadBalancer `:8080` | the `/route` API |
| daemon endpoint | `buildkitd-<key>` LoadBalancer `:1234` | the build, over mTLS |

This is **gateway mode** (`--daemon-service-type=LoadBalancer`): buildd makes each daemon Service a
LoadBalancer and `/route` returns its **external** ingress IP (see
[architecture.md](architecture.md#public-gateway-mode)).

### The certificate SAN requirement

mTLS validates the daemon's hostname, so the **daemon certificate's SAN must cover the public
address** the runner dials. The cert script ([`deploy/cert/create-certs.sh`](../deploy/cert)) bakes
in `*.buildcat.svc`; for public exposure also include the LB IP/DNS (and `127.0.0.1` if you
port-forward for tests). If the SAN is wrong you get TLS validation failures or
`context deadline exceeded` against a name that doesn't resolve/validate. When the public DNS name
isn't resolvable from the runner, dial the LB IP directly and override `servername` via the buildx
driver-opt so the cert's DNS SAN still validates.

### S3 from CI — the runner never touches S3

When `BUILDCAT_S3_*` is set, `build.sh` adds `--cache-from/--cache-to type=s3`. Crucially the
**daemon** performs the S3 I/O, so:

- the endpoint can be **in-cluster** (`http://minio.buildcat.svc:9000`) — unreachable from the
  runner, yet it works, because the in-cluster daemon connects;
- S3 creds are CI secrets attached to the build, never on the daemon.

The example CI log shows the daemon doing it: `importing cache manifest from s3:…`. See
[storage-and-cold-cache.md](storage-and-cold-cache.md).

## Live endpoints (this deployment, `ovh-dev`)

- buildd `/route`: `http://57.128.55.102:8080`
- example daemon (`SocialGouv/buildcat-example` → key `pa081c22c974da132`): `tcp://135.125.57.6:1234`
- S3 (in-cluster MinIO, proof backend): `http://minio.buildcat.svc:9000`, bucket `buildcache`
