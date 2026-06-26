# buildkit-operator — reusable GitLab CI/CD components

Importable CI bricks to build container images through buildkit-operator from **GitLab CI**, the
counterpart to the [GitHub Action](../action.yml). The build runs on the project's hot remote
`buildkitd` (warm cache + optional S3 cold cache), so the job needs only `docker buildx + curl + jq` —
**no privileged `docker:dind`**.

| Component | What it does |
|---|---|
| [`build.yml`](build.yml) | Route → `docker buildx build` against the project's daemon over mTLS (warm/cold cache, monorepo-aware, optional push/provenance/SBOM/cosign). |

## 1. Set the CI/CD variables (once, at the group level)

The mTLS material + bearer token are **secrets** — pass them as **masked/File** CI/CD variables, not
as component inputs (inputs are visible in the config). Mirror of the GitHub org secrets:

| Variable | Value |
|---|---|
| `BUILDKIT_OPERATOR_BUILDD_URL` | buildd `/route` API URL (e.g. `http://<lb-ip>:8080`) |
| `BUILDKIT_OPERATOR_CA` / `_CERT` / `_KEY` | client mTLS CA / cert / key (PEM) |
| `BUILDKIT_OPERATOR_TOKEN` | bearer token for `/route` (when buildd is exposed with auth) |
| `BUILDKIT_OPERATOR_GATEWAY_IP` | *(optional)* gateway LoadBalancer IP — maps the SNI host when there is no wildcard DNS yet |

## 2. Include the component

GitHub-hosted (remote include — works today):

```yaml
include:
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v1/templates/build.yml"
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

## Inputs

`tags` is the only required input. Everything else has a sensible default (see
[`build.yml`](build.yml) `spec.inputs`):

`job_name`, `stage`, `image`, `repo` (default `$CI_PROJECT_URL` = the cache key), `name` (monorepo
component), `arch` (`amd64`/`arm64`), `context`, `dockerfile`, `target`, `untrusted`, `push`,
`provenance`, `sbom`, `sign`, `ref` (the buildkit-operator git ref the build script is fetched from —
keep it in sync with the include).

## Examples

**Build + push on the default branch, monorepo component, with provenance & SBOM:**

```yaml
include:
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v1/templates/build.yml"
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
  - remote: "https://raw.githubusercontent.com/SocialGouv/buildkit-operator/v1/templates/build.yml"
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
  script:
    - >
      curl -fsS -XPOST "$BUILDKIT_OPERATOR_BUILDD_URL/prewarm"
      -H "authorization: Bearer $BUILDKIT_OPERATOR_TOKEN" -H 'content-type: application/json'
      -d "{\"repo\":\"$CI_PROJECT_URL\",\"arch\":\"amd64\"}"
```

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
  the GitHub Action runs, pinned to `ref` — there is no duplicated build logic.

See [docs/ci-integration.md](../docs/ci-integration.md) for the end-to-end model (mTLS SAN, gateway,
S3 cold cache) and [docs/onboarding.md](../docs/onboarding.md) for the GitHub equivalent.
