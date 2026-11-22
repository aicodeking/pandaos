# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | - |
| Harness client CPU | AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 |
| Harness client egress (Cloudflare colo) | HEL DE |
| Requested size | default |
| Preset (presets.json) | trusted-writeback |
| boatd options over the defaults | {"diskCache":"Writeback","warmPool":false} |
| Firecracker drive cache_type | Writeback (boatd.json diskCache) |
| VM vCPU / RAM / disk (measured) | 4 vCPU / 7965 MiB / 50.7 GiB |
| VM CPU model | AMD EPYC |
| VM kernel / OS / virt | 6.1.191 / Ubuntu 24.04.5 LTS / kvm |
| VM egress (Cloudflare colo, country) | HEL FI |
| Node (benchmark / image default) | - / v26.13.0 |
| Repeats (fresh VMs) | 3 |
| Run started (UTC) | 2026-11-13T03:51:42Z |
| Run URL | - |
| Source commit (boat / PandaOS) | 0d160dd8d05177f706faf3a9f68791dd92f387a7 |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and its 28.06 index units is one solo VM, not a fully packed server.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (b) create -> exec-ready | s | lower | 1.02 [0.987-1.39] n=3 | - | - | only one side measured |
| (d) disk seq write 1M | MB/s | higher | 1,652 [1,626-1,672] n=3 | - | - | only one side measured |
| (d) disk seq read 1M | MB/s | higher | 12,976 [10,365-13,165] n=3 | - | - | only one side measured |
| (d) disk rand write 4k | IOPS | higher | 211,011 [210,142-212,262] n=3 | - | - | only one side measured |
| (d) disk rand read 4k, fio unpinned | IOPS | higher | 213,865 [136,420-215,224] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand read 4k, fio unpinned | IOPS | higher | 215,627 [213,993-215,898] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand write 4k | IOPS | higher | 247,347 [245,959-247,412] n=3 | - | - | only one side measured |
| (d) PTS fio job: seq write 1M | MiB/s | higher | 1,723 [1,722-1,757] n=3 | - | - | only one side measured |
| (d) rand write 4k, fsync after every write, iodepth 1 | IOPS | higher | 84.00 [83.50-84.00] n=3 | - | - | only one side measured |
| (d) seq write 1M, fsync at the end | MB/s | higher | 1,612 [1,582-1,682] n=3 | - | - | only one side measured |

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/starslingdev/hpc-sandbox-benchmarks/blob/297198a96ee827df5edcd918bd37b949526cfe20/LEADERBOARD.md (StarSling run 37073680030, commit f571eec3dc1378680f223a365d586f61a4fc6658, generated 2026-10-03T09:36:17Z), cited on https://boat.dev and https://docs.boat.dev/pricing.md (both fetched 2026-11-17) (measured by the vendor on 2026-10-03 for node_runs_s and the rows (StarSling run 37073680030); 2026-08-24 for the three pts_* disk metrics). Vendor-published, measured by StarSling, not by us. boat.dev's page says: 'every boat sandbox in this run ran on an AMD Ryzen 9 9950X host. The fleet also has AMD Ryzen 7 9700X hosts, which this run did not measure'. The leaderboard lists boat as a KVM virtual machine (declared; detected qemu-kvm, strong), so 28.06 is a sandbox VM (a 4 vCPU / 8 GiB KVM guest), not a bare-metal host score. The run is marked PARTIAL by StarSling (800 of 840 planned cells complete; the Node.js web tooling table itself has 5 sandboxes x 10 trials per provider, 95% bootstrap interval 23.27-28.22 for boat, so the median sits at the top of its interval). boat is ranked 2nd of 15 behind Brezel (30.06). Earlier figure (HISTORICAL, superseded): 27.67 index units, AnicetNgrt/hpc-sandbox-benchmarks, 2026-08-24, row 'Ascii Box (bare metal)', p50 of 21 trials, 95% CI 26.01-28.34, a 4 vCPU / 7.76 GiB KVM guest on a Ryzen 9 9950X; kept under 'historical'. The two runs use different harness repos, trial counts and provider sets, so 28.06 vs 27.67 (+1.4%) is inside both intervals and is not evidence of a speed-up. The geometric-mean subtest index is not completed jobs/s; 1000 / 27.67 or 1000 / 28.06 does not establish time to complete 1000 Node jobs. The Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (index units) | 28.06 | - | - | StarSling run 37073680030 (2026-10-03): median of 10 trials on 5 sandboxes, 95% bootstrap interval 23.27-28.22; Node.js web tooling geometric-mean index (V8 subtest runs/s, not completed suites/s), 4 vCPU / 8 GiB |
| pts_rand_read_iops (IOPS) | 433,500 | 215,627 [213,993-215,898] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here; PandaOS's disk, filesystem and cache mode differ (see the report's caveats), so its numbers cannot speak for boat.dev's NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | 247,347 [245,959-247,412] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 4KB rand write, same job. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | 1,723 [1,722-1,757] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |

The vendor's whole table (VENDOR-MEASURED, not by this harness). The CPU column is what the vendor's page or run reported, so these rows differ in hardware as well as software.

| Provider | index units | CPU |
|---|---|---|
| boat (StarSling, 2026-10-03) | 28.06 | AMD Ryzen 9 9950X (host, per boat.dev's page) |
| Brezel | 30.06 | not reported in LEADERBOARD.md |
| Namespace | 24.9 | not reported in LEADERBOARD.md |
| Daytona (VM) | 21.52 | not reported in LEADERBOARD.md |
| Microsandbox Cloud | 21.34 | not reported in LEADERBOARD.md |
| Novita | 20.16 | not reported in LEADERBOARD.md |
| tama | 18.69 | not reported in LEADERBOARD.md |
| Blaxel | 18.03 | not reported in LEADERBOARD.md |
| Modal (VM) | 15.48 | not reported in LEADERBOARD.md |
| run.cloud | 13.89 | not reported in LEADERBOARD.md |
| Vercel Sandbox | 12.66 | not reported in LEADERBOARD.md |
| Modal (gVisor) | 12.6 | not reported in LEADERBOARD.md |
| E2B | 11.08 | not reported in LEADERBOARD.md |
| Freestyle | 10.22 | not reported in LEADERBOARD.md |
| Runloop | 8.93 | not reported in LEADERBOARD.md |

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Index-normalized USD per 1000 index units = hourly price / 3.6 / index; npm cost = hourly price × seconds / 3600. An index-normalized cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 index units | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0233 | AMD Ryzen 7 PRO 8700GE; `default` VM | AMD EPYC (guest); harness host AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 | - | - |

CPU-consistent figure: boat.dev's `default` VM price with boat.dev's own published speed on the CPU it sells (28.06 index units, vendor-measured, AMD Ryzen 9 9950X (host, per boat.dev's page)): **0.000356 USD per 1000 index units**. PandaOS's price here is AMD Ryzen 7 PRO 8700GE: its index-normalized cost is CPU-consistent only where the speed above was measured on that CPU (the density section says which).

## Caveats

- The PandaOS numbers come from a dedicated server, with the harness on the same box talking to its local API. The server, its disks and its store are the harness-declared host in the table above. boat.dev runs in the EU (Germany/Finland/France).
- The Node.js web-tooling geometric-mean index is the geometric mean of subtest rates (subtest runs/s), not completed Node jobs or full suites per second. host_runs_s sums VM indices; node_runs_s and *_runs_s are legacy JSON keys. Cost per 1000 index units is an index-normalized price, not a bill for 1000 completed jobs. Completed-suite rates, where available, use successful pass counts divided by the density level wall time, including synchronization and harness overhead; no vendor completed-suite rate is available.
- Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); boat.dev stop archives the disk and resume/fork restore it in the background after `ready` (docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.
- Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled to `ready`). New-from-snapshot uses each one's create-from-snapshot.
- Exec round-trip (`run_true`) asymmetry: native PandaOS (`/v1/vms/{id}/exec`) runs `["true"]` directly via `execve`, whereas boat.dev and compat (`/api/v1/sandboxes/{id}/commands`) run `bash -lc true` and record a command. Use `--driver pandaos --compat` for the like-for-like mode.
- Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.
- Raw lifecycle latencies include the API round trip from the harness host to the provider: the PandaOS harness talks to the API on the same box, the boat.dev side (if measured) talks to boat.dev over the internet. The NET rows subtract k x that side's measured API round-trip p50 (clamped at >= 0), where k is the recorded serial critical-path HTTP call count (`api_calls`, excluding intermediate async polls that overlap server-side work) per operation when present, falling back to `drivers.py` `ROUND_TRIPS` for older artifacts (2 for create/fork/resume/from-snapshot, stop and snapshot 1 on PandaOS and 2 on boat.dev).
- Disk numbers come from this server's boat store as declared above. boat.dev's published disk numbers come from other NVMe drives with an unpublished fio target and cache mode, so even the same fio job is not a like-for-like disk comparison.
- Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean no clear winner.
