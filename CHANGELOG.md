

## [0.5.2](https://github.com/socialgouv/buildkit-operator/compare/v0.5.1...v0.5.2) (2026-06-25)


### Bug Fixes

* **ci:** build golangci-lint from source so its Go version matches go.mod ([153d900](https://github.com/socialgouv/buildkit-operator/commit/153d9001054da6d65a4e97b001696f7901cd7dc6))
* **helm:** nil-safe api.* guard so helm upgrade --reuse-values doesn't break ([ac7c6d9](https://github.com/socialgouv/buildkit-operator/commit/ac7c6d95ceeee800faea72d6f3e1750c21d1fd41))

## [0.5.1](https://github.com/socialgouv/buildkit-operator/compare/v0.5.0...v0.5.1) (2026-06-25)


### Bug Fixes

* **ci:** lowercase image owner for cosign + disable matrix fail-fast ([19a182a](https://github.com/socialgouv/buildkit-operator/commit/19a182a1988bd544110ecd4eb31bb62c7ce2055b))

## [0.5.0](https://github.com/socialgouv/buildkit-operator/compare/v0.4.1...v0.5.0) (2026-06-25)


### Features

* harden CI, supply-chain, observability and robustness from full project review ([3446961](https://github.com/socialgouv/buildkit-operator/commit/3446961657863d6f9f07aeaba4fe8a788f18c05f))

## [0.4.1](https://github.com/SocialGouv/buildkit-operator/compare/v0.4.0...v0.4.1) (2026-06-25)

## [0.4.0](https://github.com/SocialGouv/buildkit-operator/compare/v0.3.0...v0.4.0) (2026-06-25)

## [0.3.0](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.3.0) (2026-06-25)


### Features

* **certs:** opt-in cert-manager issuance for the mTLS material ([3cd485b](https://github.com/socialgouv/buildkit-operator/commit/3cd485b6c83fadc825b2bcae0d23bd395278fdf0))
* **kata:** target the whole build nodepool so replacement nodes auto-get Kata (durable) ([34f8d38](https://github.com/socialgouv/buildkit-operator/commit/34f8d38ebb279b248870d4c403b7de92d0e959fd))


### Bug Fixes

* **helm:** raise OVH/OpenStack LB idle timeout so cold-start routes and long builds aren't cut ([085b9de](https://github.com/socialgouv/buildkit-operator/commit/085b9de37e008c978a3d1cfeb018320435207990))

## [0.2.3](https://github.com/SocialGouv/buildkit-operator/compare/v0.2.2...v0.2.3) (2026-06-25)


### Features

* **sandbox:** skip companion for forks; document Kata vCPU floor for nested virt ([7290418](https://github.com/SocialGouv/buildkit-operator/commit/7290418ac853eb9e6fd10c976379490345749c24))

## [0.2.2](https://github.com/SocialGouv/buildkit-operator/compare/v0.2.1...v0.2.2) (2026-06-25)


### Features

* **sandbox:** run sandboxed forks privileged + non-rootless (Kata microVM) ([72553fb](https://github.com/SocialGouv/buildkit-operator/commit/72553fbb026a0bd742a7c4eb20aa6aa56a18008a))


### Bug Fixes

* close metadata egress, release leaked inflight, normalize repo ports ([219ec50](https://github.com/SocialGouv/buildkit-operator/commit/219ec509493329578528f1ffb79921949c08bd29))

## [0.2.1](https://github.com/SocialGouv/buildkit-operator/compare/v0.2.0...v0.2.1) (2026-06-25)


### Bug Fixes

* harden routing, lifecycle and gateway (full review remediation) ([1b22e91](https://github.com/SocialGouv/buildkit-operator/commit/1b22e91174e9b8f498818de7ef4bf979c67f54f7))

## [0.2.0](https://github.com/SocialGouv/buildkit-operator/compare/v1...v0.2.0) (2026-06-25)


### Features

* **daemon:** bump default buildkit image to v0.22.0-rootless (ovh-prod parity) ([6a843f9](https://github.com/SocialGouv/buildkit-operator/commit/6a843f9aff351e948573456f3d25c06ece3f9550))
* **scheduling:** pin daemon pods to a dedicated build nodepool ([761f6f2](https://github.com/SocialGouv/buildkit-operator/commit/761f6f234ec1e2ed979e2dccc8d1de693a756328))


### Bug Fixes

* **release:** create the GitHub release via gh CLI (release-it octokit call crashes on v17) ([2cdd912](https://github.com/SocialGouv/buildkit-operator/commit/2cdd912149f97d2120f87af8571dffd39de3bc56))

## 0.1.0 (2026-06-25)


### Features

* **action:** optional gateway-ip input (no-wildcard-DNS escape hatch) + skip image rebuilds on docs/action changes ([2f13fd3](https://github.com/SocialGouv/buildkit-operator/commit/2f13fd33a5220d3bbc116d50959209a54a7c15a7))
* **ci:** reusable composite GitHub Action (action.yml + scripts/build.sh) ([2ec9152](https://github.com/SocialGouv/buildkit-operator/commit/2ec915267df6f4333aa654987e2e8415b0cc7bc7))
* **gateway:** optional LoadBalancer exposure for off-cluster CI ([63932b3](https://github.com/SocialGouv/buildkit-operator/commit/63932b314ec415308d8c8f7817afbd7a9bb8d129))
* **gateway:** single shared SNI gateway, deterministic endpoint, ClusterIP daemons ([b0668a0](https://github.com/SocialGouv/buildkit-operator/commit/b0668a0e4b82daeef45b60e3ca5bfb0e472629ce))
* **ha:** leader election for buildd (run multiple replicas) ([1b05bf4](https://github.com/SocialGouv/buildkit-operator/commit/1b05bf4ce56517614bd6c314772040536854bd79))
* M1 scaffold — buildcat control plane (daemon-per-project on OVH) ([063a3df](https://github.com/SocialGouv/buildkit-operator/commit/063a3df3b37525e665d9bb905d0620a29116cb93))
* **m2:** elasticity — tier-aware scale-to-zero, /prewarm, warm pool ([4e9feae](https://github.com/SocialGouv/buildkit-operator/commit/4e9feae35b45d508687c9eec044c41ce87843cfb))
* **m3:** durability — periodic in-use snapshots + restore-from-snapshot ([db8966f](https://github.com/SocialGouv/buildkit-operator/commit/db8966f12f3f3bee2d9773be0822a883b75618bf))
* **m4:** observability, fork-PR isolation, cold-start backpressure ([d8adb5c](https://github.com/SocialGouv/buildkit-operator/commit/d8adb5c2ae559a40504dce1d17389f32a57e5e79))
* **m5:** conditional fan-out (CoW clones) + promote mechanic ([0800e61](https://github.com/SocialGouv/buildkit-operator/commit/0800e617fdade4dfa56b4ce9ac7a5a372a9342d9))
* pull-through cache (base-image mirror) + sandboxed runtime for fork daemons ([5f2dc25](https://github.com/SocialGouv/buildkit-operator/commit/5f2dc251da1ef065218a7b8491a58d91e01decbe))
* **router:** optional Name component in the key for monorepos ([cb6ce42](https://github.com/SocialGouv/buildkit-operator/commit/cb6ce420f64d642fde13fbd93b05bdc9fa3f80b0))
* **s3:** cold cache as a project policy (buildd-driven, creds on the daemon) ([dd50077](https://github.com/SocialGouv/buildkit-operator/commit/dd50077a29afd61de7cf5f55bebeb297432dbac1))
* **security:** daemon egress NetworkPolicy + supply-chain attestations in the Action ([c439b04](https://github.com/SocialGouv/buildkit-operator/commit/c439b04d4d3b062cad161108e1bf4d171c1e7665))
* **security:** opt-in internet-less egress for untrusted fork daemons ([e1647cc](https://github.com/SocialGouv/buildkit-operator/commit/e1647ccd10af287f8b4280024e1f5ffec0d8de75))


### Bug Fixes

* **builder:** unset allowPrivilegeEscalation for rootless; optional companion; buildd --context ([337bd89](https://github.com/SocialGouv/buildkit-operator/commit/337bd8964a047d8b4a75f5c055d8efc6f5c9a704))
* **controller:** default IdleTimeoutSec in applyDefaults (warm scale-to-zero bug) ([8f0fded](https://github.com/SocialGouv/buildkit-operator/commit/8f0fdedeaaada5a1163b210cea6ef5b9025875d5))
* **controller:** fully reconcile the daemon Service + StatefulSet, not just replicas ([403f7a9](https://github.com/SocialGouv/buildkit-operator/commit/403f7a9165957d99ec9ef02594aeaaa2f2d7f8a9))
* **release:** use npm ci so release-it sees a clean working dir ([3cb4cd1](https://github.com/SocialGouv/buildkit-operator/commit/3cb4cd13bc7916816af3e5ec1c4e548eb9e4fdf4))
* **status:** MergeFrom patches for LastBuildTime + reconciler status (no clobber) ([c58cb40](https://github.com/SocialGouv/buildkit-operator/commit/c58cb408ec9bb53542aef2c0a87b3d62e4682529))
* **status:** RetryOnConflict for touchLastBuild (correct the prior MergeFrom attempt) ([d3be0ef](https://github.com/SocialGouv/buildkit-operator/commit/d3be0ef4de5adafd62103f59a44a5255cacd3ec7))
