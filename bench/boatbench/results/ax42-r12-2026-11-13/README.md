# AX42, r12 (2026-11-13): sequential-write profile, pinned random reads, archived "before" figures

Same box, install and boatd config as [`../ax42-r11-2026-11-13`](../ax42-r11-2026-11-13/README.md) (main `0d160dd`,
trusted mode, `Unsafe` drive cache, Async engine, qgroups on, warm pool on, governor `performance`). Nothing on the
server was reconfigured: no NixOS change, no boatd restart, no drop-in. Every VM was named `bb-r12-*` and deleted;
every scratch file lived in `/var/lib/boat/bb-r12-scratch` and was removed (the logs end with an empty `/v1/vms` and
`scratch removed: yes`). Tools came from `nix shell nixpkgs#python3 nixpkgs#fio nixpkgs#e2fsprogs nixpkgs#jq`
(no package installed on the host). boat.dev: not measured.

## `seqw/`: where a guest's 1M sequential write goes (`seqw_prof.py`)

One VM; the PTS-style seq write (1M, iodepth 64, O_DIRECT, libaio, 30 s) in the guest; from second 8 to 18 the host
sampled the guest's `/sys/block/vda/stat`, the host's `/sys/block/nvme0n1/stat` and the CPU of every thread of the
VM's `firecracker` (`/proc/PID/task/*/stat`). Two rounds of four guest variants. Raw: `seqw/seqw-t*-*.json`,
sequence in `seqw/log.txt`.

| Variant (guest) | fio MiB/s | guest requests/s | guest avg request | guest queue depth | busiest firecracker thread |
|---|---|---|---|---|---|
| as booted (`none`, `max_sectors_kb` 1280, `max_segments` 1) | 1,632 / 1,667 | 62,374 / 69,403 | 26.7 / 24.8 KiB | 121 / 121 | VMM thread `firecracker` 92.1 / 95.0% |
| `max_sectors_kb` = hw max | 1,666 / 1,577 | 70,542 / 98,098 | 24.2 / 16.5 KiB | 121 / 120 | `firecracker` 92.5 / 93.3% |
| `mq-deadline` | 1,630 / 1,566 | 64,242 / 85,843 | 26.1 / 18.6 KiB | 248 / 247 | `firecracker` 88.5 / 91.9% |
| fio buffers in a 2 MiB huge page (`--iomem=mmaphuge`) | 1,914 / 1,904 | 1,910 / 1,914 | 1,024 KiB | 64 / 64 | io_uring worker `iou-wrk` 99.3 / 99.6% (`firecracker` 2.3%) |

Host NOCOW file on the same btrfs, same job, 20 s (`seqw/host-nocow-{1,2,3}.json`): **4,853 / 4,825 / 4,815 MiB/s**.
The guest as booted reaches 0.34× of that.

**Measured cause.** `max_segments` is 1 (Firecracker's virtio-blk offers no `VIRTIO_BLK_F_SEG_MAX`), so each guest
request is one physically contiguous run of fio's buffer: 17–27 KiB on average, not 1 MiB. The guest keeps its queue
full (121 of 128 requests in flight), and the one VMM thread that serves virtio-blk runs at 88–95% of a CPU on
62–98k requests/s. That thread is the ceiling. The host block layer is not: during the guest's write the NVMe saw
1.1–3.3k requests/s of 119–127 KiB at a queue depth of 0.6–6.8 (`Unsafe` writes land in the host page cache and are
flushed later). Physically contiguous buffers prove it: with 1 MiB requests the VMM thread drops to 2.3%, throughput
rises 15–17%, and the next ceiling is the single io_uring worker copying the data into the host page cache (99.5%).
No guest queue knob changes the request size (`max_sectors_kb`, `mq-deadline`: within run-to-run spread, as in r11's
`pre-update/knobs-pre/`), so **nothing shipped**. Huge-page buffers are a property of the guest workload, not a
setting boatd or the image can give every program. A real fix needs Firecracker to offer `SEG_MAX` (or more VMM I/O
threads).

`seqw/log.txt` also has a first host line, `host NOCOW fio (same job, 20 s): 743.3 MiB/s`: that file was **not**
NOCOW (`chattr` was not on the PATH, so the scratch dir kept CoW + zstd; it matches r11's 746 MiB/s CoW figure). Its
JSON was dropped; the three `host-nocow-*.json` files above ran with `chattr` from `nixpkgs#e2fsprogs` and
`lsattr` showed `C`.

## `disk-d-pinned/`: workload d with the pinned random reads

`boatbench.py run --driver pandaos --repeats 3 --only d` (08:41–09:01Z), `BOATBENCH_PRESET=pandaos-install`,
`BOATBENCH_RUN_ID=r12` (VMs `bb-r12-0` … `bb-r12-2`, warm), from `bench/boatbench` at local commit `a8d1aba` (the
`source_commit` in the JSON; the same tree is in this commit's history under its pushed SHA). `run.log` is the
harness log. `sh ../regen.sh` writes `report-disk-pinned.md` byte for byte.

| 4k rand read, iodepth 32, 15 s | repeat 1 | repeat 2 | repeat 3 |
|---|---|---|---|
| fio unpinned | 217,575 | 215,100 | 219,617 |
| pinned to the virtio-blk IRQ vCPU (vCPU 3 each time) | 58,891 | 59,341 | 59,090 |
| pinned to the vCPU with the fewest of those interrupts | 216,963 | 217,216 | 216,919 |
| PTS job (iodepth 64, 60 s), unpinned | 218,278 | 59,484 | 59,143 |

The pinned rows agree within 1% across repeats; the unpinned PTS job landed on the interrupt vCPU 2 times of 3, so
its median (59,484) is decided by chance, which is why both pinned rows are now reported next to it.

## `before/`: docs/cost.md "before" figures that were observed, not archived (`before_repro.py`)

**`before/irq/` reproduced and archived.** One VM, 4k rand read with fio pinned to each vCPU in turn, 3 rounds of:
the interrupt as found, the interrupt moved (`smp_affinity_list` 1), and `rq_affinity` 2 (`before/log.txt`):

| | fio on the IRQ vCPU | fio on the other 3 vCPUs |
|---|---|---|
| as found | 58,578 (IRQ on vCPU 3) / 58,916 / 58,782 (IRQ stayed on vCPU 1 after round 1) | 213,015–219,222 |
| interrupt moved to vCPU 1 | 58,419 / 58,846 / 57,704 | 213,160–217,623 |
| `rq_affinity` 2 | 58,775 / 58,733 / (vCPU 1) | 171,817–174,692 |

As observed on 2026-11-12: the slow vCPU moves with the interrupt (all of the interrupts of a round land on one vCPU,
`interrupts per vCPU` lines), and `rq_affinity` 2 lowers the other vCPUs from ~216k to ~173k. Restoring
`smp_affinity_list` to `0-3` leaves an edge-triggered IO-APIC interrupt on the vCPU it was last moved to.

**`before/` image layout: not reproduced.** `cp --sparse=always` of today's `ubuntu-24.04` rootfs into a NOCOW scratch
dir gave 5 extents and 3.0 GiB allocated, not the 22,567 extents of the 2026-11-12 image (that image was replaced
when the fixed boatd converted it), and FICLONE of it took 2.8–3.2 ms like the installed image's 2.5–3.0 ms. The
151–170 ms FICLONE figure therefore stays "observed, not archived".

## `iolat/`: does a dump's writeback starve another VM's reads on this NVMe? (`iolat_ab.sh`)

No boatd and no VM: two scratch cgroups (`/sys/fs/cgroup/bb-r12/{w,r}`, memory + io controllers, removed at the end).
`w` writes 8 GiB buffered with an fsync at the end (a memory dump and the merge's fsync), `r` starts 1 s later and
does 4k O_DIRECT random reads at iodepth 1 for 15 s. 3 rounds, round-robin:

| | reader IOPS | clat mean | clat p99 | writer: 8 GiB incl. fsync |
|---|---|---|---|---|
| reader alone | 17,541–17,607 | 56 µs | 62–67 µs | – |
| with the writer | 15,931–15,971 | 62 µs | 250–252 µs | 7.54–7.65 s |
| with the writer, reader `io.latency` target 10 ms (blk-iolatency, `CONFIG_BLK_CGROUP_IOLATENCY=y` here) | 15,892–15,952 | 62 µs | 252–254 µs | 7.55–7.58 s |

The NVMe absorbs a 1.1 GB/s writeback with the reader's mean latency up 6 µs and its p99 at a quarter of a
millisecond, so a dump has nothing to starve here, and an `io.latency` target never trips. The CI runner's kernel
(`7.0.0-1019-azure`) is built without `CONFIG_BLK_CGROUP_IOLATENCY`, so it cannot help there either: boatd does not
set it (tried in r12, measured as no-op on both, removed). The cold-create-during-dump slow mode is a property of the
CI runner's loop file on a throttled cloud disk; see `components/boat/boatd/e2e-perf.sh` for its numbers.
