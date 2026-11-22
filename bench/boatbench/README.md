# boatbench: PandaOS boat vs boat.dev

The same workloads, on the same VM size, through each provider's HTTP API.
Python 3 stdlib only.

```sh
python3 bench/boatbench/test_boatbench.py                        # unit tests
python3 bench/boatbench/test_boatdev.py                          # BoatDev driver vs boat.dev's OpenAPI, offline
BOAT_URL=... BOAT_KEY=... python3 bench/boatbench/boatbench.py run --driver pandaos
BOATDEV_API_KEY=boat_... python3 bench/boatbench/boatbench.py run --driver boatdev
python3 bench/boatbench/boatbench.py report boatbench-results/results-*.json --out report.md
python3 bench/boatbench/boatbench.py presets results-pandaos-*.json   # PandaOS per boatd preset
bench/boatbench/run-on-server.sh                                 # owner's server, both sides + report
```

CI: `.github/workflows/boatbench.yml` (workflow_dispatch). The `pandaos` job builds
boatd + boat-api from source on a GitHub KVM runner (`stack.sh`). The `boatdev` job
runs only if the `BOATDEV_API_KEY` secret exists, and prints SKIPPED otherwise.
It runs one `pandaos` job per boatd preset (`presets` input, default all three). The
`report` job writes `report-<preset>.md` per preset and `presets.md` (the presets side
by side) into the job summary and an artifact.

## What boat.dev published, and what we reproduce

boat.dev's home page (re-read 2026-11-17) now cites StarSling's run of 3 October 2026
(run 37073680030): "V8 Web Tooling Benchmark on a 4 vCPU / 8 GB sandbox ... boat: 28.06 runs per
second, second fastest" (Brezel 30.06, Namespace 24.90, Daytona 21.52, ... 15 providers). The
[leaderboard](https://github.com/starslingdev/hpc-sandbox-benchmarks/blob/297198a96ee827df5edcd918bd37b949526cfe20/LEADERBOARD.md)
(commit `f571eec`, generated 2026-10-03) gives boat 28.06 with a 95% bootstrap interval of 23.27-28.22
(5 sandboxes, 10 trials), lists boat as a KVM virtual machine, and marks the whole run **partial**
(800 of 840 planned cells). boat.dev's page says every boat sandbox in that run ran on an AMD Ryzen 9 9950X
host (the fleet also has Ryzen 7 9700X hosts, not measured). `published.json` and the reports use **28.06**.

The earlier figure was **27.67 geometric-mean index units**, from the
"Ascii Box (bare metal)" row of
[AnicetNgrt/hpc-sandbox-benchmarks](https://github.com/AnicetNgrt/hpc-sandbox-benchmarks)
(`docs/results/2026-08-24-cpu/`, 21 trials, 95% CI 26.01-28.34; re-fetched 2026-11-17). boat.dev was
formerly Ascii Box. It is kept in `published.json` under `historical` and is not rendered as current.
The two runs differ in harness repo, trial count and provider set; +1.4% is inside both intervals, so it is
no evidence of a speed-up. The three fio disk figures (`pts_*`) in `published.json` are still from the
2026-08-24 run: the October run reports different fio metrics, so they are not refreshed. What the
2026-08-24 repo shows (the 28.06 run's own harness was not inspected beyond its leaderboard):

- The "loop" is **Phoronix Test Suite `pts/node-web-tooling-1.0.1`**: the
  [V8 Web Tooling Benchmark](https://github.com/v8/web-tooling-benchmark) v0.5.3
  (Babel, TypeScript, Babylon, Terser, ... in Node), built by PTS's `install.sh`
  (patched lockfile + `npm install`, which runs webpack), then `node dist/cli.js`.
- **The geometric-mean index** is the benchmark's own "Geometric mean: N runs/s" line,
  combining subtest rates. It does not count completed Node jobs or full suites/s.
  Dividing 1000 by 27.67 does not measure time to finish 1000 jobs.
  No repository is cloned and no project test suite runs.
- They ran 21 trials on fresh machines (PTS DynamicRunCount inside each), and the
  published number is the median. They did not say which Node version they used.
- **"Bare metal" is the host, not what was measured.** `run.json` reports that row as a
  4 vCPU / 7.76 GiB KVM guest (QEMU Q35, SeaBIOS, `detectedIsolation: vm`) on an
  **AMD Ryzen 9 9950X**, egress OVH Lille. So 27.67 was a sandbox VM on a dedicated
  desktop-class CPU. boat.dev's own Hetzner row, on a shared EPYC-Rome, scores 8.88.
- The Vercel Sandbox row was on boat.dev's August page but not in that August leaderboard (the October leaderboard has it at 12.66).

### Workload (a), the closest reproducible version

- The same PTS profile, `install.sh` from `phoronix-test-suite/test-profiles@1b05a0e`
  (sha256 pinned), and the same tarball (sha256 pinned). We run the profile's own steps
  without the PTS PHP runner.
- Node is pinned to **v22.22.3** (an official tarball with its sha256 checked) and is
  the same on both sides. This takes the guest images' Node versions out of the
  comparison (our image ships Node 26). The report also prints each image's own Node.
- 3 passes per VM (the profile's `TimesToRun`) and N fresh VMs (default 5). Within each
  VM, the 3 passes are aggregated by their per-VM median first so across-VM median, IQR
  and `n` reflect independent VMs (`n = VMs`), not pseudoreplicated intra-VM passes. PTS
  DynamicRunCount is not used.

### The other workloads (our definitions; boat.dev publishes nothing for these)

| | What | How |
|---|---|---|
| b | lifecycle | create→exec-ready, fork→exec-ready, resume→exec-ready, new-from-snapshot→exec-ready: time from the API call until `true` exits 0 (polled with adaptive backoff, `0.05 s` stepping to `0.10 s` → `0.20 s`, the same loop on both sides). stop: until the provider reports it stopped (PandaOS: synchronous call; boat.dev: `state == archived`). snapshot: until it can be used (PandaOS: synchronous; boat.dev saved snapshot `status == ready`). exec RTT: p50/p95 of 50 sequential `true` calls; HTTP 429 calls are retried and not counted |
| c | 10 concurrent creates | wall time from one shared start until all 10 answer `true` |
| d | disk | fio (apt-installed if missing; dd fallback, flagged in the report) in `$HOME`, O_DIRECT, libaio, 1 job. Ours: 1M seq write/read MB/s, 4k rand write/read IOPS, iodepth 32, 15 s, 2 GiB file. The 4k rand read runs three times: unpinned, then with `--cpus_allowed` on the vCPU taking the most virtio-blk interrupts of the disk (from `/proc/interrupts`), then on the vCPU taking the fewest: where fio lands moved it 4x on the AX42 (59k vs 219k IOPS). Off virtio or on 1 vCPU only the unpinned run exists; results from before this change hold only the unpinned one, labelled so. `pts_*`: the job file of [`pts/fio-2.1.0`](https://github.com/phoronix-test-suite/test-profiles/blob/d2f1a150d388bd062737b445891edda0780f7e25/pts/fio-2.1.0/install.sh), which boat.dev's numbers used: 4k rand read/write, 1M seq write, iodepth 64, 1 GiB, 20 s start delay, 5 s ramp, 60 s. The published comparison uses these. Two flushing jobs: 4k rand write with `--fsync=1` at iodepth 1, and 1M seq write with `--end_fsync=1`: the only ones where Firecracker's `Writeback` cache (guest flushes reach the disk) differs from `Unsafe`. A loop file on a shared runner still cannot speak for NVMe |
| e | network | curl download of two fixed files on two CDNs: the Go 1.25.0 linux-amd64 tarball (59.7 MB, dl.google.com) and the pinned Node tarball (nodejs.org) in Mbit/s; error pages (<1 MB) count as failures, not samples. speed.cloudflare.com was dropped because it answers curl with 403; TCP connect time (connect minus DNS) to 1.1.1.1 and github.com, median of 10 |
| g | density under load | N VMs (`BOATBENCH_DENSITY_LEVELS`, default 1,2,4,6) run 2 passes of the Node loop (a) at once from one barrier. Every VM is fresh and set up on its own (no forks: on PandaOS a long exec on a fork's source VM never returned, run 42818758694); VMs the provider refuses are left out. One pass may take 900 s at most. Per level: per-VM geometric-mean index, aggregate host index (their sum; PandaOS only, since all VMs share one host), efficiency (per-VM at N / at 1), CPU per loop ((user+sys)/real of node in the guest) and loops per host CPU (host CPUs = CPUs online where the harness runs, or `BOATBENCH_HOST_CPUS`). The report projects the AX102 from the measured efficiency at its load, **labelled as a projection** |
| f | cold npm install | `npm ci --ignore-scripts` of `npm-project/` (committed lockfile, 538 packages: express, react, eslint, jest, typescript, webpack), empty cache, pinned Node's npm |

Sizes: PandaOS `default` = 4 vCPU / 8192 MiB / 51 GiB (boatd's default). boat.dev
`default` = 4 vCPU / 8 GB / 51 GB (docs.boat.dev/pricing.md). Each repeat records what
the VM reports (`nproc`, MemTotal, disk, CPU model, kernel, virt, Cloudflare egress
colo). The report flags any VM that is not 4 vCPU / ~8 GiB.

Fairness: both adapters (`drivers.py`) are thin stdlib `http.client` keep-alive clients with no SDK
or CLI on either side, pooling persistent HTTP/TLS sockets per `(scheme, host, port)` so WAN calls
do not pay repeated TCP + TLS handshakes. Guest scripts (`workloads.py`) are the same bytes, run with `bash -lc` as
each provider's default user. Lifecycle times include the API round trip from the
harness host (localhost for CI's PandaOS, the WAN for boat.dev). So every repeat that
runs b or c first measures a baseline: 25 authenticated GETs of the VM
(`api_rtt_p50_ms`; PandaOS `GET /v1/vms/{id}`, boat.dev `GET /sandboxes/{id}`). Both drivers
poll async state transitions with the same adaptive fast backoff (`0.05 s` stepping to `0.10 s` →
`0.20 s`) and records exact serial critical-path HTTP calls (`api_calls`, excluding intermediate
async polls that overlap server-side work while retaining `poll_http_calls` and `http_calls` for
diagnostics) per lifecycle operation. Each lifecycle metric is reported raw and as `<metric>_net` =
max(0, raw − k × that p50), where k
is the recorded `api_calls` count when present, falling back to `drivers.py` `ROUND_TRIPS`
for historical artifacts (2 for create/fork/resume/from-snapshot, 1 for PandaOS stop and
snapshot, 2 for boat.dev's, 1 for exec RTT).

## boatd presets, options and the warm pool

`presets.json` holds four presets, and `BOATBENCH_PRESET` (the workflow's `presets`
input) picks one per job:

- `trusted`: boatd's defaults.
- `trusted-warm`: `warmPool` on.
- `trusted-writeback`: `diskCache` `Writeback`: Firecracker passes guest flushes to the
  host, so fsync and O_DIRECT writes are durable. Every other preset's write numbers
  are `Unsafe`-cache numbers.
- `untrusted`: `components/boat/boatd/untrusted.json`, the list that `module.nix` imports for
  `untrustedTenants`, key by key (the unit test compares them). Its settings: the disk shared by weight
  (`ioCost` with `ioCostQoS` latency targets) under an `io.max` ceiling of 4,000 MB/s and 200,000 IOPS;
  a 25% uplink share with fair queueing; `storageOvercommit` 1; `hostDirtyRatio` 20; no SMTP; no `warmPool`
  or KSM. Until `46507a8` the list instead had the `Sync` disk engine and 1 Gbit/s egress and ingress. The
  AX42 Ubuntu run used that older list.
  `stack.sh` first measures the state disk once for `ioCost` (`boatd -measure-disk`,
  about 25 s, stored in the store's `iocost-model`, as the module's
  `boat-iocost-model.service` does; any config with `ioCost` and no `ioCostModel` gets it
  too). It also turns SMT off if the runner allows it, resolves/creates the `boat-fc` group
  (`jailGid`), sets `kernel.io_uring_disabled=1` and `kernel.io_uring_group=$jail_gid` (so
  jailed Firecracker VMMs keep production `Async` `io_uring` disk I/O while unprivileged
  processes are blocked), configures `hostHttpsAddrs` alongside `hostHttpsPort`, stops KSM,
  and makes the store a sparse 1 TiB file so `storageOvercommit` 1 admits the bench's VMs.
  **`memOvercommit` 1 is applied** for `untrusted` (matching `untrusted.json` and `boatd`'s
  `untrustedTenants` validation, which rejects `memOvercommit > 1`; `stack.sh` also clamps
  any invalid `BENCH_MEM_OVERCOMMIT > 1` to `1` when `untrustedTenants` is enabled, while
  honoring valid values `<= 1`). Note that on a 16 GiB CI runner, `memOvercommit: 1` admits
  only one 8 GiB `default` VM; full multi-VM `untrusted` density runs require a dedicated
  server such as the AX42 (7 VMs at `memOvercommit: 1`).

The `boatd_options` input (or `BOATBENCH_BOATD_OPTIONS` for `stack.sh`) is JSON merged over
the bench `boatd.json` and the preset, and the report prints the effective `boatd.json`
and the Firecracker drive `cache_type`. With `{"warmPool":true}`, `stack.sh`
waits for the template before measuring, so a create cannot boot cold and pass as warm.
`warmPool` is off by default. VMs from one template share boot-time randomness, including
the ASLR offsets of running processes, across tenants, and every warm report says so.

## Host vs VM, and the projection

CI's `pandaos` job alternates workloads a and d between the runner itself
(`--driver host`, same scripts, no VM) and a fresh VM on it. The report prints the VM /
host ratio as a measurement. fio's host directory is on the same btrfs store as the VM
disks and NOCOW like them. A VM's O_DIRECT stops at the host page cache, so a disk ratio
above 1 does not mean the VM beats the host.

The report then prints a **projection** of PandaOS on the vendor's CPU, in its own section
marked NOT A MEASUREMENT. It scales the measured VM / host ratio by the vendor's score, by
OpenBenchmarking's public averages for this benchmark, and by the CPU ratio from those
averages or from PassMark single-thread (`published.json`, `cpu_scaling`). The vendor's
repo has no EPYC 7763 row. Only a run on comparable hardware can replace this estimate.

`stack.sh` sets `cpuOvercommit` from the CPUs online after SMT is turned off: room for
12 × 4 vCPUs, never below 12 (`BENCH_CPU_OVERCOMMIT` overrides). On a 4-vCPU runner that
stays 12; in `untrusted` (2 CPUs online) it is 24, so workload (c)'s 10 VMs fit.

## Latest measured runs

**Real hardware, 2026-11-11:** a Hetzner AX42 (Ryzen 7 PRO 8700GE, Zen 4, 8c/16t, 64 GB,
NVMe RAID1), all presets, density at 7 and 11 VMs, host vs VM. Tables, commands and
caveats: [`docs/cost.md`](../../docs/cost.md#measured-on-a-real-dedicated-server-hetzner-ax42).
On a server you do not want to repartition, keep everything under one directory with
`BENCH_WORK=/opt/boatbench/work BENCH_STATE=/opt/boatbench/state stack.sh up ...`, and
`unset BOATBENCH_BOATD_OPTIONS` before bringing up the next preset: the previous preset's
`$WORK/env` sets it, and `stack.sh` merges it over the new preset.

[42818758694](https://github.com/aicodeking/pandaos/actions/runs/42818758694) (`8cf09f8`,
workloads a–f, 5 repeats; overall `cancelled` because a first density step timed out, a–f
had finished) and [42860774575](https://github.com/aicodeking/pandaos/actions/runs/42860774575)
(`2a9f90b`, `success`: d with the flushing jobs, g density), presets `trusted`,
`trusted-warm`, `trusted-writeback`, `untrusted`. `boatdev` was not measured (no key). GitHub
gave each job a different CPU, so compare CPU-bound rows only through ratios on one runner.
Tables, projections and verdict: [`docs/cost.md`](../../docs/cost.md#measured-boatbench-on-a-github-runner).

## Cost

`prices.json` belongs to the cost comparison (`docs/cost.md`). The PandaOS price is not hard-coded.
Report prices follow the artifact's recorded `size`: boat.dev uses its saved `sizes` rate card
(`small`, `default`, `large`, `xlarge`). PandaOS per-VM admission pricing and density projections
support only `default`; other or missing shapes suppress cost cells rather than reuse default counts.
The vendor published-speed reference always uses the `default` rate.
`boatbench.py report ... --server KEY` picks a server from `servers[]`. The keys are `ax42`, `ax102` (the default,
`pandaos.default_server`), `ax162`, `ovh-advance-3` and `latitude-m4-medium`. Each entry carries its list price,
setup fee, backup cost, source URL, CPU and admission counts. From these, `report.load_prices` derives:
- the host's price per hour: (month + setup/12 + backup) / 730;
- the per-VM price: the host's price per hour divided by the `memOvercommit` 1.0 count;
- the density model (trusted: the 1.5 count on threads; untrusted: the untrusted count on cores).

USD per 1000 index units = usd_per_hour / 3600 × 1000 / geometric_mean_index. Cost per cold npm ci =
usd_per_hour / 3600 × seconds. If the file is missing, the report shows index units only.

The density section prints a **measured** cost per index unit table only when the speed was measured on the priced CPU. The
guest's model counts, and so does the harness host's when the harness runs on the PandaOS host. That table is
host $/h / 3.6 / measured aggregate host index at each N. On any other server the section prints a **projection** of the
measured curve, marked NOT A MEASUREMENT. Rows whose price and speed come from different CPUs are marked
**MIXED CPUs**.

`boatbench.py costs` prints the price-only tables in `docs/cost.md`: cost per VM-hour by utilization, break-even
against boat.dev, and stopped-VM retention. Retention is shown under the shipped `storageOvercommit` 1 policy, where
every VM, running or stopped, is charged its nominal 59 GiB. It is also shown under a thin-provisioning **model** that
charges actual use (`assumptions.retention`).

Workload g also runs on `--driver host`: N Node loops side by side directly on the host, with Node installed once.
The report puts that next to the VM curve. It compares ratios only when the recorded host description,
client CPU/OS and density host CPU counts match; missing or differing evidence is marked **UNMATCHED**
and ratios are suppressed. Matching ratios include guest, VMM and policy effects without isolating their causes. On a
host without an FHS loader (NixOS), set `BB_NODE_LOADER` to the glibc loader command
(`/nix/store/...-glibc-*/lib/ld-linux-x86-64.so.2 --library-path <glibc>/lib:<gcc-lib>/lib`). The same pinned Node
binary then runs through that loader. `BOATBENCH_RUN_ID` replaces the random run ID that names the VMs (`bb-<run_id>-*`). `BOATBENCH_CONCURRENT_N` sets how many VMs workload c creates at once (default
10), for example a team quota of 8.

Raw results of server runs are committed under `results/` (one directory per run, with a README giving the hardware,
OS, commit and commands).

The VM counts behind the prices come from `admission.py`, which reads boatd's admission
constants (`hostReserveMiB`, `vmOverheadMiB`, overcommits, the `default` size) from
`components/boat/boatd/config.go`. The unit tests recompute every count in
`prices.json` from it and fail if `manager.go` stops counting `vmOverheadMiB`.

## The boat.dev driver

`drivers.BoatDev` has not run against the live API (no `BOATDEV_API_KEY` secret yet).
`test_boatdev.py` checks every request it makes against `boatdev-openapi.json`, the
operations it uses cut from <https://docs.boat.dev/openapi/boat-v1.yaml> (the file
records the source's sha256 and fetch date; refresh it with
`boatdev_openapi_extract.py`, which needs PyYAML). Path and method, required headers,
body fields known to the spec and valid; the responses are fixtures validated against
the spec's response schemas. That check found two bugs, now fixed: delete needs
`X-Ascii-Confirm-Delete: <sandbox id>` (without it boat.dev answers 409 and the sandbox
keeps billing until its TTL), and fork takes no `name` and answers with the new id in
`id` (its `sandbox` may be null).

A full 5-repeat run starts 70 machines (4 per repeat in b, 10 in c), more than the $20
plan's 60 starts an hour (12 a minute): use the $100 plan, or fewer repeats.

## Caveats (also printed in every report)

Firecracker's drive `cache_type` defaults to `Unsafe`: the guest's flushes are not
passed to the host, so PandaOS write numbers stop at the host page cache. The report
marks those rows with † whenever the run's cache mode was `Unsafe`.

The GitHub runner is not a dedicated server. It has 4 shared vCPUs, KVM inside a cloud
VM, and runs in a US region. boat.dev runs in the EU. On the runner, the 10-VM test
oversubscribes the host (stack.sh raises boatd's overcommit only so that test fits).
The lifecycle verbs also mean different things: our stop/fork keep guest memory, while
boat.dev restores the disk in the background after `ready`. Treat the results as
indicative until someone runs `run-on-server.sh` on the owner's server.

Benchmark units: workload (a) reports the **Node.js web-tooling geometric-mean index**
(geometric mean of subtest runs/s). This is not a count of completed Node jobs or
full suites per second. Historical JSON keys `node_runs_s`, `per_vm_runs_s` and
`host_runs_s` retain their spelling; the last sums per-VM indices. Index-normalized
costs are USD per 1000 index units. Workload (g) additionally reports **completed
suites/s** where complete pass counts and level wall time were recorded, including
barrier and harness overhead. No vendor completed-suite rate is available, so it
is not compared with the vendor index. Prices and retention capacities are projections.
The retention table separates shipped raw-capacity sparse admission, fully occupied
quotas after reserves, and an unshipped thin-provisioning model.

Density and concurrent-create cleanup delete only IDs created by that workload. Each cleanup call retries
failed deletions once, never repeats a successful deletion, and retains unresolved IDs and error details.
The JSON repeat records `concurrent_cleanup` / `density_cleanup` (`pending_vms`, `errors`); a density
trial also keeps `cleanup_pending_vms` / `cleanup_errors`. Reports surface these failures, and `run`
returns nonzero if cleanup remains unresolved. The retained density collector can safely retry `cleanup()`.
Historical saved JSON is immutable and does not gain cleanup evidence from report regeneration.
