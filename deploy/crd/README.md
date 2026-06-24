# deploy/crd — generated CRDs

This directory is the `controller-gen` output target for buildcat's CRDs. It is
populated by:

```bash
make manifests
```

which runs `controller-gen crd ... output:crd:artifacts:config=deploy/crd`
(see the project `Makefile`). Expect:

- `buildcat.dev_buildprojects.yaml`
- `buildcat.dev_builds.yaml`

Apply them directly with `kubectl apply -f deploy/crd`, or copy them into
`deploy/helm/buildcat/crds/` to have Helm install them with the chart (see that
directory's README).
