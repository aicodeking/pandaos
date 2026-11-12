# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB; PandaOS 26.05 install (NixOS, kernel 6.18.57); boat store = native btrfs (compress=zstd:1) on one NVMe, Samsung MZVL2512HCJQ; boatd defaults (trusted, memOvercommit 1.5), owner team quota 8 default VMs; governor powersave, EPP balance_performance; 16 CPUs online |
| Harness client CPU | AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 |
| Harness client egress (Cloudflare colo) | HEL DE |
| Requested size | default |
| Preset (presets.json) | pandaos-install |
| boatd options over the defaults | {"_installed":"/etc/boat/boatd.json sets no diskCache, memOvercommit, cpuOvercommit, warmPool or untrustedTenants: boatd defaults","defaultTeam":{"maxVMs":10,"maxVCPU":32,"maxMemMiB":65536}} |
| Firecracker drive cache_type | Unsafe (boatd default diskCache) |
| VM vCPU / RAM / disk (measured) | 4 vCPU / 7965 MiB / 50.7 GiB |
| VM CPU model | AMD EPYC |
| VM kernel / OS / virt | 6.1.191 / Ubuntu 24.04.5 LTS / kvm |
| VM egress (Cloudflare colo, country) | HEL FI |
| Node (benchmark / image default) | v22.22.3 / v26.13.0 |
| Repeats (fresh VMs) | 8 |
| Run started (UTC) | 2026-11-12T04:54:39Z |
| Run URL | - |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | higher | 21.25 [21.14-21.35] n=9 | - | - | only one side measured |
| (b) API round-trip baseline: authenticated GET of the VM, p50 of 25 | ms | lower | 0.311 [0.310-0.319] n=3 | - | - | only one side measured |
| (b) create -> exec-ready | s | lower | 1.58 [1.56-1.60] n=8 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50 | ms | lower | 2.51 [2.46-2.53] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p95 of 50 | ms | lower | 4.99 [4.78-5.10] n=3 | - | - | only one side measured |
| (b) fork -> fork exec-ready | s | lower | 0.824 [0.806-0.837] n=3 | - | - | only one side measured |
| (b) stop (until stopped) | s | lower | 0.728 [0.728-0.732] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready | s | lower | 0.198 [0.195-0.219] n=3 | - | - | only one side measured |
| (b) snapshot (until usable) | s | lower | 2.67 [2.61-2.87] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready | s | lower | 0.512 [0.498-0.525] n=3 | - | - | only one side measured |
| (c) 8 concurrent creates (BOATBENCH_CONCURRENT_N=8), wall to all exec-ready | s | lower | 3.54 [3.54-3.56] n=3 | - | - | only one side measured |
| (d) disk seq write 1M † | MB/s | higher | 1,514 [1,487-1,519] n=3 | - | - | only one side measured |
| (d) disk seq read 1M | MB/s | higher | 7,546 [7,058-7,583] n=3 | - | - | only one side measured |
| (d) disk rand write 4k † | IOPS | higher | 227,075 [226,432-228,134] n=3 | - | - | only one side measured |
| (d) disk rand read 4k | IOPS | higher | 218,156 [138,340-219,680] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand read 4k | IOPS | higher | 56,693 [56,424-139,164] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand write 4k † | IOPS | higher | 260,411 [260,210-260,535] n=3 | - | - | only one side measured |
| (d) PTS fio job: seq write 1M † | MiB/s | higher | 1,651 [1,628-1,671] n=3 | - | - | only one side measured |
| (d) rand write 4k, fsync after every write, iodepth 1 † | IOPS | higher | 26,008 [25,847-26,024] n=3 | - | - | only one side measured |
| (d) seq write 1M, fsync at the end † | MB/s | higher | 1,589 [1,582-1,718] n=3 | - | - | only one side measured |
| (e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com) | Mbit/s | higher | 818.8 [775.7-823.7] n=3 | - | - | only one side measured |
| (e) download Node.js 22.22.3 tarball, ~30 MB (nodejs.org) | Mbit/s | higher | 834.3 [828.3-835.0] n=3 | - | - | only one side measured |
| (e) TCP connect 1.1.1.1, median of 10 | ms | lower | 1.41 [1.40-1.42] n=3 | - | - | only one side measured |
| (e) TCP connect github.com, median of 10 | ms | lower | 24.01 [23.98-24.02] n=3 | - | - | only one side measured |
| (f) cold `npm ci` of pinned project | s | lower | 2.46 [2.45-2.47] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50, NET of API round trips (raw - k x baseline p50) | ms | lower | 2.20 [2.16-2.21] n=3 | - | - | only one side measured |
| (b) create -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 1.56 [1.56-1.58] n=3 | - | - | only one side measured |
| (b) fork -> fork exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.823 [0.805-0.836] n=3 | - | - | only one side measured |
| (b) stop (until stopped), NET of API round trips (raw - k x baseline p50) | s | lower | 0.728 [0.728-0.732] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.197 [0.195-0.219] n=3 | - | - | only one side measured |
| (b) snapshot (until usable), NET of API round trips (raw - k x baseline p50) | s | lower | 2.67 [2.61-2.87] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.511 [0.497-0.524] n=3 | - | - | only one side measured |
| (c) 8 concurrent creates (BOATBENCH_CONCURRENT_N=8), wall to all exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 3.54 [3.54-3.56] n=3 | - | - | only one side measured |

† PandaOS write numbers: Firecracker's drive `cache_type` was **Unsafe (boatd default diskCache)**. In `Unsafe` mode the guest's flushes are not passed to the host, so fsync and O_DIRECT writes complete once they reach the host page cache, not the disk. They are not durable-write numbers, and boat.dev's need not be the same kind. `diskCache: Writeback` makes boatd honour guest flushes.

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/AnicetNgrt/hpc-sandbox-benchmarks/tree/ed8ec45a1ee0f5058ca99944d9c569d49b2f8e67/docs/results/2026-08-24-cpu (leaderboard.md, README.md, run.json), linked from boat.dev's benchmark section (measured by the vendor on 2026-08-24 (Ascii Box, Freestyle); the other rows are from the vendor's July CI run). Vendor-published (boat.dev was formerly Ascii Box; its row is 'Ascii Box (bare metal)'). Despite that label, 27.67 is NOT a bare-metal host score: run.json's observedSpecs for that row report a 4 vCPU / 7.76 GiB KVM guest (virtualization kvm, manufacturer QEMU, Q35, SeaBIOS, detectedIsolation vm) with cpuModel AMD Ryzen 9 9950X, egress OVH Lille. It is a sandbox VM on a dedicated host. The host itself was not measured. boat.dev's '1,000 runs in 36 s' is 1000 / 27.67 runs/s. Their run: PTS pts/node-web-tooling-1.0.1 with DynamicRunCount, median of 21 trials on fresh machines; the Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (runs/s) | 27.67 | 21.25 [21.14-21.35] n=9 | - | p50 of 21 trials, 95% CI 26.01-28.34; p95 28.72 |
| pts_rand_read_iops (IOPS) | 433,500 | 56,693 [56,424-139,164] n=3 | - | pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here, but a loop file on a shared runner cannot speak for NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | 260,411 [260,210-260,535] n=3 | - | pts/fio-2.1.0: 4KB rand write, same job. A loop file on a shared runner cannot speak for NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | 1,651 [1,628-1,671] n=3 | - | pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. A loop file on a shared runner cannot speak for NVMe |

The vendor's whole table (VENDOR-MEASURED, not by this harness). The CPU is what each guest reported in the vendor's run.json, so these rows differ in hardware as well as software.

| Provider | runs/s | CPU the guest reported |
|---|---|---|
| Ascii Box (bare metal), now boat.dev | 27.67 | AMD Ryzen 9 9950X 16-Core Processor |
| Blaxel | 19.8 | AMD EPYC |
| Daytona (VM) | 18.6 | AMD EPYC |
| Modal (VM) | 15.07 | AMD EPYC 9654 96-Core Processor |
| Novita | 13.52 | AMD EPYC |
| E2B | 11.24 | Intel(R) Xeon(R) Processor @ 2.60GHz |
| Freestyle | 9.64 | 19/01 (no model name reported) |
| Modal (gVisor) | 9.56 | unknown |
| Ascii Box (Hetzner CX33) | 8.88 | AMD EPYC-Rome Processor |

## Runner host vs PandaOS VM (MEASURED, same runner)

Identical scripts. Host: Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB; PandaOS 26.05 install (NixOS, kernel 6.18.57); boat store = native btrfs (compress=zstd:1) on one NVMe, Samsung MZVL2512HCJQ; boatd defaults (trusted, memOvercommit 1.5), owner team quota 8 default VMs; governor powersave, EPP balance_performance; 16 CPUs online. Disk: both sides write NOCOW files on the same btrfs loop store. The VM adds its guest ext4, virtio and Firecracker's I/O engine, and its O_DIRECT stops at the host page cache, so a ratio above 1 does not mean a VM beats the host.

| Metric | Unit | Runner host, no VM (measured) | PandaOS VM (measured) | VM / host |
|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | 23.08 [23.01-23.12] n=9 | 21.25 [21.14-21.35] n=9 | 0.921 |

## PROJECTION of PandaOS on AMD Ryzen 9 9950X (NOT A MEASUREMENT)

**Nobody ran PandaOS on this CPU. These numbers are arithmetic, not benchmark results.** Estimated Node loop range: **25.5-26.5 runs/s**.

| Method | Projected runs/s (not measured) |
|---|---|
| vendor's VM score x our VM/host: 27.67 x 0.921. A floor if the vendor's VM also has overhead, because its host then scores above 27.67 | 25.5 |
| public host average x our VM/host: 28.8 x 0.921 | 26.5 |

Limits: the vendor's repo has no EPYC 7763 row, so the CPU ratios come from public data with mixed Node versions, setups and sample sizes (the 7763 row has 4 results). The VM/host ratio was measured on a shared, nested Azure VM and may not transfer to a dedicated host. It says nothing about memory, disk or network hardware.

Same-benchmark source: https://openbenchmarking.org/test/pts/node-web-tooling, per-CPU runs/s averages, retrieved 2026-11-03: 'AMD EPYC 7763 64-Core' 10.3 +/- 0.4 (4 public results), 'AMD Ryzen 9 9950X 16-Core' 28.8 +/- 2.6 (30 public results). PassMark source: https://www.cpubenchmark.net/compare/4207vs6211/AMD-EPYC-7763-vs-AMD-Ryzen-9-9950X, retrieved 2026-11-03.

## (g) Density under load: pandaos (MEASURED)

N VMs run the Node loop at the same time from one barrier, all on this one host. Efficiency = per-VM runs/s at N / at N=1. CPU per loop = (user+sys)/real of node in the guest. median [IQR] n=samples.

| N | per-VM runs/s | host runs/s (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs |
|---|---|---|---|---|---|---|
| 1 | 20.97 [20.97-20.98] n=2 | 20.97 [20.97-20.98] n=2 | 1.00 [1.00-1.00] n=2 | 1.46 [1.46-1.47] n=2 | 0.092 [0.091-0.092] n=2 | 16.00 |
| 2 | 18.97 [18.97-18.98] n=2 | 37.95 [37.93-37.96] n=2 | 0.905 [0.904-0.905] n=2 | 1.49 [1.49-1.49] n=2 | 0.183 [0.183-0.183] n=2 | 16.00 |
| 4 | 15.60 [15.53-15.67] n=2 | 62.42 [62.19-62.66] n=2 | 0.744 [0.740-0.747] n=2 | 1.48 [1.48-1.49] n=2 | 0.366 [0.366-0.366] n=2 | 16.00 |
| 7 | 11.60 [11.59-11.61] n=2 | 81.14 [81.14-81.15] n=2 | 0.553 [0.552-0.554] n=2 | 1.46 [1.46-1.46] n=2 | 0.641 [0.640-0.641] n=2 | 16.00 |
| 8 | 10.41 [10.39-10.43] n=2 | 83.45 [83.29-83.61] n=2 | 0.496 [0.495-0.498] n=2 | 1.44 [1.43-1.44] n=2 | 0.732 [0.732-0.733] n=2 | 16.00 |

### Cost per run on Hetzner AX42, MEASURED (price and speed from the same CPU)

USD per 1000 runs = $0.1631/h / 3.6 / measured host runs/s, at 100% busy, no retention; + operator time: $0.3685/h. boatd admits 11 `default` VMs on it in trusted mode (prices.json). boat.dev reference: $0.000361 (its price at its PUBLISHED solo speed; its speed under load is unmeasured).

| N VMs | host runs/s (measured) | USD per 1000 runs | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.97 | $0.002160 | $0.004881 | 5.98x |
| 2 | 37.95 | $0.001194 | $0.002698 | 3.30x |
| 4 | 62.42 | $0.000726 | $0.001640 | 2.01x |
| 7 | 81.14 | $0.000558 | $0.001262 | 1.54x |
| 8 | 83.45 | $0.000543 | $0.001227 | 1.50x |

## (g) Host-native density baseline vs VMs (MEASURED, same host)

N copies of the Node loop directly on the host (no VM), against N VMs each running one. Both lose speed as the CPUs fill; the VM / host ratio at each N is what virtualization adds.

| N | host loops: per-loop / total runs/s | efficiency | VMs: per-VM / total runs/s | efficiency | VM / host at this N |
|---|---|---|---|---|---|
| 1 | 21.96 / 21.96 | 1.00 | 20.97 / 20.97 | 1.00 | 0.955 |
| 2 | 21.11 / 42.23 | 0.962 | 18.97 / 37.95 | 0.905 | 0.899 |
| 4 | 17.40 / 69.53 | 0.793 | 15.60 / 62.42 | 0.744 | 0.896 |
| 7 | 12.99 / 90.95 | 0.592 | 11.60 / 81.14 | 0.553 | 0.893 |
| 8 | 11.72 / 93.66 | 0.534 | 10.41 / 83.45 | 0.496 | 0.888 |
| 11 | 9.02 / 99.10 | 0.411 | - / - | - | - |

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Cost = side's price per hour (prices.json) x time. A per-run cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 Node loop runs | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0233 | AMD Ryzen 7 PRO 8700GE | AMD EPYC (guest); harness host AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 | 0.000305 | 0.000016 |
| boatdev | 0.0360 | AMD Ryzen 9 9950X (Zen 5), as boat.dev's guest reported it in the vendor's benchmark run | - | - | - |

CPU-consistent figure: boat.dev's price with boat.dev's own published speed on the CPU it sells (27.67 runs/s, vendor-measured, AMD Ryzen 9 9950X 16-Core Processor): **0.000361 USD per 1000 runs**. PandaOS's price here is AMD Ryzen 7 PRO 8700GE: its per-run cost is CPU-consistent only where the speed above was measured on that CPU (the density section says which).

## Caveats

- The PandaOS numbers from CI come from a GitHub-hosted runner, NOT a dedicated server: shared 4 vCPU, KVM exposed to the runner VM (Firecracker runs nested inside Azure's hypervisor), US region. boat.dev runs in the EU (Germany/Finland/France). Comparisons are indicative until `run-on-server.sh` runs on the owner's dedicated server.
- On the runner, each PandaOS VM (4 vCPU / 8 GiB) gets the whole 4-vCPU host, and the 10-VM concurrency test oversubscribes it (boatd memOvercommit/cpuOvercommit raised just so it fits). boat.dev places VMs on its fleet.
- Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); boat.dev stop archives the disk and resume/fork restore it in the background after `ready` (docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.
- Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled to `ready`). New-from-snapshot uses each one's create-from-snapshot.
- Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.
- Raw lifecycle latencies include the API round trip from the harness host to the provider: the CI PandaOS job talks to localhost, the boat.dev job talks to boat.dev over the internet from a US runner. The NET rows subtract k x that side's measured API round-trip p50, k = the fewest sequential API calls the timing contains (drivers.py ROUND_TRIPS: 2 for create/fork/resume/from-snapshot, stop and snapshot 1 on PandaOS and 2 on boat.dev, which polls them).
- Disk numbers on the runner come from a btrfs loop file on a shared cloud disk. They cannot speak for NVMe, including boat.dev's published numbers, even when the fio job is the same.
- Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean no clear winner.
- Disk tool differs between sides: {'pandaos': {'-', 'fio'}}. dd numbers are not comparable with fio.
