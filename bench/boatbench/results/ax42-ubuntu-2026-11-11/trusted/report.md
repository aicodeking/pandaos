# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted; 16 CPUs online |
| Harness client CPU | AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 |
| Harness client egress (Cloudflare colo) | HEL DE |
| Requested size | default |
| Preset (presets.json) | trusted |
| boatd options over the defaults | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16}} |
| Firecracker drive cache_type | Unsafe (boatd default diskCache) |
| VM vCPU / RAM / disk (measured) | 4 vCPU / 7965 MiB / 50.7 GiB |
| VM CPU model | AMD EPYC |
| VM kernel / OS / virt | 6.1.191 / Ubuntu 24.04.5 LTS / kvm |
| VM egress (Cloudflare colo, country) | HEL FI |
| Node (benchmark / image default) | v22.22.3 / v26.13.0 |
| Repeats (fresh VMs) | 2-3 |
| Run started (UTC) | 2026-11-11T08:22:54Z |
| Run URL | - |
| Source commit (boat / PandaOS) | not recorded |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and its 28.06 index units is one solo VM, not a fully packed server.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (a) Node.js web-tooling geometric-mean index | index units | higher | 21.15 [21.14-21.17] n=3 | - | - | only one side measured |
| (b) API round-trip baseline: authenticated GET of the VM, p50 of 25 | ms | lower | 0.283 [0.276-0.304] n=3 | - | - | only one side measured |
| (b) create -> exec-ready | s | lower | 1.40 [1.38-1.41] n=8 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50 | ms | lower | 2.29 [2.23-2.30] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p95 of 50 | ms | lower | 3.06 [2.83-3.10] n=3 | - | - | only one side measured |
| (b) fork -> fork exec-ready | s | lower | 1.24 [1.21-1.36] n=3 | - | - | only one side measured |
| (b) stop (until stopped) | s | lower | 0.617 [0.521-0.628] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready | s | lower | 0.256 [0.238-0.265] n=3 | - | - | only one side measured |
| (b) snapshot (until usable) | s | lower | 1.79 [1.51-2.17] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready | s | lower | 0.220 [0.220-0.225] n=3 | - | - | only one side measured |
| (c) 10 concurrent creates, wall to all exec-ready | s | lower | 2.99 [2.94-3.03] n=3 | - | - | only one side measured |
| (d) disk seq write 1M † | MB/s | higher | 1,863 [1,811-2,053] n=3 | - | - | only one side measured |
| (d) disk seq read 1M | MB/s | higher | 5,903 [5,175-6,859] n=3 | - | - | only one side measured |
| (d) disk rand write 4k † | IOPS | higher | 147,933 [147,926-148,402] n=3 | - | - | only one side measured |
| (d) disk rand read 4k, fio unpinned | IOPS | higher | 82,523 [81,494-129,108] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand read 4k, fio unpinned | IOPS | higher | 155,795 [119,692-166,690] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand write 4k † | IOPS | higher | 171,442 [171,064-171,514] n=3 | - | - | only one side measured |
| (d) PTS fio job: seq write 1M † | MiB/s | higher | 2,740 [2,611-2,754] n=3 | - | - | only one side measured |
| (d) rand write 4k, fsync after every write, iodepth 1 † | IOPS | higher | 23,942 [23,907-24,044] n=3 | - | - | only one side measured |
| (d) seq write 1M, fsync at the end † | MB/s | higher | 2,640 [2,494-2,683] n=3 | - | - | only one side measured |
| (e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com) | Mbit/s | higher | 812.4 [698.3-837.6] n=3 | - | - | only one side measured |
| (e) download Node.js 22.22.3 tarball, ~30 MB (nodejs.org) | Mbit/s | higher | 846.4 [844.0-849.8] n=3 | - | - | only one side measured |
| (e) TCP connect 1.1.1.1, median of 10 | ms | lower | 1.50 [1.50-1.50] n=3 | - | - | only one side measured |
| (e) TCP connect github.com, median of 10 | ms | lower | 24.04 [24.00-24.13] n=3 | - | - | only one side measured |
| (f) cold `npm ci` of pinned project | s | lower | 2.56 [2.52-2.62] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50, NET of API round trips (raw - k x baseline p50) | ms | lower | 2.00 [1.93-2.03] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p95 of 50, NET of API round trips (raw - k x baseline p50) | ms | lower | 2.79 [2.53-2.83] n=3 | - | - | only one side measured |
| (b) create -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 1.40 [1.38-1.41] n=8 | - | - | only one side measured |
| (b) fork -> fork exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 1.24 [1.21-1.36] n=3 | - | - | only one side measured |
| (b) stop (until stopped), NET of API round trips (raw - k x baseline p50) | s | lower | 0.617 [0.521-0.628] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.255 [0.238-0.264] n=3 | - | - | only one side measured |
| (b) snapshot (until usable), NET of API round trips (raw - k x baseline p50) | s | lower | 1.79 [1.51-2.17] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.219 [0.219-0.224] n=3 | - | - | only one side measured |
| (c) 10 concurrent creates, wall to all exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 2.99 [2.94-3.02] n=3 | - | - | only one side measured |

† PandaOS write numbers: Firecracker's drive `cache_type` was **Unsafe (boatd default diskCache)**. In `Unsafe` mode the guest's flushes are not passed to the host, so fsync and O_DIRECT writes complete once they reach the host page cache, not the disk. They are not durable-write numbers, and boat.dev's need not be the same kind. `diskCache: Writeback` makes boatd honour guest flushes.

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/starslingdev/hpc-sandbox-benchmarks/blob/297198a96ee827df5edcd918bd37b949526cfe20/LEADERBOARD.md (StarSling run 37073680030, commit f571eec3dc1378680f223a365d586f61a4fc6658, generated 2026-10-03T09:36:17Z), cited on https://boat.dev and https://docs.boat.dev/pricing.md (both fetched 2026-11-17) (measured by the vendor on 2026-10-03 for node_runs_s and the rows (StarSling run 37073680030); 2026-08-24 for the three pts_* disk metrics). Vendor-published, measured by StarSling, not by us. boat.dev's page says: 'every boat sandbox in this run ran on an AMD Ryzen 9 9950X host. The fleet also has AMD Ryzen 7 9700X hosts, which this run did not measure'. The leaderboard lists boat as a KVM virtual machine (declared; detected qemu-kvm, strong), so 28.06 is a sandbox VM (a 4 vCPU / 8 GiB KVM guest), not a bare-metal host score. The run is marked PARTIAL by StarSling (800 of 840 planned cells complete; the Node.js web tooling table itself has 5 sandboxes x 10 trials per provider, 95% bootstrap interval 23.27-28.22 for boat, so the median sits at the top of its interval). boat is ranked 2nd of 15 behind Brezel (30.06). Earlier figure (HISTORICAL, superseded): 27.67 index units, AnicetNgrt/hpc-sandbox-benchmarks, 2026-08-24, row 'Ascii Box (bare metal)', p50 of 21 trials, 95% CI 26.01-28.34, a 4 vCPU / 7.76 GiB KVM guest on a Ryzen 9 9950X; kept under 'historical'. The two runs use different harness repos, trial counts and provider sets, so 28.06 vs 27.67 (+1.4%) is inside both intervals and is not evidence of a speed-up. The geometric-mean subtest index is not completed jobs/s; 1000 / 27.67 or 1000 / 28.06 does not establish time to complete 1000 Node jobs. The Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (index units) | 28.06 | 21.15 [21.14-21.17] n=3 | - | StarSling run 37073680030 (2026-10-03): median of 10 trials on 5 sandboxes, 95% bootstrap interval 23.27-28.22; Node.js web tooling geometric-mean index (V8 subtest runs/s, not completed suites/s), 4 vCPU / 8 GiB |
| pts_rand_read_iops (IOPS) | 433,500 | 155,795 [119,692-166,690] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here; PandaOS's disk, filesystem and cache mode differ (see the report's caveats), so its numbers cannot speak for boat.dev's NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | 171,442 [171,064-171,514] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 4KB rand write, same job. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | 2,740 [2,611-2,754] n=3 | - | From the older 2026-08-24 run (not refreshed in the 2026-10-03 run, which reports different fio metrics). pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |

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

## Host vs PandaOS VM (MEASURED, same host)

Identical scripts. Host: Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted; 16 CPUs online. Disk: both sides write to the same btrfs store (as declared); the host side's files are NOCOW. The VM adds its guest ext4, virtio and Firecracker's I/O engine, and its O_DIRECT stops at the host page cache, so a ratio above 1 does not mean a VM beats the host.

| Metric | Unit | Host, no VM (measured) | PandaOS VM (measured) | VM / host |
|---|---|---|---|---|
| (a) Node.js web-tooling geometric-mean index | index units | 23.06 [22.95-23.11] n=3 | 21.15 [21.14-21.17] n=3 | 0.917 |
| (d) disk seq write 1M | MB/s | 4,539 [4,528-4,600] n=3 | 1,863 [1,811-2,053] n=3 | 0.410 |
| (d) disk seq read 1M | MB/s | 7,238 [7,046-7,706] n=3 | 5,903 [5,175-6,859] n=3 | 0.816 |
| (d) disk rand write 4k | IOPS | 125,043 [124,072-125,328] n=3 | 147,933 [147,926-148,402] n=3 | 1.18 |
| (d) disk rand read 4k, fio unpinned | IOPS | 252,582 [251,958-252,622] n=3 | 82,523 [81,494-129,108] n=3 | 0.327 |
| (d) PTS fio job: rand read 4k, fio unpinned | IOPS | 253,216 [253,155-253,349] n=3 | 155,795 [119,692-166,690] n=3 | 0.615 |
| (d) PTS fio job: rand write 4k | IOPS | 123,679 [123,610-123,935] n=3 | 171,442 [171,064-171,514] n=3 | 1.39 |
| (d) PTS fio job: seq write 1M | MiB/s | 4,776 [4,759-4,915] n=3 | 2,740 [2,611-2,754] n=3 | 0.574 |
| (d) rand write 4k, fsync after every write, iodepth 1 | IOPS | 82.00 [81.00-82.50] n=3 | 23,942 [23,907-24,044] n=3 | 292.0 |
| (d) seq write 1M, fsync at the end | MB/s | 5,452 [5,324-5,473] n=3 | 2,640 [2,494-2,683] n=3 | 0.484 |

## PROJECTION of PandaOS on AMD Ryzen 9 9950X (NOT A MEASUREMENT)

**Nobody ran PandaOS on this CPU. These numbers are arithmetic, not benchmark results.** Estimated Node loop range: **25.7-26.4 index units**.

| Method | Projected index units (not measured) |
|---|---|
| vendor's VM score x our VM/host: 28.06 x 0.917. A floor if the vendor's VM also has overhead, because its host then scores above 28.06 | 25.7 |
| public host average x our VM/host: 28.8 x 0.917 | 26.4 |

Limits: the VM/host ratio (0.917) was measured on this host (see the Host line above). None of this says anything about memory, disk or network hardware.

Same-benchmark source: https://openbenchmarking.org/test/pts/node-web-tooling, per-CPU geometric-mean index averages, retrieved 2026-11-03: 'AMD EPYC 7763 64-Core' 10.3 +/- 0.4 (4 public results), 'AMD Ryzen 9 9950X 16-Core' 28.8 +/- 2.6 (30 public results). PassMark source: https://www.cpubenchmark.net/compare/4207vs6211/AMD-EPYC-7763-vs-AMD-Ryzen-9-9950X, retrieved 2026-11-03.

## (g) Density under load: pandaos (MEASURED)

N VMs run the Node loop at the same time from one barrier, all on this one host. Efficiency = per-VM index units at N / at N=1. CPU per loop = (user+sys)/real of node in the guest. median [IQR] n=samples. The index is not completed-job throughput. Completed suites/s is total successful full-suite passes divided by level wall time, including harness overhead; no vendor counterpart is published.

| N | per-VM index units | host index units (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs | completed suites/s (measured) |
|---|---|---|---|---|---|---|---|
| 1 | 20.79 [20.78-20.81] n=2 | 20.79 [20.78-20.81] n=2 | 1.00 [1.00-1.00] n=2 | 1.49 [1.48-1.49] n=2 | 0.093 [0.092-0.093] n=2 | 16.00 | 0.019 [0.019-0.019] n=2 |
| 2 | 18.94 [18.93-18.96] n=2 | 37.89 [37.86-37.92] n=2 | 0.911 [0.910-0.912] n=2 | 1.50 [1.50-1.50] n=2 | 0.186 [0.185-0.187] n=2 | 16.00 | 0.038 [0.038-0.038] n=2 |
| 4 | 15.48 [15.39-15.56] n=2 | 62.01 [61.71-62.32] n=2 | 0.744 [0.740-0.749] n=2 | 1.48 [1.48-1.48] n=2 | 0.372 [0.370-0.373] n=2 | 16.00 | 0.066 [0.066-0.066] n=2 |
| 7 | 11.51 [11.50-11.51] n=2 | 80.47 [80.41-80.53] n=2 | 0.553 [0.553-0.554] n=2 | 1.47 [1.46-1.47] n=2 | 0.650 [0.647-0.653] n=2 | 16.00 | 0.094 [0.094-0.094] n=2 |
| 11 | 7.92 [7.91-7.93] n=2 | 87.06 [86.98-87.14] n=2 | 0.381 [0.380-0.381] n=2 | 1.27 [1.27-1.27] n=2 | 1.02 [1.02-1.03] n=2 | 16.00 | 0.103 [0.103-0.103] n=2 |

### Cost per index unit on Hetzner AX42, MEASURED (price and speed from the same CPU)

USD per 1000 index units = $0.1631/h / 3.6 / measured host index units, at 100% busy, no retention; + operator time: $0.3685/h. boatd admits 11 `default` VMs on it in trusted mode (prices.json). boat.dev reference: $0.000356 (its price at its PUBLISHED solo speed; its speed under load is unmeasured).

| N VMs | host index units (measured) | USD per 1000 index units | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.79 | $0.002178 | $0.004924 | 6.11x |
| 2 | 37.89 | $0.001196 | $0.002702 | 3.35x |
| 4 | 62.01 | $0.000730 | $0.001651 | 2.05x |
| 7 | 80.47 | $0.000563 | $0.001272 | 1.58x |
| 11 | 87.06 | $0.000520 | $0.001176 | 1.46x |

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Index-normalized USD per 1000 index units = hourly price / 3.6 / index; npm cost = hourly price × seconds / 3600. An index-normalized cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 index units | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0233 | AMD Ryzen 7 PRO 8700GE; `default` VM | AMD EPYC (guest); harness host AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 | 0.000306 | 0.000017 |

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
- Firecracker drive `cache_type` was `Unsafe (boatd default diskCache)`: guest flushes (`fsync` / `O_DIRECT`) stop at the host page cache and are not durable-write numbers (`diskCache: Writeback` enables durable flushes).
