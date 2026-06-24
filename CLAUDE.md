# buildcat

Service de build **BuildKit distribué** : un `buildkitd` **chaud par `(projet, arch)`** sur OVH Managed Kubernetes. Concurrence et partage de cache (layers + cache mounts) sont **internes au daemon** ; la valeur ajoutée est un **plan de contrôle** (routage + cycle de vie) au-dessus de buildkit/containerd **VANILLA** — aucun fork, aucun snapshotter custom.

## Repères
- **Plans** : `.plans/` (dossier local, gitignoré). Plan retenu = `plan-implementation-buildkit-distribue-ovh.md` ; prérequis = `bench-phase0-cinder-ovh.md` → `results.md`. `distributed-snapshotter-plan.md` = alternative ambitieuse **écartée** (non-objectif).
- **Source buildkit** vendue dans `.repos/buildkit` (gitignoré). Réf. M1 : `examples/kubernetes/{create-certs.sh, statefulset.rootless.yaml, consistenthash/}`.

## Tests k8s
- Context **ovh-dev** (région GRA9, cluster **partagé** ⇒ toujours un **namespace dédié** + cleanup systématique).
- `KUBECONFIG=/home/jo/lab/fabrique/devops-agent-as-markdowns/.secrets/plateform-fabrique/kubeconfig` — **utiliser, jamais `cat`** (cf. rule globale secrets).
- Toujours pinner `--context ovh-dev` : le kubeconfig contient aussi `ovh-prod` (ne jamais cibler prod pour des tests).
