# Changelog

## [0.11.1](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.11.1) (2026-07-02)

### Bug Fixes

* **build:** S3 cache export is best-effort (ignore-error=true) ([7004ac6](https://github.com/socialgouv/buildkit-operator/commit/7004ac6ffff942d8df468bb0b129d120b3947108))

## [0.11.0](https://github.com/socialgouv/buildkit-operator/compare/v0.10.0...v0.11.0) (2026-07-02)

### Features

* **action:** build secrets passthrough (--secret id=…,env=…) across GitHub/Forgejo/GitLab surfaces ([d561212](https://github.com/socialgouv/buildkit-operator/commit/d561212316b9da39f33eb880f3763f26b9bf9181))

## [0.10.0](https://github.com/SocialGouv/buildkit-operator/compare/v0.9.0...v0.10.0) (2026-06-29)

### Features

* **buildd:** local backend parity — ZFS snapshots, CoW fork seed, VM forks, fan-out ([33813e1](https://github.com/SocialGouv/buildkit-operator/commit/33813e169f30872e2cb004b516e88a5dd3d8d83b))
* **buildd:** single-host Incus + ZFS backend (--backend local, MVP) ([c16b7c6](https://github.com/SocialGouv/buildkit-operator/commit/c16b7c6ea166756b22bfda951f5a329d2add2b02))
* **build:** forward --build-arg / --label across GitHub, GitLab & Forgejo ([#1](https://github.com/SocialGouv/buildkit-operator/issues/1)) ([8707e7f](https://github.com/SocialGouv/buildkit-operator/commit/8707e7f9d2a3baeeb5154bfbd0ae114bccfa4e58))
* **local:** Docker dev runtime + validate the local control plane end-to-end ([49988f8](https://github.com/SocialGouv/buildkit-operator/commit/49988f885db71d2cc83fae3d14ec831bd6cbcd9c))
* **vm:** e2e kit for the local backend + deterministic endpoints & cert mount ([08da2fa](https://github.com/SocialGouv/buildkit-operator/commit/08da2fa6efc479805f4034559d90896a0eb9a7d5))

### Bug Fixes

* **local:** incus Launch via init+device add+start; best-effort egress ([bb337ef](https://github.com/SocialGouv/buildkit-operator/commit/bb337efcde81b6ce6ca1ff223664499745aba16c))
* **local:** Ready probes the buildkitd port (instance Running != daemon serving) ([5d3202e](https://github.com/SocialGouv/buildkit-operator/commit/5d3202e383cb01785067fa701cd7f8df97b80f0d))
* **vm:** don't pass container-only security.nesting to the VM fork image ([3dee930](https://github.com/SocialGouv/buildkit-operator/commit/3dee930e1e2c17074bb5d1dd42cd7bd2562e007b))
* **vm:** quickstart waits for instance network + auto-relaxes Docker bridge netfilter ([30c86ea](https://github.com/SocialGouv/buildkit-operator/commit/30c86ea293582bbac8df686fb8e78cc1d93f9e00))
* **vm:** skip the VM fork image when qemu is absent (only /dev/kvm is not enough) ([0b0ee35](https://github.com/SocialGouv/buildkit-operator/commit/0b0ee3513065454059f79777d2ba3084df3ac7a2))
* **vm:** VM fork image build is non-fatal (incus VM unsupported on some hosts) ([bc22757](https://github.com/SocialGouv/buildkit-operator/commit/bc227570ba50a067c04feb8bb914a502ef2a4998))

## [0.9.0](https://github.com/SocialGouv/buildkit-operator/compare/v1...v0.9.0) (2026-06-27)

### Features

* **action:** gateway-host input for IP-independent gateway addressing ([7bcb4f8](https://github.com/SocialGouv/buildkit-operator/commit/7bcb4f8b6cfd5288e86e3a2fe950728c9b6d42ef))
* **forgejo:** document Forgejo Actions support + add forge CLIs to devbox ([5634d1f](https://github.com/SocialGouv/buildkit-operator/commit/5634d1fcf07c00c2949ea3f3cc71fee10b9c6af5))
* **forgejo:** reusable Forgejo Actions action + correct OIDC opt-in (enable-openid-connect) ([275c6ca](https://github.com/SocialGouv/buildkit-operator/commit/275c6ca01c510e5361f94607cb9f0391ee0abc03))
* **security:** legacy-bearer fallback while OIDC is enabled (zero-downtime migration) ([b346209](https://github.com/SocialGouv/buildkit-operator/commit/b3462098a1eccde1a37a126e3fcfa7d66460c121))
* **security:** verify CI OIDC identity on /route + harden egress, PVC, LB, CI ([c0e8906](https://github.com/SocialGouv/buildkit-operator/commit/c0e890690173516f3acb6a56767695af6c6fcc38))

## [0.8.3](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.8.3) (2026-06-26)

### Bug Fixes

* untrusted/fork builds hung — reaper killed the fork in its birth window ([059ccbd](https://github.com/socialgouv/buildkit-operator/commit/059ccbdc47c458795718a91382ff3da9ad086837))

## [0.8.2](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.8.2) (2026-06-26)

### Bug Fixes

* **buildd:** warm a fresh project reliably + expose readiness on /prewarm ([9854008](https://github.com/socialgouv/buildkit-operator/commit/98540087cbfaa509fcedbd6b177d1b7b69441efa))

## [0.8.1](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.8.1) (2026-06-26)

### Bug Fixes

* **build:** poll /route in bounded attempts when tunnelling through a proxy ([1862462](https://github.com/socialgouv/buildkit-operator/commit/186246275d695ee480ae968a4a82a9f8ea79447e))

## [0.8.0](https://github.com/socialgouv/buildkit-operator/compare/v1...v0.8.0) (2026-06-26)

### Features

* **build:** accept base64-encoded mTLS certs (GitLab masked-variable convention) ([ff678ab](https://github.com/socialgouv/buildkit-operator/commit/ff678abe2dee41c097f224c93fa80df02e5cf277))
* **gateway:** multi-domain — one gateway fronts several client populations ([031f29e](https://github.com/socialgouv/buildkit-operator/commit/031f29e48296c40119d210b8284480ee2f87193f))
* off-cluster builds behind an egress proxy (gateway on 443 + buildd Ingress + CI tunnel) ([714dab1](https://github.com/socialgouv/buildkit-operator/commit/714dab1a30672c922d78e43a8afec598c0f8c184))

### Bug Fixes

* harden buildkit operator security ([6c5ea80](https://github.com/socialgouv/buildkit-operator/commit/6c5ea80ddbdcfa13e2a59c2adb2735da5baef083))

## [0.7.0](https://github.com/socialgouv/buildkit-operator/compare/v0.6.0...v0.7.0) (2026-06-26)

### Features

* **ci:** reusable GitLab CI/CD component for builds ([73b672b](https://github.com/socialgouv/buildkit-operator/commit/73b672b648428f8687bb9b5c36853b21a0935107))

## [0.6.0](https://github.com/socialgouv/buildkit-operator/compare/v0.5.2...v0.6.0) (2026-06-26)

### ⚠ BREAKING CHANGES

* **deploy:** three-namespace topology (operator / builds / system) + founding ADRs

### Features

* **deploy:** three-namespace topology (operator / builds / system) + founding ADRs ([30c93dd](https://github.com/socialgouv/buildkit-operator/commit/30c93dd40ee23862880a3f824825b5da92aec4b5))

### Bug Fixes

* **helm:** nil-safe namespace helpers + only create builds ns ([644b4f4](https://github.com/socialgouv/buildkit-operator/commit/644b4f4f6cb3828eb55f209562bc78dd9227133c))

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
