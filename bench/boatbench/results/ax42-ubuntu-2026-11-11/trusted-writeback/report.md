# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted-writeback; 16 CPUs online |
| Harness client CPU | AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 |
| Harness client egress (Cloudflare colo) | HEL DE |
| Requested size | default |
| Preset (presets.json) | trusted-writeback |
| boatd options over the defaults | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16},"diskCache":"Writeback"} |
| Firecracker drive cache_type | Writeback (boatd.json diskCache) |
| VM vCPU / RAM / disk (measured) | 4 vCPU / 7965 MiB / 50.7 GiB |
| VM CPU model | AMD EPYC |
| VM kernel / OS / virt | 6.1.191 / Ubuntu 24.04.5 LTS / kvm |
| VM egress (Cloudflare colo, country) | HEL FI |
| Node (benchmark / image default) | - / v26.13.0 |
| Repeats (fresh VMs) | 3 |
| Run started (UTC) | 2026-11-11T11:00:39Z |
| Run URL | - |
| Source commit (boat / PandaOS) | not recorded |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and its 27.67 runs/s is one solo VM, not a fully packed server.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (b) create -> exec-ready | s | lower | 1.73 [1.72-5.43] n=3 | - | - | only one side measured |
| (d) disk seq write 1M | MB/s | higher | 2,819 [2,728-3,330] n=3 | - | - | only one side measured |
| (d) disk seq read 1M | MB/s | higher | 9,959 [8,706-10,415] n=3 | - | - | only one side measured |
| (d) disk rand write 4k | IOPS | higher | 143,135 [141,590-144,300] n=3 | - | - | only one side measured |
| (d) disk rand read 4k | IOPS | higher | 174,697 [174,570-175,520] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand read 4k | IOPS | higher | 179,819 [178,664-179,845] n=3 | - | - | only one side measured |
| (d) PTS fio job: rand write 4k | IOPS | higher | 168,718 [168,028-169,090] n=3 | - | - | only one side measured |
| (d) PTS fio job: seq write 1M | MiB/s | higher | 4,056 [3,913-4,458] n=3 | - | - | only one side measured |
| (d) rand write 4k, fsync after every write, iodepth 1 | IOPS | higher | 38.00 [38.00-38.50] n=3 | - | - | only one side measured |
| (d) seq write 1M, fsync at the end | MB/s | higher | 3,517 [2,854-4,036] n=3 | - | - | only one side measured |

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/AnicetNgrt/hpc-sandbox-benchmarks/tree/ed8ec45a1ee0f5058ca99944d9c569d49b2f8e67/docs/results/2026-08-24-cpu (leaderboard.md, README.md, run.json), linked from boat.dev's benchmark section (measured by the vendor on 2026-08-24 (Ascii Box, Freestyle); the other rows are from the vendor's July CI run). Vendor-published (boat.dev was formerly Ascii Box; its row is 'Ascii Box (bare metal)'). Despite that label, 27.67 is NOT a bare-metal host score: run.json's observedSpecs for that row report a 4 vCPU / 7.76 GiB KVM guest (virtualization kvm, manufacturer QEMU, Q35, SeaBIOS, detectedIsolation vm) with cpuModel AMD Ryzen 9 9950X, egress OVH Lille. It is a sandbox VM on a dedicated host. The host itself was not measured. boat.dev's '1,000 runs in 36 s' is 1000 / 27.67 runs/s. Their run: PTS pts/node-web-tooling-1.0.1 with DynamicRunCount, median of 21 trials on fresh machines; the Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (runs/s) | 27.67 | - | - | p50 of 21 trials, 95% CI 26.01-28.34; p95 28.72 |
| pts_rand_read_iops (IOPS) | 433,500 | 179,819 [178,664-179,845] n=3 | - | pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here; PandaOS's disk, filesystem and cache mode differ (see the report's caveats), so its numbers cannot speak for boat.dev's NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | 168,718 [168,028-169,090] n=3 | - | pts/fio-2.1.0: 4KB rand write, same job. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | 4,056 [3,913-4,458] n=3 | - | pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |

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

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Cost = side's price per hour (prices.json) x time. A per-run cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 Node loop runs | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0233 | AMD Ryzen 7 PRO 8700GE | AMD EPYC (guest); harness host AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 | - | - |
| boatdev | 0.0360 | AMD Ryzen 9 9950X (Zen 5), as boat.dev's guest reported it in the vendor's benchmark run | - | - | - |

CPU-consistent figure: boat.dev's price with boat.dev's own published speed on the CPU it sells (27.67 runs/s, vendor-measured, AMD Ryzen 9 9950X 16-Core Processor): **0.000361 USD per 1000 runs**. PandaOS's price here is AMD Ryzen 7 PRO 8700GE: its per-run cost is CPU-consistent only where the speed above was measured on that CPU (the density section says which).

## Caveats

- The PandaOS numbers come from a dedicated server, with the harness on the same box talking to its local API. The server, its disks and its store are the harness-declared host in the table above. boat.dev runs in the EU (Germany/Finland/France).
- Lifecycle verbs differ in meaning: PandaOS stop/resume and fork keep guest memory (Firecracker snapshots); boat.dev stop archives the disk and resume/fork restore it in the background after `ready` (docs.boat.dev/readiness.md). Both are measured to the first successful `true` exec.
- Snapshot: PandaOS `POST /v1/vms/{id}/snapshots` vs boat.dev saved snapshot (`POST /saved-snapshots`, polled to `ready`). New-from-snapshot uses each one's create-from-snapshot.
- Network numbers mix the VM's egress path with the distance from each provider's region to the CDN edge.
- Raw lifecycle latencies include the API round trip from the harness host to the provider: the PandaOS harness talks to the API on the same box, the boat.dev side (if measured) talks to boat.dev over the internet. The NET rows subtract k x that side's measured API round-trip p50, k = the fewest sequential API calls the timing contains (drivers.py ROUND_TRIPS: 2 for create/fork/resume/from-snapshot, stop and snapshot 1 on PandaOS and 2 on boat.dev, which polls them).
- Disk numbers come from this server's boat store as declared above. boat.dev's published disk numbers come from other NVMe drives with an unpublished fio target and cache mode, so even the same fio job is not a like-for-like disk comparison.
- Small samples (default 5 fresh VMs per side). The table shows median and IQR; overlapping IQRs mean no clear winner.
