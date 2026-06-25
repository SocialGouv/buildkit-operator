# Phase 0 benchmark — Cinder gen2 storage latencies

Before any code, a `kubectl`-only protocol measured the OVH MKS storage behaviours that **decide the
config values and validate the design assumptions**. The full protocol and raw `results.md` live in
the local `.plans/` (git-ignored); this is the durable summary and how each finding maps to config.

Environment: OVH MKS, region **GRA9**, StorageClass `csi-cinder-high-speed-gen2`,
snapshot classes `csi-cinder-snapclass-v1` and `csi-cinder-snapclass-in-use-v1`.

## Cluster facts that corrected the plan's assumptions

- `csi-cinder-high-speed-gen2` uses **`volumeBindingMode: Immediate`** (the plan assumed
  `WaitForFirstConsumer`). PVCs bind without a consumer; consumer pods are kept only to trigger the
  attach/mount we actually time.
- Both `csi-cinder-snapclass-v1` **and** `csi-cinder-snapclass-in-use-v1` already exist — **reuse
  them**, don't create a `cinder-snap`. The *in-use* variant is the key enabler for M3.
- gen2 throughput **scales with volume size**, so `cacheVolumeGi` is a performance knob, not just
  capacity.

## Findings → config

| Measurement | Result | Consequence | Config |
|---|---|---|---|
| **A — isolated attach** (bind + attach + mount, single PVC) | p50 ≈ **19.5 s** | scale-to-zero is acceptable for the `warm` tier **if** wake-ups are pre-warmed | enable scale-to-zero; `/prewarm` on push |
| **B — reattach cycle** (scale-to-zero → wake) | p50 ≈ **31 s** | don't churn; keep a daemon up across short idle gaps | `idleTimeoutSec` ≈ **900** (15 min) |
| **C — burst of cold starts** | N=50 p50 ≈ **6 min** (degrades badly under concurrency) | a thundering herd of cold starts is pathological — must be throttled | `--max-cold-starts` (default **8**) |
| **D — snapshot + clone** | **CoW**: time is ~constant regardless of data size | cloning a warm cache is cheap → horizontal fan-out is viable | M5 `spec.fanout` uses CoW clones |
| **In-use snapshot probe** | **supported** (snapshot a still-mounted PVC) | snapshot a hot daemon **without** scaling it to zero | M3 uses `csi-cinder-snapclass-in-use-v1` |

## Why these shaped the design

- **B and C are the reason buildkit-operator retains the PVC across scale-to-zero and rate-limits cold
  starts.** A 31 s reattach is fine occasionally but ruinous if you churn; a 50-wide cold burst
  taking minutes is why `--max-cold-starts` exists and why the warm pool and `/prewarm` matter.
- **D is the reason fan-out (M5) is a CoW clone, not a full copy.** Constant-time snapshot/clone
  means materializing N warm replicas from a snapshot is practical.
- **The in-use snapshot result simplified M3.** The original plan assumed OVH could only snapshot a
  detached volume (forcing a scale-to-zero to snapshot). Because in-use snapshots work, durability
  snapshots run against a live, hot daemon.

## Reproducing

The protocol is pure `kubectl`/`jq`/`awk` in a dedicated namespace with mandatory cleanup (it writes
50–100 GiB of random data into test PVCs, all ephemeral). Pin a Ready, non-cordoned node, keep
bursts under the per-node volume limit (100), and delete the namespace + verify no orphan
PV/VolumeSnapshotContent afterwards. See the protocol in `.plans/bench-phase0-cinder-ovh.md`.
