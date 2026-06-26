# Contributing

Thanks for helping improve buildkit-operator. This is the control plane (routing + lifecycle) over
**vanilla** buildkit/containerd — no fork, no custom snapshotter. Keep changes within that boundary.

### Prerequisites

- **[devbox](https://www.jetify.com/devbox)** (required) — the single dependency you install by hand.
  It provides the whole pinned toolchain from `devbox.json` + `devbox.lock`: go, node LTS (24.x) +
  **pnpm via corepack**, kubectl, helm, jq, cosign. (`controller-gen` and `golangci-lint` are not in
  devbox — they stay pinned via `go run …@version` in the `Taskfile.yml`.)
- **[direnv](https://direnv.net)** (optional) — auto-loads the devbox env when you `cd` into the repo
  (the committed `.envrc` was produced by `devbox generate direnv`; run `direnv allow` once). Without
  it, run `devbox shell` for an interactive shell, or prefix one-off commands with `devbox run -- <cmd>`.

> Without devbox at all you must provide the toolchain yourself: **Go 1.26+**, and for releases
> **Node ≥22.13 + pnpm 11** (via corepack). Dependency updates are automated by Renovate
> (`.github/renovate.json5`).

### Working on the code

- The control plane is in `cmd/` (`buildd`, `companion`, `gateway`, `build`), `internal/` (controller,
  builder, router, metrics) and `api/v1alpha1` (CRD types).
- Common tasks via [go-task](https://taskfile.dev) (prefix with `devbox run -- ` if not in a
  devbox/direnv shell); `task --list` shows them all:
  ```bash
  task manifests   # regenerate CRDs + RBAC from the +kubebuilder markers (commit the result)
  task test        # go test ./...
  task vet         # go vet ./...
  ```
- After editing `api/v1alpha1` or the `+kubebuilder:` markers, run `task manifests` and commit the
  regenerated `deploy/crd` + chart `crds/` so they stay in sync.
- Helm chart lives in `deploy/helm/buildkit-operator`; validate with `helm lint` and
  `helm template deploy/helm/buildkit-operator`.

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`,
  `docs:`, `chore:`…) — release-it derives the changelog and version bump from them.
- Match the surrounding code: comment density, naming, idiom. Prefer the smallest change that fits.
- Add/extend tests next to the code (`*_test.go`) for behavioural changes.

## Pull requests

- Keep PRs focused; describe the why, not just the what.
- CI builds the images and publishes docs; make sure `task test` and `task vet` pass locally first.
- This is a new project — no backward-compat constraints; prefer clean over additive.

## Releasing

Releases are cut by maintainers via the [`release`](.github/workflows/release.yml) workflow
(release-it: version bump, changelog, git tag, OCI Helm chart push; the `v*` tag then triggers the
matching semver images). Release tooling runs on pnpm (provisioned by corepack) —
`devbox run -- pnpm install --frozen-lockfile` then `devbox run -- pnpm exec release-it`.
