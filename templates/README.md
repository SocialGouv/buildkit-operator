# buildkit-operator — reusable GitLab CI/CD components

Importable CI bricks to build container images through buildkit-operator from **GitLab CI**, the
counterpart to the [GitHub Action](../action.yml). The build runs on the project's hot remote
`buildkitd` (warm cache + optional S3 cold cache), so the job needs only `docker buildx + curl + jq` —
**no privileged `docker:dind`**.

| Component | What it does |
|---|---|
| [`build.yml`](build.yml) | Route → `docker buildx build` against the project's daemon over mTLS (warm/cold cache, monorepo-aware, optional push/provenance/SBOM/cosign). |

## 1. Set the CI/CD variables (once, at the group level)

The mTLS material is **secret** — pass it as **masked/File** CI/CD variables, not as component inputs
(inputs are visible in the config). The `/route` credential, by contrast, is no longer a shared token:
the component auto-mints a GitLab OIDC id_token (`BUILDKIT_OPERATOR_ID_TOKEN`, via an `id_tokens:` entry
with audience from the `oidc_audience` input, default `buildkit-operator`) and sends it as the `/route`
credential — nothing to distribute. buildd verifies the forge-signed JWT and derives `repo`/`untrusted`
from the verified claim. Mirror of the GitHub org secrets:

| Variable | Value |
|---|---|
| `BUILDKIT_OPERATOR_BUILDD_URL` | buildd `/route` API URL (e.g. `http://<lb-ip>:8080`) |
| `BUILDKIT_OPERATOR_CA` / `_CERT` / `_KEY` | client mTLS CA / cert / key (PEM) |
| `BUILDKIT_OPERATOR_ADMIN_TOKEN` | *(optional)* break-glass admin token (sent as `X-Buildkit-Operator-Admin-Token`) — bypasses OIDC and trusts the request's `repo`/`untrusted`; for non-OIDC / in-cluster ops. A legacy shared `BUILDKIT_OPERATOR_TOKEN` bearer is still honoured when buildd has no OIDC providers configured. |
| `BUILDKIT_OPERATOR_GATEWAY_IP` | *(optional)* gateway LoadBalancer IP — maps the SNI host when there is no wildcard DNS yet |
| `BUILDKIT_OPERATOR_HTTP_PROXY` | *(optional)* `host:port` of an HTTP CONNECT proxy — for **egress-proxy-only** runners (see below) |
| `BUILDKIT_OPERATOR_TUNNEL` | *(optional)* `1` to tunnel the daemon connection through the proxy |

## 2. Include the component

GitHub-hosted (remote include — needs the GitLab server to allow GitHub egress; see the note below):

```yaml
include:
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v0.8.3/templates/build.yml"
    inputs:
      tags: "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA"
      push: "true"
```

Or, if the repo is mirrored into a GitLab instance (CI/CD Catalog component):

```yaml
include:
  - component: "$CI_SERVER_FQDN/<mirror-path>/build@0.6.0"
    inputs: { tags: "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA", push: "true" }
```

That generates a `buildkit-operator-build` job in the `build` stage.

> **Locked-down GitLab (allow-list / no GitHub egress).** `include: remote:` is fetched by the GitLab
> **server**, which may deny external hosts (`URL is blocked: ... not on the Allow List` → a pipeline with
> **0 jobs**; confirm with `POST /projects/:id/ci/lint`). There, **mirror the repo** and use the Catalog
> `component:` form above, or **vendor** `templates/build.yml` + `scripts/build.sh` into your repo and
> `include: local:` them. Likewise, if the runner's egress proxy doesn't allow `raw.githubusercontent.com`,
> vendor `build.sh` rather than letting the job fetch it. (This is how the SocialGouv PIC is wired.)

## Inputs

`tags` is the only required input. Everything else has a sensible default (see
[`build.yml`](build.yml) `spec.inputs`):

`job_name`, `stage`, `image`, `repo` (default `$CI_PROJECT_URL` — buildd overwrites it with the verified
OIDC `project_path` claim, so it is advisory under OIDC), `oidc_audience` (default `buildkit-operator` —
the audience of the auto-minted `/route` id_token; must match a buildd `oidc.providers` entry), `name`
(monorepo component), `arch` (`amd64`/`arm64`), `context`, `dockerfile`, `target`, `untrusted`, `push`,
`provenance`, `sbom`, `sign`, `ref` (the exact buildkit-operator tag/commit the build script is fetched
from — keep it pinned and in sync with the include).

## Examples

**Build + push on the default branch, monorepo component, with provenance & SBOM:**

```yaml
include:
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v0.8.3/templates/build.yml"
    inputs:
      tags: "$CI_REGISTRY_IMAGE/api:$CI_COMMIT_SHORT_SHA"
      name: api                  # monorepo: its own daemon + cache
      target: runtime
      push: "true"
      provenance: "mode=max"
      sbom: "true"
      sign: "true"               # cosign keyless (uses GitLab OIDC id_tokens)
```

**Untrusted build for merge requests** (ephemeral daemon, no write-back to the shared cache) — gate it
by re-declaring the job's `rules:` (GitLab merges includes):

```yaml
include:
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v0.8.3/templates/build.yml"
    inputs:
      tags: "$CI_REGISTRY_IMAGE:mr-$CI_MERGE_REQUEST_IID"
      untrusted: "true"
      push: "false"

buildkit-operator-build:
  rules:
    - if: "$CI_PIPELINE_SOURCE == 'merge_request_event'"
```

**Pre-warm on push** (mask the cold-start attach for the build that follows) — a one-liner, no
component needed:

```yaml
prewarm:
  stage: .pre
  image: curlimages/curl
  id_tokens:
    BUILDKIT_OPERATOR_ID_TOKEN: { aud: buildkit-operator }
  script:
    # the token is the OIDC id_token (buildd derives repo from its verified claim); use
    # `-H "X-Buildkit-Operator-Admin-Token: $BUILDKIT_OPERATOR_ADMIN_TOKEN"` for break-glass instead.
    - >
      curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL/prewarm"
      -H "authorization: Bearer $BUILDKIT_OPERATOR_ID_TOKEN" -H 'content-type: application/json'
      -d "{\"arch\":\"amd64\"}"
```

## Behind an egress proxy (e.g. the SocialGouv PIC)

Some CI platforms only let runners egress through an **HTTP CONNECT proxy on 443** — they cannot dial
an arbitrary IP:port. To use buildkit-operator from there:

1. **Expose buildd + gateway on 443** (chart, on the cluster side):
   - gateway on 443: `--set gateway.externalPort=443` (it still dials daemons on the internal mTLS
     port); point wildcard DNS `*.<gateway.host>` at the gateway LB.
   - buildd `/route` behind a TLS Ingress on 443: `--set ingress.enabled=true --set
     ingress.host=buildd.<domain>` plus auth on `/route` — recommended `--set oidc.providers[0].type=gitlab`
     (verifies the auto-minted id_token); the legacy `--set auth.tokenSecret=<secret>` bearer is the
     fallback for OIDC-less setups (buildd speaks HTTP; the Ingress terminates TLS). Use a public
     hostname the proxy will `CONNECT` to.
2. **Set the proxy CI/CD variables** (in addition to the mTLS ones):
   - `BUILDKIT_OPERATOR_BUILDD_URL` = `https://buildd.<domain>` (the Ingress, 443)
   - `BUILDKIT_OPERATOR_HTTP_PROXY` = `host:port` of the CONNECT proxy
   - `BUILDKIT_OPERATOR_TUNNEL` = `1`
   - `BUILDKIT_OPERATOR_GATEWAY_HOST` = `<domain>` + (optional) `BUILDKIT_OPERATOR_GATEWAY_PORT=443` —
     when the gateway is **multi-domain** (`gateway.extraDomains`) and this platform reaches it under a
     different domain than buildd advertises; the client rebuilds the endpoint from the daemon key.

The job then routes `apk`/`curl(/route)` through the proxy and **socat-tunnels** the daemon TCP through
the same proxy; mTLS is validated against the real daemon hostname (`servername`), so it stays
end-to-end. This mirrors the shared `buildkit-service` `.build-buildkit-service` backend, with the
control-plane `/route` step added.

**Cold start vs. the proxy's idle timeout.** A CONNECT proxy caps how long an idle tunnel stays open
(~50s), but a daemon's first build cold-starts in ≈ 1–2 min — a single blocking call would drop with
`SSL_read: unexpected eof`. So when `BUILDKIT_OPERATOR_TUNNEL=1`, `build.sh` first **polls `/prewarm`**
— which returns immediately with a `ready` flag — until the daemon is warm, then routes. No request is
ever held open past the proxy timeout, and a daemon that never warms fails loudly at the deadline. (A
bounded `/route` poll stays as a backstop.) Tunables (sane defaults): `BUILDKIT_OPERATOR_ROUTE_INTERVAL`
(`5`s poll), `BUILDKIT_OPERATOR_ROUTE_DEADLINE` (`900`s), `BUILDKIT_OPERATOR_ROUTE_TIMEOUT` (per-`/route`
attempt cap, default `40`s when tunnelling — keep it under the proxy's timeout).

## Notes

- **No `docker:dind`.** The remote buildx driver connects straight to the in-cluster daemon over mTLS;
  the GitLab runner does the orchestration, not the building. No privileged service, no nested Docker.
- **Cache is automatic.** `/route` returns the project's daemon endpoint and, when buildd has an S3
  cold cache configured, the per-project cache reference — the job applies `--cache-from/--cache-to`
  with **no** S3 credentials on the runner (the daemon holds them). Warm builds reuse the daemon's hot
  PVC cache transparently.
- **`target` is part of the cache key** — set it so two Dockerfile targets of one repo don't collide on
  one daemon.
- **Single source of truth.** The component fetches and runs the same [`scripts/build.sh`](../scripts/build.sh)
  the GitHub Action runs, pinned to an exact `ref` — there is no duplicated build logic.

See [docs/ci-integration.md](../docs/ci-integration.md) for the end-to-end model (mTLS SAN, gateway,
S3 cold cache) and [docs/onboarding.md](../docs/onboarding.md) for the GitHub equivalent.
