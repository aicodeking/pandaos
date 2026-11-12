# boatbench raw results: Hetzner AX42, stock Ubuntu, 2026-11-11

The raw `results-*.json` files behind the "Measured on a real dedicated server (Hetzner AX42)" section of
[`docs/cost.md`](../../../../docs/cost.md). The IP address and hostname were removed. There were none in these files: the
only addresses are 10.200.x guest addresses, 127.0.0.1 and 1.1.1.1.

- **Hardware:** Hetzner AX42. AMD Ryzen 7 PRO 8700GE (Zen 4, 8 cores / 16 threads, 35 W), 64 GB DDR5 ECC
  (`MemTotal` 63,381 MiB), 2 × 512 GB NVMe in mdadm RAID1 (`hw.txt` in each preset directory).
- **OS:** Ubuntu 24.04.5 LTS, kernel 6.8.0-146-generic, `/dev/kvm`. This was not PandaOS. boat's store was a
  sparse btrfs **loop file** on the ext4 root (on the RAID1), as `stack.sh` sets it up.
- **Code:** boatd, boat-api and the harness came from `main` at `bcf0cda`, plus that day's `stack.sh` changes
  (`BENCH_STATE`, and the stale-socket fix from the restart onward). The slim guest image was built on the box with
  `components/boat/guest/build.sh`.
- **Harness host:** the same box, talking to boat-api on 127.0.0.1. Nothing was exposed publicly.
- **boat.dev:** not measured. There was no API key.

## Commands

For each preset ($PRESET = trusted, untrusted, trusted-warm, trusted-writeback), `stack.sh up` ran before the commands
and `stack.sh down` ran after them:

```sh
git clone https://github.com/AICodeKing/pandaos.git /opt/boatbench/pandaos            # bcf0cda
OUT=/opt/boatbench/guest-out COMPRESS=0 components/boat/guest/build.sh kernel
OUT=/opt/boatbench/guest-out COMPRESS=0 components/boat/guest/build.sh rootfs slim
export BENCH_WORK=/opt/boatbench/work BENCH_STATE=/opt/boatbench/state
unset BOATBENCH_BOATD_OPTIONS
BOATBENCH_PRESET=$PRESET bench/boatbench/stack.sh up /opt/boatbench/guest-out
set -a; . $BENCH_WORK/env; set +a      # BOAT_URL, BOAT_KEY
B=bench/boatbench/boatbench.py
# trusted and untrusted: 3 host / VM pairs, then the rest
for i in 1 2 3; do
  BOATBENCH_HOST_DIR=$BENCH_STATE/host-home python3 $B run --driver host --repeats 1 --only ad --out R
  python3 $B run --driver pandaos --repeats 1 --only ad --out R
done
python3 $B run --driver pandaos --repeats 3 --only bcef --out R
BOATBENCH_DENSITY_LEVELS=1,2,4,7,11 python3 $B run --driver pandaos --repeats 2 --only g --out R   # untrusted: 1,2,4,7
# trusted-warm: --repeats 3 --only bc        trusted-writeback: --repeats 3 --only d
```

The untrusted preset ran with SMT off at runtime (`/sys/devices/system/cpu/smt/control`, 8 CPUs online). SMT was
switched back on afterwards. In every preset `memOvercommit` stayed at the bench's 8. That setting only affects
admission (workload c's 10 VMs).

## Files

| Directory | Contents |
|---|---|
| `trusted/`, `untrusted/` | 3 `results-host-*` and 3 `results-pandaos-*` (a, d) pairs, then `bcef` (3 repeats) and `g` (2 repeats); `run.log`, `stack-up.log`, `boatd-tail.log`, `hw.txt`, `smt.txt`, `report.md` |
| `trusted-warm/` | `bc`, 3 repeats |
| `trusted-writeback/` | `d`, 3 repeats |
| `presets.md` | `boatbench.py presets */results-pandaos-*.json` |

A first pass of trusted-warm and trusted-writeback was thrown away because the previous preset's
`BOATBENCH_BOATD_OPTIONS` had leaked into it (its `boatd_options` show the untrusted keys). It is not in this
directory. Two of trusted's three host / VM pairs came from a first session. The stack was restarted before pair 3,
with the same build and the same config.

## Regenerate

```sh
cd bench/boatbench/results/ax42-ubuntu-2026-11-11
python3 ../../boatbench.py presets */results-pandaos-*.json                       # per-preset table
for d in trusted untrusted trusted-warm trusted-writeback; do
  python3 ../../boatbench.py report $d/results-*.json --server ax42 --out $d/report.md
done
```

`sh ../regen.sh` runs exactly this (and the PandaOS-install reports); `git diff` is empty afterwards. These results
predate boatbench's `source_commit` field, so the reports print "not recorded"; the source was main at `bcf0cda`
plus `stack.sh` changes (Code, above).
