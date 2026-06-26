# End-to-end tests (live cluster)

These tests exercise the **real** build path — `scripts/build.sh` → `buildd /route` → a remote
`buildkitd` over mTLS — against a **running** buildkit-operator and assert both the build output and the
resulting cluster state. They cover every shipped feature: routing, warm cache, S3 cold cache, cache
mounts, **untrusted-fork isolation in a Kata microVM**, SLSA provenance + SBOM, `/prewarm` readiness,
durable VolumeSnapshots, Prometheus metrics, HA, and scale-to-zero.

They are gated behind the `e2e` build tag **and** skip unless `BKO_E2E_BUILDD_URL` is set, so
`go test ./...` (unit) never runs them.

## Requirements

- A reachable buildkit-operator: `buildd /route` URL + a kubeconfig/context for its cluster.
- On the runner: `docker buildx`, `sh`, `curl`, `jq`, `socat` (what `build.sh` needs) and `kubectl`.
- For untrusted/Kata: the cluster must actually have the `kata-clh` RuntimeClass on a node, and the
  operator must be deployed with `sandbox.runtimeClass=kata-clh`.

## Configuration (env)

| Var | Required | Default | Meaning |
|---|---|---|---|
| `BKO_E2E_BUILDD_URL` | ✅ | — | buildd `/route` API (e.g. `https://buildd.bko.fabrique.social.gouv.fr`) |
| `KUBECONFIG` | ✅ | — | kubeconfig used for the cluster assertions |
| `BKO_E2E_CONTEXT` | | `ovh-prod` | kube context to use |
| `BKO_E2E_GATEWAY_HOST` | | — | off-cluster SNI host (e.g. `bko.fabrique.social.gouv.fr`) |
| `BKO_E2E_OPERATOR_NS` | | `buildkit-operator` | control-plane namespace |
| `BKO_E2E_BUILDS_NS` | | `buildkit-builds` | daemons namespace |
| `BKO_E2E_CERTS_DIR` | | `deploy/cert/.certs/client` | dir with `ca.pem`/`cert.pem`/`key.pem` |
| `BKO_E2E_AUTH_SECRET` | | `buildkit-operator-auth` | bearer-token Secret (key `token`); read from the cluster |
| `BKO_E2E_PUSH_IMAGE` | | — | writable registry ref; enables the provenance/SBOM push test (else skipped) |

The bearer token is read from the cluster Secret, so you don't pass it by hand.

## Run

```sh
export KUBECONFIG=/path/to/kubeconfig
export BKO_E2E_BUILDD_URL=https://buildd.bko.fabrique.social.gouv.fr
export BKO_E2E_GATEWAY_HOST=bko.fabrique.social.gouv.fr
# optional: exercise the supply-chain push test
export BKO_E2E_PUSH_IMAGE=ghcr.io/<you>/bko-e2e:test

devbox run -- task test:e2e
# or a single feature:
devbox run -- task test:e2e -- -run TestUntrustedKataIsolation
```

Expect **10–20 min**: several subtests cold-start a fresh daemon (PVC provision), and the Kata fork
boots a microVM under nested virt. Each subtest cleans up the BuildProjects (and snapshots) it creates.
