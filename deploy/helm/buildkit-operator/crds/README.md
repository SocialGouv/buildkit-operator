# CRDs (generated)

Helm installs every `*.yaml` it finds in this `crds/` directory **before** the
rest of the chart, and (by Helm's CRD convention) does **not** template, upgrade,
or delete them. That is exactly what we want for buildkit-operator's CustomResource-
Definitions: install-once, managed out of band.

These CRD YAMLs are **generated**, not hand-written. They come from the
kubebuilder markers on the API types (`api/v1alpha1/*_types.go`):

```bash
task manifests
```

`task manifests` runs `controller-gen crd ...` and writes the CRDs into
`deploy/crd/` (see the project `Taskfile`). To package them with the chart, copy
the generated files here before `helm install`/`helm package`:

```bash
task manifests
cp deploy/crd/*.yaml deploy/helm/buildkit-operator/crds/
```

Expected CRDs:

- `buildkit-operator.socialgouv.github.io_buildprojects.yaml`
- `buildkit-operator.socialgouv.github.io_builds.yaml`

This directory intentionally ships only this README in version control; the
generated CRD YAMLs are produced by the build, mirroring how `deploy/crd/` is
populated. If you prefer to keep CRD lifecycle fully manual, skip the copy and
`kubectl apply -f deploy/crd` instead (the chart will simply find no CRDs here).
