# boatbench raw results: the PandaOS install on the Hetzner AX42, 2026-11-12

This is the product as shipped. PandaOS is installed on the box, and boat's store is native btrfs on the disk. It is
the same box as [`../ax42-ubuntu-2026-11-11`](../ax42-ubuntu-2026-11-11/README.md), reinstalled. The IP address,
hostname, API domain and IPv6 prefix were removed. The owner key was read from a file on the server and never
printed.

- **Hardware:** Hetzner AX42. AMD Ryzen 7 PRO 8700GE (Zen 4, 8 cores / 16 threads, 35 W), 64 GB (`MemTotal`
  63,383 MiB), 2 × Samsung MZVL2512HCJQ 512 GB NVMe.
- **OS:** PandaOS 26.05 (Yarara), NixOS, kernel 6.18.57. The system generation was
  `/nix/store/q5c6fb4a…-nixos-system-pandaos-26.05.20261026.718f670` until 05:39Z. At 05:39Z another worker switched to
  `drrwpj74…` (same version string). That switch restarted boat-api and boat-images but not boatd. boatd is
  `/nix/store/fx067krq…-boatd-0.1.0`, with Firecracker 1.17.0. The install records no source commit
  (`nixos-version --configuration-revision`: unknown).
- **Store:** `/var/lib/boat` is btrfs (`compress=zstd:1,discard=async`) directly on **one** NVMe (nvme0n1, no RAID). The
  OS is on the other NVMe.
- **boatd config:** `/etc/boat/boatd.json` as installed. It sets no `diskCache`, `memOvercommit`, `cpuOvercommit`,
  `warmPool` or `untrustedTenants`, so boatd's defaults apply (trusted mode, `memOvercommit` 1.5, which admits 11
  VMs by RAM). The owner team's quota (`maxVCPU` 32, `maxMemMiB` 65536) allows **8** `default` VMs. That was the
  admission limit for this key, so workload c ran 8 concurrent creates (`BOATBENCH_CONCURRENT_N=8`), and density
  requested 11 VMs and got 8 (the API refused the other 3 with HTTP 429 quota_exceeded). The report's cache type
  ("Unsafe, boatd default") is read from boatd's source at main. The installed build's default was not checked
  separately.
- **CPU governor:** amd-pstate active, `powersave` with EPP `balance_performance` (the box's setting). In
  `performance/` the governor was `performance` (EPP `performance`) for the A/B test. It was reverted on all 16
  policies afterwards (`logs/run.log`: "reverted").
- **Harness:** run on the box against `http://127.0.0.1:8740`, with no WAN. The code is `bench/boatbench` at main
  `8e9ac7a`. Its run path was uploaded a few minutes before that commit and is identical. The host driver ran the same
  pinned Node binary through the glibc loader (`BB_NODE_LOADER`), because NixOS has no FHS loader. There is no fio on
  the host, so there is no host disk baseline in this run.
- **boat.dev:** not measured. There was no API key.

## Commands

The exact scripts are `run-all.sh` (04:51–06:28Z) and `run2.sh` (06:28–06:54Z). `thin.py` measured a stopped VM's
disk use and wrote `thin.json`. In short:

```sh
export BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) BOATBENCH_CONCURRENT_N=8 BOATBENCH_PRESET=pandaos-install
for i in 1 2 3; do boatbench.py run --driver host --repeats 1 --only a; boatbench.py run --driver pandaos --repeats 1 --only ad; done
boatbench.py run --driver pandaos --repeats 3 --only bcef                                   # rerun, run2.sh
BOATBENCH_DENSITY_LEVELS=1,2,4,7,8,11 boatbench.py run --driver pandaos --repeats 2 --only g # rerun, run2.sh
BOATBENCH_DENSITY_LEVELS=1,2,4,7,8,11 boatbench.py run --driver host --repeats 2 --only g    # host-native baseline
# governor performance:
boatbench.py run --driver host --repeats 1 --only a; boatbench.py run --driver pandaos --repeats 1 --only a
BOATBENCH_DENSITY_LEVELS=1,4,8 boatbench.py run --driver pandaos --repeats 1 --only g
BOATBENCH_DENSITY_LEVELS=1,4,8,11 boatbench.py run --driver host --repeats 1 --only g
```

## Files and run IDs

| Directory | File (run ID) | What |
|---|---|---|
| `powersave/` | `results-host-…T045155Z` (25e7d9), `…T050337Z` (f6a03c), `…T051515Z` (1056a7) | host Node loop (a), 3 passes each |
| `powersave/` | `results-pandaos-…T045439Z` (dc7c96), `…T050622Z` (cb2137), `…T051800Z` (ca2db2) | VM a + d, alternating with the host |
| `powersave/` | `results-pandaos-…T062855Z` (595790) | b, c (8), e, f, 3 repeats |
| `powersave/` | `results-pandaos-…T063131Z` (849e85) | g, VMs at N = 1, 2, 4, 7, 8 (11 refused by quota), 2 repeats |
| `powersave/` | `results-host-…T053936Z` (28a141) | g on the host, N = 1, 2, 4, 7, 8, 11 Node loops, 2 repeats |
| `performance/` | 2af7f9, 16dd93, 945166, 555bc1 | governor A/B: host a, VM a, VM g (1, 4, 8), host g (1, 4, 8, 11) |
| `contaminated/` | 52f3a9 (bcef), 3ccb41 (g) | **Do not use.** See below |
| `logs/` | `run.log`, `run2.log`, `builder-cpu.log` | harness logs; CPU use of the other VM |
| | `report-powersave.md`, `report-performance.md` | `boatbench.py report <dir>/results-*.json --server ax42` |

**Why two runs are in `contaminated/`.** The server is shared. At 05:26Z another worker started an `xlarge` VM
(16 vCPU / 32 GiB) in the same team. From 05:26 to 05:36Z it used about 2.4 CPUs on average (`logs/builder-cpu.log`).
It also took half the team's vCPU quota, so density got only 4 VMs and 4 of the 8 concurrent creates were refused.
At 05:39Z a `nixos-rebuild switch` restarted boat-api in the middle of density repeat 2, which orphaned its 4 VMs.
They were deleted by hand at about 05:48Z. Workloads b, c, e, f and g were rerun from 06:28Z, after the other worker
confirmed that its VMs were gone. The a/d pairs (04:51–05:26Z) ran before any of this. The host density baseline
started at 05:39:36Z: from 05:37:46 on, the other VM used about 0.004 CPU, and it was gone by 05:45:46.

Every VM and snapshot the runs created was deleted (`logs/run.log` ends with `left over: {'vms': []} {'snapshots': []}`;
`/v1/vms` and `/v1/snapshots` were checked empty afterwards). `/tmp/bbr` and `/tmp/bb` were removed from the server.
