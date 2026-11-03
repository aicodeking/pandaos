# boatbench: PandaOS boat vs boat.dev

The same workloads, on the same VM size, through each provider's HTTP API.
Python 3 stdlib only.

```sh
python3 bench/boatbench/test_boatbench.py                        # unit tests
BOAT_URL=... BOAT_KEY=... python3 bench/boatbench/boatbench.py run --driver pandaos
BOATDEV_API_KEY=boat_... python3 bench/boatbench/boatbench.py run --driver boatdev
python3 bench/boatbench/boatbench.py report boatbench-results/results-*.json --out report.md
bench/boatbench/run-on-server.sh                                 # owner's server, both sides + report
```

CI: `.github/workflows/boatbench.yml` (workflow_dispatch). The `pandaos` job builds
boatd + boat-api from source on a GitHub KVM runner (`stack.sh`). The `boatdev` job
runs only if the `BOATDEV_API_KEY` secret exists, and prints SKIPPED otherwise.
The `report` job writes `report.md` into the job summary and an artifact.

## What boat.dev published, and what we reproduce

boat.dev's home page says "the same Node.js build & test loop, 1,000 runs, on each
provider's 4 vCPU / 8 GB machine": boat 36 s / 27.67 runs/s, Daytona 18.6, Vercel
Sandbox 12.96, E2B 11.24, Freestyle 9.64, Modal 9.56. It links
[AnicetNgrt/hpc-sandbox-benchmarks](https://github.com/AnicetNgrt/hpc-sandbox-benchmarks)
(branch `provider/ascii-box`, `docs/results/2026-08-24-cpu/`). boat.dev was formerly
Ascii Box, and its 27.67 is the "Ascii Box (bare metal)" row there. What that repo shows:

- The "loop" is **Phoronix Test Suite `pts/node-web-tooling-1.0.1`**: the
  [V8 Web Tooling Benchmark](https://github.com/v8/web-tooling-benchmark) v0.5.3
  (Babel, TypeScript, Babylon, Terser, ... in Node), built by PTS's `install.sh`
  (patched lockfile + `npm install`, which runs webpack), then `node dist/cli.js`.
- **runs/s** is the benchmark's own "Geometric mean: N runs/s" line. "1,000 runs in 36 s"
  is just 1000 / 27.67. No repository is cloned and no project test suite runs.
- They ran 21 trials on fresh machines (PTS DynamicRunCount inside each), and the
  published number is the median. They did not say which Node version they used.
- **"Bare metal" is the host, not what was measured.** `run.json` reports that row as a
  4 vCPU / 7.76 GiB KVM guest (QEMU Q35, SeaBIOS, `detectedIsolation: vm`) on an
  **AMD Ryzen 9 9950X**, egress OVH Lille. So 27.67 is a sandbox VM on a dedicated
  desktop-class CPU. boat.dev's own Hetzner row, on a shared EPYC-Rome, scores 8.88.
- The Vercel Sandbox row is on boat.dev's page but not in the linked leaderboard.

### Workload (a), the closest reproducible version

- The same PTS profile, `install.sh` from `phoronix-test-suite/test-profiles@1b05a0e`
  (sha256 pinned), and the same tarball (sha256 pinned). We run the profile's own steps
  without the PTS PHP runner.
- Node is pinned to **v22.22.3** (an official tarball with its sha256 checked) and is
  the same on both sides. This takes the guest images' Node versions out of the
  comparison (our image ships Node 26). The report also prints each image's own Node.
- 3 passes per VM (the profile's `TimesToRun`) and N fresh VMs (default 5). We report
  the median and IQR over all passes. PTS DynamicRunCount is not used.

### The other workloads (our definitions; boat.dev publishes nothing for these)

| | What | How |
|---|---|---|
| b | lifecycle | create→exec-ready, fork→exec-ready, resume→exec-ready, new-from-snapshot→exec-ready: time from the API call until `true` exits 0 (polled every 0.1 s, the same loop on both sides). stop: until the provider reports it stopped (PandaOS: synchronous call; boat.dev: `state == archived`). snapshot: until it can be used (PandaOS: synchronous; boat.dev saved snapshot `status == ready`). exec RTT: p50/p95 of 50 sequential `true` calls; HTTP 429 calls are retried and not counted |
| c | 10 concurrent creates | wall time from one shared start until all 10 answer `true` |
| d | disk | fio (apt-installed if missing; dd fallback, flagged in the report) in `$HOME`, O_DIRECT, libaio, 1 job. Ours: 1M seq write/read MB/s, 4k rand write/read IOPS, iodepth 32, 15 s, 2 GiB file. `pts_*`: the job file of [`pts/fio-2.1.0`](https://github.com/phoronix-test-suite/test-profiles/blob/d2f1a150d388bd062737b445891edda0780f7e25/pts/fio-2.1.0/install.sh), which boat.dev's numbers used: 4k rand read/write, 1M seq write, iodepth 64, 1 GiB, 20 s start delay, 5 s ramp, 60 s. The published comparison uses these. A loop file on a shared runner still cannot speak for NVMe |
| e | network | curl download of two fixed files on two CDNs: the Go 1.25.0 linux-amd64 tarball (59.7 MB, dl.google.com) and the pinned Node tarball (nodejs.org) in Mbit/s; error pages (<1 MB) count as failures, not samples. speed.cloudflare.com was dropped because it answers curl with 403; TCP connect time (connect minus DNS) to 1.1.1.1 and github.com, median of 10 |
| f | cold npm install | `npm ci --ignore-scripts` of `npm-project/` (committed lockfile, 538 packages: express, react, eslint, jest, typescript, webpack), empty cache, pinned Node's npm |

Sizes: PandaOS `default` = 4 vCPU / 8192 MiB / 51 GiB (boatd's default). boat.dev
`default` = 4 vCPU / 8 GB / 51 GB (docs.boat.dev/pricing.md). Each repeat records what
the VM reports (`nproc`, MemTotal, disk, CPU model, kernel, virt, Cloudflare egress
colo). The report flags any VM that is not 4 vCPU / ~8 GiB.

Fairness: both adapters (`drivers.py`) are thin urllib clients with no SDK or CLI on
either side. Guest scripts (`workloads.py`) are the same bytes, run with `bash -lc` as
each provider's default user. Lifecycle times include the API round trip from the
harness host, so run `run-on-server.sh` on the server, or close to it, for a fair
WAN picture.

## boatd options and the warm pool

The `boatd_options` input (or `BOATBENCH_BOATD_OPTIONS` for `stack.sh`) is JSON merged over
the bench `boatd.json`, and the report prints it. With `{"warmPool":true}`, `stack.sh`
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

## Cost

`prices.json` belongs to the cost comparison (`docs/cost.md`):
`{"pandaos": {"usd_per_hour": X, "source": "..."}, "boatdev": {"usd_per_hour": 0.036, "source": "..."}}`.
Cost per 1000 Node loop runs = usd_per_hour / 3600 × 1000 / runs_s. Cost per cold
npm ci = usd_per_hour / 3600 × seconds. If the file is missing, the report shows
runs/s only.

## Caveats (also printed in every report)

The GitHub runner is not a dedicated server. It has 4 shared vCPUs, KVM inside a cloud
VM, and runs in a US region. boat.dev runs in the EU. On the runner, the 10-VM test
oversubscribes the host (stack.sh raises boatd's overcommit only so that test fits).
The lifecycle verbs also mean different things: our stop/fork keep guest memory, while
boat.dev restores the disk in the background after `ready`. Treat the results as
indicative until someone runs `run-on-server.sh` on the owner's server.
