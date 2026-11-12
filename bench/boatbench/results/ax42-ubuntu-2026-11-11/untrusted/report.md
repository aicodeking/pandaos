# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset untrusted; 8 CPUs online |
| Harness client CPU | AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x8 |
| Harness client egress (Cloudflare colo) | HEL DE |
| Requested size | default |
| Preset (presets.json) | untrusted |
| boatd options over the defaults | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16},"untrustedTenants":true,"btrfsQgroups":true,"storageOvercommit":1,"egressMbit":1000,"ingressMbit":1000,"ioCost":true,"ioCostQoS":"rpct=95 rlat=5000 wpct=95 wlat=10000 min=25 max=400","ioMaxMBps":4000,"ioMaxIOPS":200000,"ctPerVM":4096,"diskEngine":"Sync","hostDirtyRatio":20,"warmPool":false,"ksmTeam":""} |
| Firecracker drive cache_type | Unsafe (boatd default diskCache) |
| VM vCPU / RAM / disk (measured) | 4 vCPU / 7965 MiB / 50.7 GiB |
| VM CPU model | AMD EPYC |
| VM kernel / OS / virt | 6.1.191 / Ubuntu 24.04.5 LTS / kvm |
| VM egress (Cloudflare colo, country) | HEL FI |
| Node (benchmark / image default) | v22.22.3 / v26.13.0 |
| Repeats (fresh VMs) | 8 |
| Run started (UTC) | 2026-11-11T09:52:15Z |
| Run URL | - |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | higher | 21.21 [21.18-21.30] n=9 | - | - | only one side measured |
| (b) API round-trip baseline: authenticated GET of the VM, p50 of 25 | ms | lower | 0.286 [0.280-0.294] n=3 | - | - | only one side measured |
| (b) create -> exec-ready | s | lower | 1.39 [1.39-1.40] n=8 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50 | ms | lower | 2.21 [2.19-2.24] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p95 of 50 | ms | lower | 2.65 [2.57-2.76] n=3 | - | - | only one side measured |
| (b) fork -> fork exec-ready | s | lower | 0.556 [0.552-0.602] n=3 | - | - | only one side measured |
| (b) stop (until stopped) | s | lower | 0.627 [0.615-0.642] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready | s | lower | 0.211 [0.196-0.222] n=3 | - | - | only one side measured |
| (b) snapshot (until usable) | s | lower | 1.67 [1.47-1.73] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready | s | lower | 0.227 [0.201-0.232] n=3 | - | - | only one side measured |
| (c) 10 concurrent creates, wall to all exec-ready | s | lower | 4.25 [4.18-4.30] n=3 | - | - | only one side measured |
| (d) disk seq write 1M † | MB/s | higher | 351.1 [334.1-359.9] n=3 | - | - | only one side measured |
| (d) disk seq read 1M | MB/s | higher | 5,265 [4,366-5,830] n=3 | - | - | only one side measured |
| (d) disk rand write 4k † | IOPS | higher | 84,281 [82,666-85,408] n=3 | - | - | only one side measured |
| (d) disk rand read 4k | IOPS | higher | 200,768 [199,228-202,214] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand read 4k | IOPS | higher | 202,589 [129,914-206,160] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand write 4k † | IOPS | higher | 159,704 [158,989-160,201] n=3 | - | - | only one side measured |
| (d) PTS fio job: seq write 1M † | MiB/s | higher | 2,476 [2,220-2,504] n=3 | - | - | only one side measured |
| (d) rand write 4k, fsync after every write, iodepth 1 † | IOPS | higher | 25,190 [25,098-25,370] n=3 | - | - | only one side measured |
| (d) seq write 1M, fsync at the end † | MB/s | higher | 686.9 [624.8-1,014] n=3 | - | - | only one side measured |
| (e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com) | Mbit/s | higher | 519.0 [489.3-651.0] n=3 | - | - | only one side measured |
| (e) download Node.js 22.22.3 tarball, ~30 MB (nodejs.org) | Mbit/s | higher | 842.4 [839.0-848.8] n=3 | - | - | only one side measured |
| (e) TCP connect 1.1.1.1, median of 10 | ms | lower | 1.56 [1.48-1.77] n=3 | - | - | only one side measured |
| (e) TCP connect github.com, median of 10 | ms | lower | 24.16 [24.02-24.25] n=3 | - | - | only one side measured |
| (f) cold `npm ci` of pinned project | s | lower | 2.51 [2.49-2.52] n=3 | - | - | only one side measured |
| (b) exec round trip `true`, p50 of 50, NET of API round trips (raw - k x baseline p50) | ms | lower | 1.93 [1.90-1.96] n=3 | - | - | only one side measured |
| (b) create -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 1.42 [1.40-1.43] n=3 | - | - | only one side measured |
| (b) fork -> fork exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.555 [0.552-0.602] n=3 | - | - | only one side measured |
| (b) stop (until stopped), NET of API round trips (raw - k x baseline p50) | s | lower | 0.627 [0.615-0.642] n=3 | - | - | only one side measured |
| (b) resume -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.210 [0.195-0.222] n=3 | - | - | only one side measured |
| (b) snapshot (until usable), NET of API round trips (raw - k x baseline p50) | s | lower | 1.67 [1.47-1.73] n=3 | - | - | only one side measured |
| (b) new-from-snapshot -> exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 0.227 [0.201-0.232] n=3 | - | - | only one side measured |
| (c) 10 concurrent creates, wall to all exec-ready, NET of API round trips (raw - k x baseline p50) | s | lower | 4.24 [4.18-4.30] n=3 | - | - | only one side measured |

† PandaOS write numbers: Firecracker's drive `cache_type` was **Unsafe (boatd default diskCache)**. In `Unsafe` mode the guest's flushes are not passed to the host, so fsync and O_DIRECT writes complete once they reach the host page cache, not the disk. They are not durable-write numbers, and boat.dev's need not be the same kind. `diskCache: Writeback` makes boatd honour guest flushes.

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/AnicetNgrt/hpc-sandbox-benchmarks/tree/ed8ec45a1ee0f5058ca99944d9c569d49b2f8e67/docs/results/2026-08-24-cpu (leaderboard.md, README.md, run.json), linked from boat.dev's benchmark section (measured by the vendor on 2026-08-24 (Ascii Box, Freestyle); the other rows are from the vendor's July CI run). Vendor-published (boat.dev was formerly Ascii Box; its row is 'Ascii Box (bare metal)'). Despite that label, 27.67 is NOT a bare-metal host score: run.json's observedSpecs for that row report a 4 vCPU / 7.76 GiB KVM guest (virtualization kvm, manufacturer QEMU, Q35, SeaBIOS, detectedIsolation vm) with cpuModel AMD Ryzen 9 9950X, egress OVH Lille. It is a sandbox VM on a dedicated host. The host itself was not measured. boat.dev's '1,000 runs in 36 s' is 1000 / 27.67 runs/s. Their run: PTS pts/node-web-tooling-1.0.1 with DynamicRunCount, median of 21 trials on fresh machines; the Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (runs/s) | 27.67 | 21.21 [21.18-21.30] n=9 | - | p50 of 21 trials, 95% CI 26.01-28.34; p95 28.72 |
| pts_rand_read_iops (IOPS) | 433,500 | 202,589 [129,914-206,160] n=3 | - | pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here, but a loop file on a shared runner cannot speak for NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | 159,704 [158,989-160,201] n=3 | - | pts/fio-2.1.0: 4KB rand write, same job. A loop file on a shared runner cannot speak for NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | 2,476 [2,220-2,504] n=3 | - | pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. A loop file on a shared runner cannot speak for NVMe |

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

Identical scripts. Host: Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset untrusted; 8 CPUs online. Disk: both sides write NOCOW files on the same btrfs loop store. The VM adds its guest ext4, virtio and Firecracker's I/O engine, and its O_DIRECT stops at the host page cache, so a ratio above 1 does not mean a VM beats the host.

| Metric | Unit | Runner host, no VM (measured) | PandaOS VM (measured) | VM / host |
|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | 23.02 [22.88-23.05] n=9 | 21.21 [21.18-21.30] n=9 | 0.921 |
| (d) disk seq write 1M | MB/s | 4,250 [4,239-4,264] n=3 | 351.1 [334.1-359.9] n=3 | 0.083 |
| (d) disk seq read 1M | MB/s | 7,591 [7,174-7,769] n=3 | 5,265 [4,366-5,830] n=3 | 0.694 |
| (d) disk rand write 4k | IOPS | 123,432 [121,996-123,450] n=3 | 84,281 [82,666-85,408] n=3 | 0.683 |
| (d) disk rand read 4k | IOPS | 246,344 [246,143-246,844] n=3 | 200,768 [199,228-202,214] n=3 | 0.815 |
| (d) PTS fio job: rand read 4k | IOPS | 247,222 [247,036-247,308] n=3 | 202,589 [129,914-206,160] n=3 | 0.819 |
| (d) PTS fio job: rand write 4k | IOPS | 122,329 [122,108-122,381] n=3 | 159,704 [158,989-160,201] n=3 | 1.31 |
| (d) PTS fio job: seq write 1M | MiB/s | 4,137 [3,970-4,157] n=3 | 2,476 [2,220-2,504] n=3 | 0.599 |
| (d) rand write 4k, fsync after every write, iodepth 1 | IOPS | 77.00 [76.50-78.00] n=3 | 25,190 [25,098-25,370] n=3 | 327.1 |
| (d) seq write 1M, fsync at the end | MB/s | 5,242 [5,211-5,260] n=3 | 686.9 [624.8-1,014] n=3 | 0.131 |

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
| 1 | 20.96 [20.93-20.98] n=2 | 20.96 [20.93-20.98] n=2 | 1.00 [1.00-1.00] n=2 | 1.48 [1.47-1.49] n=2 | 0.185 [0.184-0.186] n=2 | 8.00 |
| 2 | 19.38 [19.36-19.40] n=2 | 38.76 [38.72-38.80] n=2 | 0.925 [0.925-0.925] n=2 | 1.49 [1.48-1.49] n=2 | 0.370 [0.368-0.373] n=2 | 8.00 |
| 4 | 15.98 [15.92-16.05] n=2 | 63.96 [63.68-64.25] n=2 | 0.763 [0.760-0.765] n=2 | 1.38 [1.38-1.38] n=2 | 0.740 [0.735-0.745] n=2 | 8.00 |
| 7 | 10.82 [10.82-10.82] n=2 | 75.91 [75.88-75.94] n=2 | 0.516 [0.516-0.517] n=2 | 1.08 [1.08-1.08] n=2 | 1.30 [1.29-1.30] n=2 | 8.00 |

### PROJECTION: Hetzner AX102, untrusted (15 VMs on 16 CPUs), NOT A MEASUREMENT

Load = 15 VMs x 1.48 CPU per loop / 16 CPUs = **1.39 loops per CPU** (OUTSIDE the measured range: the nearest measured point is used). Measured efficiency there: **0.482**, so a packed host delivers 15 x 0.482 = 7.2 solo-VM equivalents, and cost per run is **2.07x** the uncontended figure.
With this runner's solo speed (20.96 runs/s, a different CPU) as the per-VM speed: $0.4188/h / (15 x 20.96 x 0.482) = **0.000768 USD per 1000 runs (MIXED CPUs)**. Nobody ran PandaOS on that server; the efficiency curve comes from this runner, whose CPU, SMT layout and memory bandwidth differ.

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Cost = side's price per hour (prices.json) x time. A per-run cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 Node loop runs | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0279 | AMD Ryzen 9 7950X3D (Zen 4), Hetzner AX102 | AMD EPYC | 0.000365 (MIXED CPUs) | 0.000019 (MIXED CPUs) |
| boatdev | 0.0360 | AMD Ryzen 9 9950X (Zen 5), as boat.dev's guest reported it in the vendor's benchmark run | - | - | - |

CPU-consistent figure: boat.dev's price with boat.dev's own published speed on the CPU it sells (27.67 runs/s, vendor-measured, AMD Ryzen 9 9950X 16-Core Processor): **0.000361 USD per 1000 runs**. PandaOS has no CPU-consistent figure yet: its price is an AX102 (Ryzen 9 7950X3D, Zen 4), and nobody has run boatbench on one. The 9950X projection below is not that CPU either.

## Caveats

- The PandaOS numbers from CI come from a GitHub-hosted runner, NOT a dedicated server: shared 4 vCPU, KVM exposed to the runner VM (Firecracker runs nested inside Azure's hypervisor), US region. boat.dev runs in the EU (Germany/Finland/France). Comparisons are indicative until `run-on-server.sh` runs on the owner's dedicated server.
- On the runner, each PandaOS VM (4 vCPU / 8 GiB) gets the whole 4-vCPU host, and the 10-VM concurrency test oversubscribes it (boatd memOvercommit/cpuOvercommit raised just so it fits). boat.dev places VMs on its fleet.
- Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); boat.dev stop archives the disk and resume/fork restore it in the background after `ready` (docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.
- Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled to `ready`). New-from-snapshot uses each one's create-from-snapshot.
- Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.
- Raw lifecycle latencies include the API round trip from the harness host to the provider: the CI PandaOS job talks to localhost, the boat.dev job talks to boat.dev over the internet from a US runner. The NET rows subtract k x that side's measured API round-trip p50, k = the fewest sequential API calls the timing contains (drivers.py ROUND_TRIPS: 2 for create/fork/resume/from-snapshot, stop and snapshot 1 on PandaOS and 2 on boat.dev, which polls them).
- Disk numbers on the runner come from a btrfs loop file on a shared cloud disk. They cannot speak for NVMe, including boat.dev's published numbers, even when the fio job is the same.
- Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean no clear winner.
- Disk tool differs between sides: {'pandaos': {'fio', '-'}}. dd numbers are not comparable with fio.
