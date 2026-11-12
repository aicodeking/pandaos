# Host disk baseline and VM disk diagnostics: the PandaOS install on the Hetzner AX42, 2026-11-12

Same box and install as [`../ax42-pandaos-2026-11-12`](../ax42-pandaos-2026-11-12/README.md): PandaOS 26.05, kernel
6.18.57, boat's store is btrfs (`compress=zstd:1,discard=async`) directly on nvme0n1 (Samsung MZVL2512HCJQ, no RAID),
boatd defaults (`Unsafe` drive cache, io_uring `Async` engine, NOCOW VM disks). Run 12:45–14:17Z by `fio_ab.py`
from a scratch copy of `bench/boatbench` at `/tmp/bbfio`, with fio 3.42 from `nix shell nixpkgs#fio` on the host and
fio 3.36 from apt in the guest. The box's source commit is not recorded (`nixos-version --configuration-revision`:
unknown); the system generation was the one the 06:28Z reruns used (no switch since).

**Differences from the benchmark run.** The CPU governor was `performance` (EPP `performance`) on all CPUs when this
started. The benchmark run used `powersave` / `balance_performance`. It was found that way and was not changed.
The box was otherwise idle (no other VMs; `GET /v1/vms` empty before and after).

**Nothing was left behind.** The host scratch directory `/var/lib/boat/bb-fio-scratch` (on the same btrfs as
boatd's `stateDir`, at most 2 GiB used) was deleted by the script (`scratch removed: True` in `out-host/log.txt`,
`out-life/log.txt`). Every VM was named `bb-fio-*` and deleted; each log ends with `GET /v1/vms` returning `[]`.
The one snapshot (`out-life`) was deleted (`GET /v1/snapshots` empty). `/tmp/bbfio` was removed from the server.
No NixOS config, service, network or SSH setting was changed. `out-vm` and `wb` dropped the host page cache
(`sync; echo 3 > /proc/sys/vm/drop_caches`) and ran `sync`: runtime actions with no lasting state.

## Commands

```sh
# on the server, from /tmp/bbfio (a copy of bench/boatbench plus fio_ab.py)
nix shell nixpkgs#fio nixpkgs#python3 nixpkgs#e2fsprogs -c python3 fio_ab.py host out-host
export BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key)
nix shell nixpkgs#python3 nixpkgs#e2fsprogs -c python3 fio_ab.py vm out-vm
nix shell nixpkgs#python3 -c python3 fio_ab.py wb out-wb
nix shell nixpkgs#python3 nixpkgs#e2fsprogs nixpkgs#compsize -c python3 fio_ab.py life out-life
nix shell nixpkgs#python3 nixpkgs#e2fsprogs -c python3 fio_ab.py wb2 out-wb2
nix shell nixpkgs#python3 -c python3 fio_ab.py wb3 out-wb3
nix shell nixpkgs#python3 -c python3 fio_ab.py irq out-irq
# lifecycle under the governor as found (b only, 3 fresh VMs):
BOATBENCH_PRESET=pandaos-install nix shell nixpkgs#python3 -c python3 boatbench.py run --driver pandaos --repeats 3 --only b --out out
```

`fio_ab.py` is the final version; the modes were added one after another as the results came in, and each run
used the version that existed then (same jobs). The fio jobs are workloads.py's: `COMMON` = `--direct=1
--ioengine=libaio --numjobs=1 --time_based --group_reporting`, the PTS job = `--size=1g --iodepth=64 --startdelay=20
--ramp_time=5 --runtime=60` (pts/fio-2.1.0, what boat.dev published), variants 20 s.

## Files

| Directory | What |
|---|---|
| `out-host/` | host, no VM: PTS seq write 1M then 3 × PTS rand read 4k, on a NOCOW file and on a CoW file (the store's default, so zstd-compressed); then rand read qd1, io_uring qd64, and a buffered read |
| `out-vm/` | one VM: 3 × (PTS seq write, PTS rand read, host `sync` + drop caches, PTS rand read again); rand read qd1 and io_uring qd64; then workloads.DISK (the benchmark's whole d sequence) twice. Its disk-file lookup used a wrong path, so its "resident" fields are empty |
| `out-wb/` | one VM: 6 × (PTS seq write, then PTS rand read), a host `sync` before the read in even trials; host Dirty / Writeback every 1 s |
| `out-wb2/` | one VM: 4 × the same without `sync`, recording nvme0n1 read I/Os during the read, the VM cgroup's memory.stat and the disk file's page-cache residency |
| `out-wb3/` | one VM: 6 × the same without `sync`, sampling the logical CPU and CPU time of every Firecracker thread every 2 s |
| `out-irq/` | one VM: fio pinned (`taskset`) to the vCPU that takes virtio-blk's interrupt, then to another vCPU, 3 × each, 20 s reads. **This settles the 56k / 220k split: 59.5–59.9k vs 219.1–219.8k** |
| `lifecycle-b-performance/` | boatbench workload b, 3 repeats, under the `performance` governor (`source_commit`: unknown, as the install records none) |
| `out-life/` | image and snapshot file layout (lsattr, filefrag, compsize), one reflink copy of each image, a create / snapshot / new-from-snapshot with boatd's journal lines |

Each directory's `log.txt` is the run's log; every fio run's full JSON output is next to it.
