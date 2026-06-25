# Contributing

Thanks for helping improve buildkit-operator. This is the control plane (routing + lifecycle) over
**vanilla** buildkit/containerd — no fork, no custom snapshotter. Keep changes within that boundary.

## Development

- **Go** 1.24+. The control plane is in `cmd/` (`buildd`, `companion`, `gateway`, `build`),
  `internal/` (controller, builder, router, metrics) and `api/v1alpha1` (CRD types).
- Common targets:
  ```bash
  make manifests   # regenerate CRDs + RBAC from the +kubebuilder markers (commit the result)
  make test        # go test ./...
  go vet ./...
  ```
- After editing `api/v1alpha1` or the `+kubebuilder:` markers, run `make manifests` and commit the
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
- CI builds the images and publishes docs; make sure `make test` and `go vet` pass locally first.
- This is a new project — no backward-compat constraints; prefer clean over additive.

## Releasing

Releases are cut by maintainers via the [`release`](.github/workflows/release.yml) workflow
(release-it: version bump, changelog, git tag, OCI Helm chart push; the `v*` tag then triggers the
matching semver images).
