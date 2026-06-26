# buildkit-operator

Service de build **BuildKit distribué** : un `buildkitd` **chaud par `(projet, arch)`** sur OVH Managed Kubernetes. Concurrence et partage de cache (layers + cache mounts) sont **internes au daemon** ; la valeur ajoutée est un **plan de contrôle** (routage + cycle de vie) au-dessus de buildkit/containerd **VANILLA** — aucun fork, aucun snapshotter custom.

## Repères
- **Plans** : `.plans/` (dossier local, gitignoré). Plan retenu = `plan-implementation-buildkit-distribue-ovh.md` ; prérequis = `bench-phase0-cinder-ovh.md` → `results.md`. `distributed-snapshotter-plan.md` = alternative ambitieuse **écartée** (non-objectif).
- **Source buildkit** vendue dans `.repos/buildkit` (gitignoré). Réf. M1 : `examples/kubernetes/{create-certs.sh, statefulset.rootless.yaml, consistenthash/}`.

## Env de dev (devbox + direnv)
- Toolchain reproductible via **devbox** (`devbox.json` + `devbox.lock`) : go, node LTS (24.x) + **pnpm via corepack** (`DEVBOX_COREPACK_ENABLED=1`), kubectl, helm, jq, cosign. `direnv` auto-charge l'env (`.envrc` généré par `devbox generate direnv`) ; `.devbox/` et `.direnv/` sont gitignorés.
- **Toujours préfixer les commandes par `devbox run -- <cmd>`** (ou être dans `devbox shell` / un shell direnv-chargé) pour garantir les bonnes versions. Ex : `devbox run -- make test`, `devbox run -- go vet ./...`, `devbox run -- pnpm install --frozen-lockfile`. Raccourcis devbox dispo : `devbox run test|lint|manifests|build`.
- **Node/pnpm** : pas de `npm`. pnpm est géré par corepack (champ `packageManager` de `package.json`). Installer les deps de release avec `devbox run -- pnpm install --frozen-lockfile`.
- `controller-gen` et `golangci-lint` ne sont **pas** dans devbox : ils restent pinnés via `go run …@version` dans le `Makefile` (source de vérité unique des versions).

## Tests k8s
- Context **ovh-dev** (région GRA9, cluster **partagé** ⇒ toujours un **namespace dédié** + cleanup systématique).
- `KUBECONFIG=/home/jo/lab/fabrique/devops-agent-as-markdowns/.secrets/plateform-fabrique/kubeconfig` — **utiliser, jamais `cat`** (cf. rule globale secrets).
- Toujours pinner `--context ovh-dev` : le kubeconfig contient aussi `ovh-prod` (ne jamais cibler prod pour des tests).
