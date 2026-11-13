# AX42 re-measured at main `0d160dd` (PandaOS install, trusted mode), 2026-11-13

Same box as [`../ax42-pandaos-2026-11-12`](../ax42-pandaos-2026-11-12/README.md) and
[`../ax42-diag-2026-11-12`](../ax42-diag-2026-11-12/README.md): Hetzner AX42, Ryzen 7 PRO 8700GE, 64 GB, boat's store
btrfs (`compress=zstd:1,discard=async`) on one NVMe. Host IP, domain and the owner team's ID were removed
(`t_team_OWNER`).

- **Update (02:36–02:39Z):** `/etc/pandaos` backed up to `/root/pandaos-backup-r11-20261113T023554Z`;
  `services.boat.untrustedTenants = false;` added to `local.nix` (the server profile's default is now `true`);
  `nix flake update` moved the `pandaos` input from `3d00774` to `0d160dd` (nixpkgs unchanged, `718f670`);
  `nixos-rebuild build` showed the same kernel, initrd and kernel params, and unchanged nftables / sshd / networkd
  units; then `switch`, `systemctl restart boatd boat-gateway`. No reboot. System generation
  `gy3xg0a9…-nixos-system-pandaos-26.05.20261026.718f670`, boatd `bfhvpbvg…-boatd-0.1.0`.
- **Guest image:** `components/boat/guest/build.sh kernel` + `rootfs slim` at `0d160dd`, built on the box with a
  temporary dockerd (`--iptables=false --bridge=none`, containers on the host network; stopped and its data removed
  afterwards), imported with `boat-admin images import --id ubuntu-24.04`. sha256: vmlinux `5001bc1b…`,
  rootfs-slim.ext4.zst `aea5f51b…`. `ubuntu-full` was not rebuilt. boatd converted the import to NOCOW at its next
  start (`run.log`: `extents 18932 -> 5`; the same journal holds the old image's `22567 -> 4` at 02:39Z).
- **boatd config:** as installed at `0d160dd` with trusted tenants: `Unsafe` drive cache, `auto` (Async) engine,
  qgroups on, and **`warmPool` on**. The module turns the warm pool on when `untrustedTenants` is false; the
  2026-11-12 install run had it off. So `lifecycle-b/` creates are warm, and `lifecycle-b-cold/` repeats b with
  `warmPool: false` for a like-for-like comparison. Other configs ran through a runtime-only systemd drop-in
  (`/run/systemd/system/boatd.service.d`, removed at the end; `boatd_with` in the scripts): `diskCache: Writeback`
  and `diskEngine: Sync`, **each with `warmPool: false`** (`run-r11-nowarm.sh`). Their first attempt with the warm
  pool on (`run-r11.sh`, `run-r11-flush.sh`) restored a template booted with the installed `Unsafe`/Async drive: a
  boatd bug, fixed after `0d160dd` (the template's stamp did not cover the drive config). Those results are in
  `aborted/warm-template/`, kept as the evidence (1 NVMe flush in 20 s of guest fsyncs under "Writeback").
- **CPU governor:** `performance` (as found; not changed). The 2026-11-12 diagnostic run had the same; the
  benchmark run used `powersave`.
- **Harness:** `bench/boatbench` at `0d160dd` plus `API_RTT_GAP_S` (this commit): boat-api limits a key to 10
  requests/s (burst 40), and the 25 back-to-back pings of the API baseline got HTTP 429 in the first attempt
  (`aborted/run.log`). With 0.15 s between pings the baseline's p50 is 1.25 ms, against 0.26–0.28 ms back to back
  (the CPU idles between pings). It only enters the `_net` values (at most 2 × 1 ms on a lifecycle metric).
  `fc_cache_type` reads "unknown (boatd source not found)" where the harness copy had no boatd source; it was
  `Unsafe`, boatd's default, except in `disk-d-writeback/`.
- **boat.dev:** not measured.

## Commands

`run-r11.sh` (02:50–03:38Z) and `run-r11-cold.sh` (03:41–03:42Z), with `SRC=0d160dd8…`, from a copy of
`bench/boatbench` at `/tmp/bbr11` (removed afterwards):

```sh
python3 ab.py fixes OUT/fixes                                    # image extents + FICLONE, snapshot mem extents, guest boot chain
boatbench.py run --driver pandaos --repeats 3 --only b           # lifecycle-b/
boatbench.py run --driver pandaos --repeats 3 --only d           # disk-d/
python3 ab.py seqw OUT/seqw unsafe-async                         # then Writeback, then the Sync engine:
BOATBENCH_PRESET=trusted-writeback boatbench.py run --driver pandaos --repeats 3 --only d   # disk-d-writeback/
python3 ab.py irq OUT/irq                                        # unpinned rand read, IRQ as booted vs off vCPU 0
BOATBENCH_BOATD_OPTIONS='{"warmPool":false}' boatbench.py run --driver pandaos --repeats 3 --only b  # lifecycle-b-cold/
# run-r11-flush.sh, then run-r11-nowarm.sh (03:48–04:13Z), warm pool off for every changed config:
python3 ab.py flush OUT/flush unsafe-cold; python3 ab.py seqw OUT/seqw unsafe-async-cold
# diskCache Writeback: flush, workload d x3 (disk-d-writeback/), seqw; then diskEngine Sync: seqw
```

`pre-update/` ran at 02:21–02:35Z on the old generation (boatd from `3d00774`, the 2026-11-11 image) with the same
script under its first name (`seqw_ab.py`; same jobs): `knobs` (guest block-queue settings, round-robin, 3 × each)
and `irq`.

## Files

| Path | What |
|---|---|
| `lifecycle-b/`, `lifecycle-b-cold/` | workload b, 3 repeats each, warm pool on / off. `report.md`, `report-cold.md` |
| `disk-d/`, `disk-d-writeback/` | workload d, 3 repeats each: `Unsafe` (warm pool on, as installed) / `Writeback` (warm pool off). `report.md`, `report-writeback.md` |
| `seqw/` | 20 s PTS-style 1M seq write (iodepth 64), 3 × each: `unsafe-async` (warm), and cold-booted `unsafe-async-cold`, `writeback-async-cold`, `unsafe-sync-cold` |
| `flush/` | nvme0n1's flush counter (`/sys/block/nvme0n1/stat` field 16) around a 20 s fsync-per-write 4k job: `Unsafe` 1 flush, `Writeback` 3,361 flushes at 84 IOPS |
| `irq/`, `pre-update/irq-pre/` | 8 × alternating unpinned 4k rand reads, virtio-blk IRQ as booted (`0-3`) vs `1-3` |
| `pre-update/knobs-pre/` | seq write under scheduler none / mq-deadline, `max_sectors_kb` = hw max, `nr_requests` 1024 |
| `fixes/log.txt` | after the post-install fixes: image extents and FICLONE times, snapshot `mem` extents and boatd's journal lines, the guest's `/proc/cmdline`, `systemd-analyze`, critical chain, `ldconfig.service` condition |
| `aborted/` | the first attempt (02:45Z): 429s in b, and the image not yet converted (`fixes-unconverted-image/`: 18,915 CoW extents); `warm-template/`: the changed-config runs that got the template's `Unsafe` drive. Not used for numbers |
| `run.log` | both scripts' logs, including boatd's image-conversion journal lines |

`sh ../regen.sh` regenerates the three reports (`report.md`, `report-cold.md`, `report-writeback.md`) byte for byte. Every VM and snapshot was deleted (`run.log` ends with
empty `/v1/vms` and `/v1/snapshots` and no drop-in).
