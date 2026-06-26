# deploy/crd — generated CRDs

This directory is the `controller-gen` output target for buildkit-operator's CRDs. It is
populated by:

```bash
task manifests
```

which runs `controller-gen crd ... output:crd:artifacts:config=deploy/crd`
(see the project `Taskfile`). Expect:

- `buildkit-operator.socialgouv.github.io_buildprojects.yaml`
- `buildkit-operator.socialgouv.github.io_builds.yaml`

Apply them directly with `kubectl apply -f deploy/crd`, or copy them into
`deploy/helm/buildkit-operator/crds/` to have Helm install them with the chart (see that
directory's README).
