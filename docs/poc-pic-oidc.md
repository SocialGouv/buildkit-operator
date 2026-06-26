# PoC — build from the PIC with OIDC identity (method)

The reproducible method behind the PoC: build a real project **from the SocialGouv PIC** GitLab
(`pic.sg.social.gouv.fr`, egress-proxy-only) through buildkit-operator, and move `/route` auth from a
shared bearer to **OIDC** (per-build, forge-signed identity). See [security.md](security.md) for the
auth model and [ci-integration.md](ci-integration.md) for the client patterns.

> **Status: validated end-to-end (2026-06-26).** The example app builds from PIC project 548; prod buildd
> is on the OIDC release (`helm` rev 21, `OIDC identity verification ENABLED {providers: 2}`); the PIC
> pipeline authenticates via the **GitLab OIDC id_token** (buildd log: `route … repo:
> pic.sg.social.gouv.fr/socialgouv/produits-dnum/…/buildkit-operator-test untrusted:false`, no
> bearer-fallback line), with the build running on the remote daemon and exporting the S3 cache.

## 1. The PIC build brick (reusable)

PIC constraints (see [pic-gitlab-egress-constraints](../docs/ci-integration.md)): the GitLab server
blocks `include: remote:` and runners egress **only** through a CONNECT proxy on 443. So the build is
**self-contained**: vendor `scripts/build.sh` as `ci/build.sh` and run it inline.

Layout of a PIC project (e.g. the test project `socialgouv/produits-dnum/studio-tech/architecture/buildkit-operator-test`, id 548):

```
Dockerfile, <app files>     # the thing to build (here the buildkit-operator-example node app)
ci/build.sh                 # vendored copy of buildkit-operator scripts/build.sh (single source)
.gitlab-ci.yml              # inline job: export proxy -> apk curl/jq/socat -> sh ci/build.sh
```

`.gitlab-ci.yml` (essentials):

```yaml
buildkit-operator-build:
  image: docker:27
  variables:
    REPO: "$CI_PROJECT_URL"          # cache key (normalized by the router)
    TAGS: "$CI_REGISTRY_IMAGE:test-$CI_COMMIT_SHORT_SHA"
    PUSH: "false"                    # validates route+tunnel+remote build+cache; no registry login
  before_script:
    - |
      if [ -n "${BUILDKIT_OPERATOR_HTTP_PROXY:-}" ]; then export http_proxy=... https_proxy=... ; fi
      command -v curl >/dev/null && command -v jq >/dev/null && command -v socat >/dev/null || apk add --no-cache curl jq socat
  script:
    - sh ci/build.sh
```

Project CI/CD variables (set once, project-level):
`BUILDKIT_OPERATOR_BUILDD_URL` (the buildd **Ingress**, `https://buildd.bko.fabrique.social.gouv.fr`),
`BUILDKIT_OPERATOR_GATEWAY_HOST` (`gw.bko.ovh`), `BUILDKIT_OPERATOR_TUNNEL=1`,
`BUILDKIT_OPERATOR_HTTP_PROXY` (the PIC CONNECT proxy), and masked `BUILDKIT_OPERATOR_TOKEN` +
mTLS `BUILDKIT_OPERATOR_CA`/`CERT`/`KEY`.

`build.sh` does the rest: poll `/prewarm` (never holds a call past the proxy idle timeout), `/route` via
the Ingress, socat-tunnel the daemon mTLS stream through the proxy, `buildx build` on the remote warm
daemon, S3 cache export, `/complete`. **No `docker:dind`.**

**Status:** green — example app on project 548, pipeline succeeds in ~1 min; trace shows
`route -> remote buildkitd -> exporting cache to Amazon S3 -> Job succeeded`.

## 2. Auth: bearer today, OIDC in one switch

`BUILDKIT_OPERATOR_TOKEN` is the `/route` credential. Today it is the **legacy shared bearer** (works
against the currently-live buildd). To move a project to **OIDC** (once buildd verifies tokens):

```yaml
# in .gitlab-ci.yml
buildkit-operator-build:
  id_tokens:
    BUILDKIT_OPERATOR_ID_TOKEN: { aud: buildkit-operator }
  variables:
    BUILDKIT_OPERATOR_TOKEN: "$BUILDKIT_OPERATOR_ID_TOKEN"
```

GitLab signs the token and injects it (no extra runner egress); buildd verifies it and **binds the build
to this project's `project_path`** — a leaked credential can no longer impersonate another repo. GitHub
Actions is the same idea with `permissions: id-token: write` (the Action mints it).

> **GOTCHA — GitLab variable precedence.** A **project/group CI/CD variable** `BUILDKIT_OPERATOR_TOKEN`
> OVERRIDES the `.gitlab-ci.yml` `variables:` value, so setting `BUILDKIT_OPERATOR_TOKEN:
> "$BUILDKIT_OPERATOR_ID_TOKEN"` in the YAML is silently ignored while the project var (the bearer)
> exists — the build keeps using the bearer (visible as `legacy-bearer fallback` in buildd logs). To
> actually switch a project to OIDC, **delete the project-level `BUILDKIT_OPERATOR_TOKEN` variable** so
> the `.gitlab-ci.yml` id_token wins (this is also "Step C" / strict OIDC for that project).

## 3. OIDC rollout method (staged, zero-downtime)

1. **Code** (buildcat `main`, image `sha-<commit>`): `/route` verifies a forge OIDC JWT and overwrites
   the client repo with the verified claim; `oidc.providers` (github + gitlab + forgejo); a **legacy
   bearer fallback** stays valid while `auth.tokenSecret` is set, so consumers migrate with no downtime.
2. **GitOps** (`infra-apps/applications/buildkit-operator.yaml`): bump image + `oidc.providers` +
   `repoAllowlist`, buildd `service.type: ClusterIP` behind the TLS Ingress, hard-pin daemons to
   `nodepool: prod-build`, `gateway.maxConns`, `forkEgressStrict: false` (Kata is the fork boundary).
   **Keep the live gateway config** (`externalPort: 443`, `extraDomains`, the external-dns wildcard
   annotation) — it was live-patched onto the hand-deployed release; dropping it breaks the SNI gateway.
3. **Go-live** = apply that desired state to prod. The ovh-prod release is **hand-deployed via Helm**
   (release `buildkit-operator`, the buildd Ingress is already helm-managed), and the GitOps
   ApplicationSet is not yet Argo-adopted (the `applications` app-of-apps is shared across teams — do not
   sync it just for this). So the isolated path is a **`helm upgrade`** of that release to the new chart
   with the GitOps values — verify the values diff is ONLY the intended hardening first (we found and
   fixed gateway-config drops this way), use `--atomic` for auto-rollback. This is a production deploy:
   it needs explicit authorization. *(Done: rev 21, buildd `sha-b346209`, buildd Service → ClusterIP,
   `OIDC … ENABLED {providers: 2, allowlistSize: 3}`.)* Rollback if needed: `helm -n buildkit-operator
   rollback buildkit-operator 20`.
4. **Flip consumers** to OIDC (§2) and re-run — validates the OIDC path end-to-end. *(Done for PIC 548:
   deleted the project bearer var, re-ran → OIDC-verified, no fallback.)*
5. **Strict** (Step C): once all consumers mint tokens, drop `auth.tokenSecret` so the global bearer is
   no longer accepted. *(Done per-project for 548 by deleting its bearer var; fleet-wide = drop
   `auth.tokenSecret` from the release/GitOps once GitHub consumers also mint tokens.)*

## Gotchas found
- The hand-deployed prod values **drift** from GitOps (image tag, gateway externalPort/extraDomains,
  external-dns annotation, `namespace` vs `namespaces`). Always diff `helm get values` against the
  intended values before upgrading; a blind apply drops live config.
- The buildd `/route` LoadBalancer serves plain HTTP — the new chart refuses it without an IP allowlist;
  the durable answer is ClusterIP + the existing nginx TLS Ingress (PIC reaches it on 443).
- Enabling `oidc.providers` makes buildd require a token from every caller — the bearer fallback is what
  makes the cutover safe; remove `auth.tokenSecret` only after consumers migrate.
