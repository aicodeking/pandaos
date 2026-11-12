# boatbench: PandaOS boat vs boat.dev

Measured numbers below come from this harness (`bench/boatbench`). Published numbers are boat.dev's own claims and are labelled as such. Method: `bench/boatbench/README.md`.

## Sides: hardware and location

| | pandaos |
|---|---|
| Host / location (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB; PandaOS 26.05 install (NixOS, kernel 6.18.57); boat store = native btrfs (compress=zstd:1) on one NVMe, Samsung MZVL2512HCJQ; boatd defaults (trusted, memOvercommit 1.5), owner team quota 8 default VMs; governor performance, EPP performance; 16 CPUs online |
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
| Repeats (fresh VMs) | 2 |
| Run started (UTC) | 2026-11-12T06:09:10Z |
| Run URL | - |
| Source commit (boat / PandaOS) | not recorded |

## Measured results

median [IQR q1-q3] n=samples; the verdict compares medians and notes overlapping IQRs.

**boat.dev was not measured by this harness (no `BOATDEV_API_KEY`), so no row has a measured-vs-measured verdict.** The only boat.dev numbers are the vendor's published claims (next section), and its 27.67 runs/s is one solo VM, not a fully packed server.

| Metric | Unit | Better | PandaOS (measured) | boat.dev (measured) | PandaOS / boat.dev | Verdict |
|---|---|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | higher | 21.43 [21.41-21.48] n=3 | - | - | only one side measured |
| (b) create -> exec-ready | s | lower | 1.59 [1.59-1.59] n=2 | - | - | only one side measured |

† PandaOS write numbers: Firecracker's drive `cache_type` was **Unsafe (boatd default diskCache)**. In `Unsafe` mode the guest's flushes are not passed to the host, so fsync and O_DIRECT writes complete once they reach the host page cache, not the disk. They are not durable-write numbers, and boat.dev's need not be the same kind. `diskCache: Writeback` makes boatd honour guest flushes.

## Published (boat.dev's claims) vs measured (this harness)

Source: https://github.com/AnicetNgrt/hpc-sandbox-benchmarks/tree/ed8ec45a1ee0f5058ca99944d9c569d49b2f8e67/docs/results/2026-08-24-cpu (leaderboard.md, README.md, run.json), linked from boat.dev's benchmark section (measured by the vendor on 2026-08-24 (Ascii Box, Freestyle); the other rows are from the vendor's July CI run). Vendor-published (boat.dev was formerly Ascii Box; its row is 'Ascii Box (bare metal)'). Despite that label, 27.67 is NOT a bare-metal host score: run.json's observedSpecs for that row report a 4 vCPU / 7.76 GiB KVM guest (virtualization kvm, manufacturer QEMU, Q35, SeaBIOS, detectedIsolation vm) with cpuModel AMD Ryzen 9 9950X, egress OVH Lille. It is a sandbox VM on a dedicated host. The host itself was not measured. boat.dev's '1,000 runs in 36 s' is 1000 / 27.67 runs/s. Their run: PTS pts/node-web-tooling-1.0.1 with DynamicRunCount, median of 21 trials on fresh machines; the Node version is not stated.

| Metric | boat.dev PUBLISHED | PandaOS MEASURED | boat.dev MEASURED (this harness) | Note |
|---|---|---|---|---|
| node_runs_s (runs/s) | 27.67 | 21.43 [21.41-21.48] n=3 | - | p50 of 21 trials, 95% CI 26.01-28.34; p95 28.72 |
| pts_rand_read_iops (IOPS) | 433,500 | - | - | pts/fio-2.1.0: 4KB rand read, O_DIRECT, libaio, 1 job, iodepth 64, 1 GiB, 60 s. Same job here; PandaOS's disk, filesystem and cache mode differ (see the report's caveats), so its numbers cannot speak for boat.dev's NVMe |
| pts_rand_write_iops (IOPS) | 412,500 | - | - | pts/fio-2.1.0: 4KB rand write, same job. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |
| pts_seq_write_mibps (MiB/s) | 5,838 | - | - | pts/fio-2.1.0: 1MB seq write, same job. PTS labels this MB/s, but it equals their 1MB IOPS (5837), so it is MiB/s. PandaOS's disk differs, so its numbers cannot speak for boat.dev's NVMe |

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

## Host vs PandaOS VM (MEASURED, same host)

Identical scripts. Host: Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB; PandaOS 26.05 install (NixOS, kernel 6.18.57); boat store = native btrfs (compress=zstd:1) on one NVMe, Samsung MZVL2512HCJQ; boatd defaults (trusted, memOvercommit 1.5), owner team quota 8 default VMs; governor performance, EPP performance; 16 CPUs online. Disk: both sides write to the same btrfs store (as declared); the host side's files are NOCOW. The VM adds its guest ext4, virtio and Firecracker's I/O engine, and its O_DIRECT stops at the host page cache, so a ratio above 1 does not mean a VM beats the host.

| Metric | Unit | Host, no VM (measured) | PandaOS VM (measured) | VM / host |
|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | 22.93 [22.21-23.05] n=3 | 21.43 [21.41-21.48] n=3 | 0.935 |

## PROJECTION of PandaOS on AMD Ryzen 9 9950X (NOT A MEASUREMENT)

**Nobody ran PandaOS on this CPU. These numbers are arithmetic, not benchmark results.** Estimated Node loop range: **25.9-26.9 runs/s**.

| Method | Projected runs/s (not measured) |
|---|---|
| vendor's VM score x our VM/host: 27.67 x 0.935. A floor if the vendor's VM also has overhead, because its host then scores above 27.67 | 25.9 |
| public host average x our VM/host: 28.8 x 0.935 | 26.9 |

Limits: the VM/host ratio (0.935) was measured on this host (see the Host line above). None of this says anything about memory, disk or network hardware.

Same-benchmark source: https://openbenchmarking.org/test/pts/node-web-tooling, per-CPU runs/s averages, retrieved 2026-11-03: 'AMD EPYC 7763 64-Core' 10.3 +/- 0.4 (4 public results), 'AMD Ryzen 9 9950X 16-Core' 28.8 +/- 2.6 (30 public results). PassMark source: https://www.cpubenchmark.net/compare/4207vs6211/AMD-EPYC-7763-vs-AMD-Ryzen-9-9950X, retrieved 2026-11-03.

## (g) Density under load: pandaos (MEASURED)

N VMs run the Node loop at the same time from one barrier, all on this one host. Efficiency = per-VM runs/s at N / at N=1. CPU per loop = (user+sys)/real of node in the guest. median [IQR] n=samples.

| N | per-VM runs/s | host runs/s (sum) | efficiency | CPU per loop | loops per host CPU | host CPUs |
|---|---|---|---|---|---|---|
| 1 | 20.97 [20.97-20.97] n=1 | 20.97 [20.97-20.97] n=1 | 1.00 [1.00-1.00] n=1 | 1.48 [1.48-1.48] n=1 | 0.093 [0.093-0.093] n=1 | 16.00 |
| 4 | 15.97 [15.97-15.97] n=1 | 63.92 [63.92-63.92] n=1 | 0.762 [0.762-0.762] n=1 | 1.48 [1.48-1.48] n=1 | 0.371 [0.371-0.371] n=1 | 16.00 |
| 8 | 10.51 [10.51-10.51] n=1 | 84.06 [84.06-84.06] n=1 | 0.501 [0.501-0.501] n=1 | 1.43 [1.43-1.43] n=1 | 0.742 [0.742-0.742] n=1 | 16.00 |

### Cost per run on Hetzner AX42, MEASURED (price and speed from the same CPU)

USD per 1000 runs = $0.1631/h / 3.6 / measured host runs/s, at 100% busy, no retention; + operator time: $0.3685/h. boatd admits 11 `default` VMs on it in trusted mode (prices.json). boat.dev reference: $0.000361 (its price at its PUBLISHED solo speed; its speed under load is unmeasured).

| N VMs | host runs/s (measured) | USD per 1000 runs | + operator time | vs boat.dev reference |
|---|---|---|---|---|
| 1 | 20.97 | $0.002160 | $0.004882 | 5.98x |
| 4 | 63.92 | $0.000709 | $0.001602 | 1.96x |
| 8 | 84.06 | $0.000539 | $0.001218 | 1.49x |

## (g) Host-native density baseline vs VMs (MEASURED, same host)

N copies of the Node loop directly on the host (no VM), against N VMs each running one. Both lose speed as the CPUs fill; the VM / host ratio at each N is what virtualization adds.

| N | host loops: per-loop / total runs/s | efficiency | VMs: per-VM / total runs/s | efficiency | VM / host at this N |
|---|---|---|---|---|---|
| 1 | 23.10 / 23.10 | 1.00 | 20.97 / 20.97 | 1.00 | 0.908 |
| 4 | 17.71 / 70.66 | 0.767 | 15.97 / 63.92 | 0.762 | 0.902 |
| 8 | 11.65 / 93.08 | 0.504 | 10.51 / 84.06 | 0.501 | 0.902 |
| 11 | 8.96 / 98.64 | 0.388 | - / - | - | - |

## Cost

**These rows use ONE uncontended VM's speed**, while the PandaOS price assumes a fully packed host. Workload (g) measures the packed case; where it ran, its projection above is the fairer PandaOS figure.

Cost = side's price per hour (prices.json) x time. A per-run cost only means something when the price and the speed come from the same CPU. **Rows marked MIXED CPUs are arithmetic, not a cost of any real machine.**

| Side | USD/hour | The price buys | Speed measured on | USD per 1000 Node loop runs | USD per cold npm ci |
|---|---|---|---|---|---|
| pandaos | 0.0233 | AMD Ryzen 7 PRO 8700GE | AMD EPYC (guest); harness host AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16 | 0.000302 | - |
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
