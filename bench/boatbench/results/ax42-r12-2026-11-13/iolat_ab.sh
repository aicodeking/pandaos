#!/usr/bin/env bash
# r12 on the AX42: does blk-iolatency keep a dump's writeback from starving another VM's reads? No boatd, no VM:
# two scratch cgroups under /sys/fs/cgroup/bb-r12 on the store's NVMe. "w" writes 8 GiB buffered with an fsync at the
# end (a memory dump and the merge's fsync; NOCOMP like boatd's dirs; the memory controller is on, so its writeback
# is charged to "w" as a VM's is to the VM), "r" starts 1 s later and does 4k O_DIRECT
# random reads at iodepth 1 for 15 s (a booting guest's reads). Variants, round-robin 3 x: alone (r only), none (no
# io.latency), iolat (r target=10000 us, w no target: what boatd writes for a running VM and a dumping one).
# Run as: sudo nix shell nixpkgs#fio nixpkgs#e2fsprogs nixpkgs#jq -c bash iolat_ab.sh OUT    (everything is removed)
set -euo pipefail
OUT=${1:?OUT}; mkdir -p "$OUT"
S=/var/lib/boat/bb-r12-scratch CG=/sys/fs/cgroup/bb-r12
DEV=$(lsblk -dno MAJ:MIN "$(findmnt -no SOURCE /var/lib/boat | sed 's/\[.*//')" | tr -d ' ')
cleanup() { rm -rf "$S"; for c in w r; do rmdir "$CG/$c" 2>/dev/null || true; done; rmdir "$CG" 2>/dev/null || true
  echo "cleanup: scratch $(test -e $S && echo LEFT || echo removed), cgroup $(test -e $CG && echo LEFT || echo removed)"; }
trap cleanup EXIT
mkdir -p "$S" && chattr +m "$S"
mkdir -p "$CG" && echo "+memory +io" >"$CG/cgroup.subtree_control" && mkdir -p "$CG/w" "$CG/r"
log() { echo "$(date -u +%H:%M:%SZ) $*" | tee -a "$OUT/log.txt"; }
log "dev $DEV $(cat /sys/block/nvme0n1/queue/scheduler) wbt_lat_usec=$(cat /sys/block/nvme0n1/queue/wbt_lat_usec) $(uname -r) $(grep -E '^(Dirty|Writeback):' /proc/meminfo | tr -s ' ' | tr '\n' ' ')"
fio --name=prep --filename="$S/r" --size=4G --rw=write --bs=1M --direct=1 --ioengine=libaio --iodepth=16 >/dev/null
in_cg() { local c=$1; shift; sh -c 'echo $$ >"$0/cgroup.procs"; exec "$@"' "$CG/$c" "$@"; }
for t in 1 2 3; do
  for v in alone none iolat; do
    if [ "$v" = iolat ]; then echo "$DEV target=10000" >"$CG/r/io.latency"; else echo "$DEV target=max" >"$CG/r/io.latency"; fi
    rm -f "$S/w"; sync
    wpid=""
    if [ "$v" != alone ]; then
      in_cg w fio --name=w --filename="$S/w" --size=8G --rw=write --bs=1M --ioengine=psync --end_fsync=1 \
        --output-format=json --output="$OUT/t$t-$v-writer.json" & wpid=$!
      sleep 1
    fi
    in_cg r fio --name=r --filename="$S/r" --size=4G --rw=randread --bs=4k --direct=1 --ioengine=psync --runtime=15 \
      --time_based --output-format=json --output="$OUT/t$t-$v-reader.json"
    [ -z "$wpid" ] || wait "$wpid"
    r=$(jq -r '.jobs[0].read | "\(.iops|floor) IOPS, clat mean \(.clat_ns.mean/1000|floor) us, p99 \(.clat_ns.percentile["99.000000"]/1000|floor) us, max \(.clat_ns.max/1000|floor) us"' "$OUT/t$t-$v-reader.json")
    w=$([ -z "$wpid" ] && echo - || jq -r '.jobs[0] | "\(.write.io_bytes/1048576|floor) MiB in \(.job_runtime/1000) s incl. fsync"' "$OUT/t$t-$v-writer.json")
    log "t$t $v: reader $r | writer $w | r io.latency: $(cat "$CG/r/io.latency" | tr '\n' ' ')"
  done
done
echo "$DEV target=max" >"$CG/r/io.latency"
