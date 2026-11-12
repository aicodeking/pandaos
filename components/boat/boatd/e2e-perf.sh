# shellcheck shell=bash
# shellcheck disable=SC2154 # WORK, STATE, HERE and the helpers come from e2e.sh / e2e-host.sh
# Performance of a 4 vCPU / 8 GiB VM, measured the way bench/boatbench does (its
# fio script is imported, not copied). Sourced by e2e-host.sh with no VMs left.
# Prints `METRIC {...}`; timings past their gate fail the run after everything ran.
#
#   ceiling   fio on the host's state dir: what the guest cannot beat on this runner
#   disk      fio in a VM; extents of its disk; guest scheduler and discard
#   snapshot  named snapshot, new-from-snapshot, fork of a VM after fio and 3 GiB of writes
#   density   idle RSS per default VM with and without the balloon, KSM saving
#   warm      10 concurrent creates, cold and from a warm template
#
# Gates: GitHub's EPYC 7763 runner (4 shared vCPU, loop btrfs, host fio 329-449 MB/s).
# Each is about 2x the worst value (or half the worst, for minimums) in the 11 green
# boat-e2e runs on main 42710565475-43209685030 (2026-11-09..12); a gate already
# tighter than that stays where it is. They catch regressions, not runner noise: rand
# IOPS move by a factor of 5 between runs. Worst green value -> gate:
PERF_SNAPSHOT_MAX=${E2E_PERF_SNAPSHOT_MAX:-18}    # 5.91-9.73 s (3.8 GiB to write) -> 18 (1.85x)
PERF_FROM_MAX=${E2E_PERF_FROM_MAX:-5}             # 0.75-2.48 s -> 5; red runs saw 3.18 (43151531250) and 4.07: the restore reads the mem file behind the dump's 3.8 GiB writeback
PERF_FORK_MAX=${E2E_PERF_FORK_MAX:-3}             # right after that snapshot: 1.10-2.03 s -> 3 (1.5x)
PERF_WARM_TEN_MAX=${E2E_PERF_WARM_TEN_MAX:-13}    # 4.93-9.44 s; 10 x 4 vCPU on 4 host CPUs -> 13 (1.4x)
PERF_RSS_MAX=${E2E_PERF_RSS_MAX:-700}             # idle default VM: 527-530 MiB -> 700
PERF_WARM_RSS_MAX=${E2E_PERF_WARM_RSS_MAX:-450}   # idle warm VM: 259-280 MiB -> 450
PERF_FREED_RSS_MAX=${E2E_PERF_FREED_RSS_MAX:-1000} # 3 GiB used, freed, 20 s later: 529-536 MiB -> 1000
PERF_SEQ_WRITE_MIN=${E2E_PERF_SEQ_WRITE_MIN:-250}  # MB/s: 530-911 -> 250
PERF_RAND_READ_MIN=${E2E_PERF_RAND_READ_MIN:-15000} # IOPS: 25k-171k (all Async) -> 15000 (0.6x)
PERF_RAND_WRITE_MIN=${E2E_PERF_RAND_WRITE_MIN:-15000} # IOPS: 34k-58k (all Async) -> 15000
check_min() { # NAME VALUE MIN
  printf 'e2e: %-14s %s (min %s)\n' "$1" "$2" "$3"
  awk -v a="$2" -v b="$3" 'BEGIN { exit !(a >= b) }' || perf_fail+="$1 was $2 < $3; "
}

disk_stuck() { # fio has 5 minutes: what the host was doing when it missed them
  echo "--- blocked tasks"; echo w | sudo tee /proc/sysrq-trigger >/dev/null; sudo dmesg | tail -n 60
  sudo btrfs qgroup show --raw "$STATE" | head -n 20 || true
  fail "$1"
}
perf_cfg() { # JQ_FILTER NAME: the base config without the e2e's caps, plus JQ_FILTER
  jq ".ioMaxMBps = 0 | .egressMbit = 0 | .memOvercommit = 8 | .cpuOvercommit = 12 | .defaultTeam.maxVMs = 16 | $1" \
    "$WORK/boatd.json" >"$WORK/boatd-perf-$2.json"
  restart_boatd "$WORK/boatd-perf-$2.json"
}
perf_vm() { bd POST /v1/vms "$(jq -nc --arg n "$1" '{name: $n, team: "perf", size: "default"}')" | jq -r .id; }
perf_del() { local d; for d in "$@"; do bd DELETE "/v1/vms/$d" >/dev/null; done; }
# Same dirty state on every run: 2 GiB in tmpfs, 1 GiB of guest page cache.
# (A first-touch random writer on a sparse disk is the worst case for extents:
# 337k and a 2.2 s reflink in run 41584046201, not what boatbench's fio does.)
perf_dirty() { bx "$1" 'mkdir -p /dev/shm/d && head -c 2G /dev/urandom >/dev/shm/d/dirty; head -c 1G /dev/urandom >/root/cache; sync; cat /root/cache >/dev/null' 300; }
vm_extents() { sudo filefrag "$STATE/vms/$1/firecracker/$1/root/rootfs.ext4" | awk '{print $2}'; }
rss_mib() { # idle RSS of a VM's firecracker, and its cgroup's memory.current
  local pid; pid=$(vmf "$1" pid)
  echo "$(( $(sudo awk '/^VmRSS:/ {print $2}' "/proc/$pid/status") / 1024 )) $(( $(cat "/sys/fs/cgroup/boat-e2e/$1/memory.current") >> 20 ))"
}
last_snap_log() { sudo grep -E "snapshot |freeze " "$WORK/boatd.log" | tail -n 3 | sed -E 's/^[0-9/]+ [0-9:]+ //' | tr '\n' ' '; }
sudo apt-get install -y -qq fio >/dev/null 2>&1 || { sudo apt-get update -qq && sudo apt-get install -y -qq fio >/dev/null; }
# Without boatbench's three PTS jobs: 85 s each, for numbers not gated here, on 3 engines and configs.
(cd "$HERE/../../bench/boatbench" && python3 -c '
import re, workloads
disk, n = re.subn(r",\s*\(\"pts_.*?\)\]:", "]:", workloads.DISK, flags=re.S)
assert n == 1, "boatbench changed its job list"
print(disk)') >"$WORK/tmp/disk.sh"

sudo grep -E "snapshot |freeze |merge " "$WORK/boatd.log" | head -n 30 | sed -E 's/^[0-9/]+ [0-9:]+ //' # e2e-host's timings

# A guest whose ext4 buddy pages are gone (dropped cache, memory pressure) must survive
# allocating blocks, also across a snapshot: linux 6.1.191 oopsed (guest/kernel/fix-mballoc.sh).
log "perf e2e: guest allocates blocks with no page cache, before and after a restore"
perf_cfg '.' rc
rc1=$(newvm '{"name":"rc","team":"rc"}')
bx "$rc1" 'sync; echo 3 >/proc/sys/vm/drop_caches; head -c 1M /dev/urandom >/root/a; sync; rm /root/a; sync; echo 3 >/proc/sys/vm/drop_caches' || fail "guest died allocating blocks with a dropped cache"
rcs=$(bd POST "/v1/vms/$rc1/snapshots" '{}' | jq -r .id)
rc2=$(newvm "{\"name\":\"rd\",\"team\":\"rc\",\"snapshot\":\"$rcs\"}")
[ "$(vmf "$rc2" agent_ready)" = true ] || fail "restored guest did not take its identity"
bx "$rc2" 'head -c 1M /dev/urandom >/root/b; sync; md5sum /usr/bin/ssh-keygen >/dev/null' || fail "restored guest cannot use its disk"
perf_del "$rc2"
bd DELETE "/v1/vms/$rc1/snapshots/$rcs" >/dev/null
perf_del "$rc1"

log "perf e2e: ceiling (fio on the host, NOCOW file in the state dir)"
hostfio() {
  sudo touch "$STATE/fio.tmp" && sudo chattr +C "$STATE/fio.tmp" 2>/dev/null || true
  sudo fio --name=h --filename="$STATE/fio.tmp" --size=2G --direct=1 --ioengine=libaio --iodepth=32 --numjobs=1 \
    --runtime=15 --time_based --group_reporting --output-format=json --rw="$1" --bs="$2" | jq "(.jobs[0].$3.$4)"
}
c_sw=$(hostfio write 1M write bw_bytes)
c_rw=$(hostfio randwrite 4k write iops)
c_rr=$(hostfio randread 4k read iops)
sudo rm -f "$STATE/fio.tmp"
echo "ceiling: seq write $((c_sw / 1000000)) MB/s, rand write ${c_rw%.*} IOPS, rand read ${c_rr%.*} IOPS"

log "perf e2e: disk (fio in the VM, boatbench's settings)"
perf_cfg '.' base
pv=$(perf_vm pv)
guest_blk=$(bx "$pv" 'printf "sched=%s discard_max=%s" "$(cat /sys/block/vda/queue/scheduler)" "$(cat /sys/block/vda/queue/discard_max_bytes)"')
echo "guest: $guest_blk"
# Random reads the host has no cache for: what a queue depth is for. (fio's other numbers read
# back what they just wrote, from the host's page cache, where Sync's one-at-a-time is enough.)
cold_read() {
  bx "$1" 'fio --name=c --filename=/root/c --size=2G --direct=1 --ioengine=libaio --iodepth=32 --rw=write --bs=1M --output-format=json >/dev/null; sync' 200
  sudo sh -c 'sync; echo 1 >/proc/sys/vm/drop_caches'
  bx "$1" 'fio --name=c --filename=/root/c --size=2G --direct=1 --ioengine=libaio --iodepth=32 --rw=randread --bs=4k --runtime=15 --time_based --output-format=json --output=/tmp/c.json; rm /root/c; grep -m1 "\"iops\" " /tmp/c.json | tr -dc "0-9."' 200
}
# disk.sh apt-installs fio in the guest. The guest's archive.ubuntu.com timed out for
# 300 s in runs 42757402652 (both attempts) while the runner's own mirror answered:
# the guest uses the runner's mirror, so the gate does not depend on the public archive.
HOST_MIRROR=$(grep -rhoE 'https?://[a-z0-9.-]+\.ubuntu\.com/ubuntu/?' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null |
  grep -vE '//(archive|security)\.ubuntu\.com' | head -1 | sed 's|/*$||' || true)
put_disk() {
  local t0=$SECONDS
  curl -fsS --unix-socket "$SOCK" -X PUT --data-binary "@$WORK/tmp/disk.sh" "http://boatd/v1/vms/$1/files?path=/tmp/disk.sh&mode=755" >/dev/null
  [ -z "$HOST_MIRROR" ] || bx "$1" "sed -i -e 's|http://archive.ubuntu.com/ubuntu|$HOST_MIRROR|' -e 's|http://security.ubuntu.com/ubuntu|$HOST_MIRROR|' /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list 2>/dev/null; true" >/dev/null
  # Installed here, outside disk.sh's 300 s, so a slow mirror is not a slow disk.
  bx "$1" 'command -v fio >/dev/null || { apt-get update -qq >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1; }; command -v fio' 600 >/dev/null ||
    fail "guest could not install fio from ${HOST_MIRROR:-archive.ubuntu.com} in 600 s"
  echo "guest fio ready in $((SECONDS - t0)) s (${HOST_MIRROR:-archive.ubuntu.com})"
}
put_disk "$pv"
disk=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p') || disk_stuck "fio with quotas on"
echo "disk (Async): $disk"
cold_async=$(cold_read "$pv")
echo "cold rand read (Async): $cold_async IOPS"
echo "extents after boatbench's fio: $(vm_extents "$pv")"

perf_del "$pv"
perf_cfg '.diskEngine = "Sync"' sync
pv=$(perf_vm pv)
put_disk "$pv"
disk_sync=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p') || disk_stuck "fio on the Sync engine"
echo "disk (Sync, same runner): $disk_sync"
cold_sync=$(cold_read "$pv")
echo "cold rand read (Sync): $cold_sync IOPS"
perf_del "$pv"

# untrustedTenants' host setting: io_uring only for CAP_SYS_ADMIN and kernel.io_uring_group,
# the jailed VMMs' shared gid (jailGid). Was io_uring off for all (Sync): the "before" above.
disk_iou=null
if [ "$JAILER" = 1 ] && [ -e /proc/sys/kernel/io_uring_group ]; then
  log "perf e2e: Async engine, io_uring only for the VMMs' gid (untrustedTenants)"
  iou_was="$(cat /proc/sys/kernel/io_uring_disabled) $(cat /proc/sys/kernel/io_uring_group)"
  iou_restore() { local d g; read -r d g <<<"$iou_was"; sudo sysctl -qw kernel.io_uring_disabled="$d" kernel.io_uring_group="$g"; }
  iou_fail() { iou_restore; fail "$1"; } # later steps (e2e-edge) run on this host too
  sudo sysctl -qw kernel.io_uring_disabled=1 kernel.io_uring_group=899999
  # uid nobody with gid GID (setpriv: sudo -g needs a runas-group grant the runner lacks).
  # shellcheck disable=SC2024 # the log is the runner's own file
  iou_fio() { sudo setpriv --reuid=65534 --regid="$1" --clear-groups fio --name=u --filename="/tmp/iou-probe-$1" --size=4M --ioengine=io_uring --rw=write --bs=1M >"$WORK/tmp/iou-$1.log" 2>&1; }
  ! iou_fio 65534 || iou_fail "io_uring open to a process outside kernel.io_uring_group"
  iou_fio 899999 || iou_fail "io_uring refused to kernel.io_uring_group: $(tail -n 5 "$WORK/tmp/iou-899999.log")"
  sudo rm -f /tmp/iou-probe-*
  perf_cfg '.jailGid = 899999 | .diskEngine = "Async"' iou # Async refused would fail the create
  pv=$(perf_vm pv)
  [ "$(sudo awk '/^Gid:/ {print $2}' "/proc/$(vmf "$pv" pid)/status")" = 899999 ] || iou_fail "the VMM does not run with jailGid"
  put_disk "$pv"
  disk_iou=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p') || { iou_restore; disk_stuck "fio on Async with io_uring_group"; }
  echo "disk (Async, io_uring for the VMMs only): $disk_iou"
  echo "untrusted seq write: Sync (before) $(jq -r .seq_write_mbps <<<"$disk_sync") MB/s, Async with io_uring_group (after) $(jq -r .seq_write_mbps <<<"$disk_iou") MB/s"
  perf_del "$pv"
  iou_restore
else
  echo "perf e2e: no jailer or no kernel.io_uring_group (Linux < 6.6): Async with io_uring_group not measured"
fi
perf_cfg '.' again
pv=$(perf_vm pv)

log "perf e2e: snapshot, new-from-snapshot, fork (4/8, 2 GiB dirty + 1 GiB cache)"
perf_snap() { # LABEL: one measurement set on a fresh VM; sets snap_s fromsnap_s fork_s fork2_s
  local v sn f; v=$(perf_vm "s$1")
  put_disk "$v"
  bx "$v" 'bash /tmp/disk.sh' 300 >/dev/null
  perf_dirty "$v"
  echo "$1: extents $(vm_extents "$v"), guest $(bx "$v" 'free -m | awk "/Mem:/ {print \"used=\"\$3,\"cache=\"\$6}"')"
  local t; t=$(now); sn=$(bd POST "/v1/vms/$v/snapshots" '{"name":"x"}' | jq -r .id); snap_s=$(secs "$t" "$(now)")
  echo "$1: snapshot ${snap_s}s: $(last_snap_log)"
  t=$(now); f=$(bd POST /v1/vms "{\"name\":\"n$1\",\"team\":\"perf\",\"snapshot\":\"$sn\"}" | jq -r .id); fromsnap_s=$(secs "$t" "$(now)")
  [ "$(vmf "$f" agent_ready)" = true ] || fail "new-from-snapshot not ready"
  echo "$1: new-from-snapshot ${fromsnap_s}s: $(sudo grep -E "$f: (create|boot|guest)" "$WORK/boatd.log" | sed -E 's/^.*'"$f"': //' | tr '\n' ' ')"
  t=$(now); bx "$f" true; echo "$1: its first exec, with the dump in writeback: $(secs "$t" "$(now)")s" # boatbench counts this
  perf_del "$f"
  t=$(now); f=$(bd POST "/v1/vms/$v/fork" "{\"name\":\"f$1\"}" | jq -r .id); fork_s=$(secs "$t" "$(now)")
  [ "$(vmf "$f" agent_ready)" = true ] || fail "fork not ready"
  [ "$(bx "$f" 'ls -l /dev/shm/d/dirty | wc -l')" = 1 ] || fail "fork lost tmpfs"
  echo "$1: fork ${fork_s}s: $(last_snap_log)"
  perf_del "$f"
  sudo sync # the same fork with the first dump on disk: what a fork costs without a flush to wait for
  t=$(now); f=$(bd POST "/v1/vms/$v/fork" "{\"name\":\"g$1\"}" | jq -r .id); fork2_s=$(secs "$t" "$(now)")
  echo "$1: fork, base clean ${fork2_s}s: $(last_snap_log)"
  perf_del "$f"
  bd DELETE "/v1/vms/$v/snapshots/$sn" >/dev/null
  perf_del "$v"
}
perf_snap p
kvm_snap=$snap_s kvm_from=$fromsnap_s kvm_fork=$fork_s kvm_fork2=$fork2_s
perf_del "$pv"

log "perf e2e: the same with btrfs quotas off: what the default costs"
perf_cfg '.btrfsQgroups = false' noq
sudo btrfs quota disable "$STATE"
pv=$(perf_vm pv)
put_disk "$pv"
disk_noq=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p') || disk_stuck "fio with quotas off"
echo "disk (quotas off): $disk_noq"
perf_del "$pv"
perf_snap q
noq_snap=$snap_s noq_from=$fromsnap_s noq_fork=$fork_s
perf_cfg '.' reQ
echo "quotas on / off: snapshot ${kvm_snap}/${noq_snap}s, new-from-snapshot ${kvm_from}/${noq_from}s, fork ${kvm_fork}/${noq_fork}s"

log "perf e2e: density (idle default VMs)"
perf_idle() { # LABEL COUNT: sum of idle memory after 60 s; sets idle_rss idle_cg (MiB per VM)
  local vs=() v r rss=0 cg=0
  for i in $(seq "$2"); do vs+=("$(perf_vm "i$1$i")"); done
  sleep 60
  for v in "${vs[@]}"; do r=$(rss_mib "$v"); rss=$((rss + ${r% *})); cg=$((cg + ${r#* })); done
  idle_rss=$((rss / $2)) idle_cg=$((cg / $2))
  echo "$1: $2 idle VMs: RSS $idle_rss MiB per VM, cgroup $idle_cg MiB per VM"
  [ -n "${3:-}" ] && sleep "$3"
  idle_vms=("${vs[@]}")
}
perf_cfg '.balloon = false' nob
perf_idle noballoon 4
nb_rss=$idle_rss nb_cg=$idle_cg
perf_del "${idle_vms[@]}"
perf_cfg '.' bal
perf_idle balloon 4
b_rss=$idle_rss b_cg=$idle_cg
# A guest that used memory gives it back: 3 GiB in tmpfs, then freed.
bx "${idle_vms[0]}" 'head -c 3G /dev/zero >/dev/shm/x; rm /dev/shm/x'
peak=$(rss_mib "${idle_vms[0]}"); sleep 20; back=$(rss_mib "${idle_vms[0]}")
echo "free page reporting: RSS after freeing 3 GiB: ${peak% *} MiB, 20 s later ${back% *} MiB"
perf_del "${idle_vms[@]}"
perf_cfg '.ksmTeam = "perf"' ksm
perf_idle ksm 4 120
k_rss=$idle_rss k_cg=$idle_cg
ksm_sharing=$(($(cat /sys/kernel/mm/ksm/pages_sharing) / 256))
ksm_shared=$(($(cat /sys/kernel/mm/ksm/pages_shared) / 256))
echo "ksm: $ksm_sharing MiB saved, $ksm_shared MiB kept once (RSS counts shared pages in each VM; cgroup $k_cg MiB per VM)"
perf_del "${idle_vms[@]}"
echo 0 | sudo tee /sys/kernel/mm/ksm/run >/dev/null

log "perf e2e: 10 concurrent creates, cold and warm"
perf_ten() { # LABEL: sets ten_s
  local t ids d; d=$WORK/tmp/ten-$1; rm -rf "$d"; mkdir -p "$d"
  t=$(now)
  local pids=()
  for i in $(seq 10); do perf_vm "t$1$i" >"$d/$i" & pids+=($!); done
  wait "${pids[@]}"
  ten_s=$(secs "$t" "$(now)")
  ids=$(cat "$d"/*)
  [ "$(wc -w <<<"$ids")" = 10 ] || fail "$1: not 10 VMs: $ids"
  echo "$1: 10 concurrent creates ${ten_s}s"
}
perf_cfg '.' cold
perf_ten cold
cold_ten=$ten_s
# shellcheck disable=SC2046
perf_del $(cat "$WORK/tmp/ten-cold"/*)
perf_cfg '.warmPool = true' warm
for _ in $(seq 120); do sudo test -f "$STATE/warm/slim@e2e/warm.json" && break; sleep 1; done
sudo test -f "$STATE/warm/slim@e2e/warm.json" || fail "default image template not built: $(sudo tail -n 5 "$WORK/boatd.log")"
# The defaults key is image + size: the size under test is built by its first create.
bd DELETE "/v1/vms/$(perf_vm warm0)" >/dev/null
for _ in $(seq 120); do sudo test -f "$STATE/warm/slim@default/warm.json" && break; sleep 1; done
sudo test -f "$STATE/warm/slim@default/warm.json" || fail "template not built: $(sudo tail -n 5 "$WORK/boatd.log")"
perf_ten warm
warm_ten=$ten_s
sleep 20
wr=0; for f in "$WORK/tmp/ten-warm"/*; do r=$(rss_mib "$(cat "$f")"); wr=$((wr + ${r% *})); done
warm_rss=$((wr / 10))
echo "warm: idle RSS $warm_rss MiB per VM"
for f in "$WORK/tmp/ten-warm"/*; do fingerprint "$(cat "$f")"; echo; done >"$WORK/tmp/warm.fp"
[ "$(tr ' ' '\n' <"$WORK/tmp/warm.fp" | grep -c .)" = 20 ] || fail "warm VM without identity: $(cat "$WORK/tmp/warm.fp")"
[ "$(tr ' ' '\n' <"$WORK/tmp/warm.fp" | grep . | sort -u | wc -l)" = 20 ] || fail "warm VMs share an identity: $(cat "$WORK/tmp/warm.fp")"
# Warm VMs are real VMs: disk writable, network up, entropy differs.
w1=$(cat "$WORK/tmp/ten-warm/1"); w2=$(cat "$WORK/tmp/ten-warm/2")
bx "$w1" 'echo ok >/root/w && sync' >/dev/null || fail "warm VM disk"
bx "$w1" 'curl -fsS --max-time 10 -o /dev/null https://github.com/' || fail "warm VM network"
[ "$(bx "$w1" 'head -c 16 /dev/urandom | od -An -tx1')" != "$(bx "$w2" 'head -c 16 /dev/urandom | od -An -tx1')" ] || fail "warm VMs share an RNG"
# shellcheck disable=SC2046
perf_del $(cat "$WORK/tmp/ten-warm"/*)
sudo rm -rf "$STATE/warm"
# How many idle default VMs an 8 GiB budget holds, by RSS (cgroup memory also counts the
# reclaimable host page cache of their disks).
ksm_each=$((ksm_sharing / 4)) # pages_sharing is the saving
fit_cold=$((8192 / b_rss)) fit_warm=$((8192 / warm_rss)) fit_ksm=$((8192 / (k_rss - ksm_each)))
echo "8 GiB budget: $fit_cold idle cold-booted VMs, $fit_warm warm, $fit_ksm cold with KSM (saves $ksm_each MiB each)"
d() { jq -r ".$1" <<<"$disk"; }
check_max perf_snapshot "$kvm_snap" "$PERF_SNAPSHOT_MAX"
check_max perf_from_snap "$kvm_from" "$PERF_FROM_MAX"
check_max perf_fork "$kvm_fork" "$PERF_FORK_MAX"
check_max perf_warm_ten "$warm_ten" "$PERF_WARM_TEN_MAX"
check_mib() { # NAME MIB MAX
  printf 'e2e: %-14s %s MiB (max %s)\n' "$1" "$2" "$3"
  [ "$2" -le "$3" ] || perf_fail+="$1 was $2 MiB > $3; "
}
check_mib perf_rss "$b_rss" "$PERF_RSS_MAX"
check_mib perf_warm_rss "$warm_rss" "$PERF_WARM_RSS_MAX"
check_mib perf_freed_rss "${back% *}" "$PERF_FREED_RSS_MAX"
check_min perf_seq_write "$(d seq_write_mbps)" "$PERF_SEQ_WRITE_MIN"
check_min perf_rand_read "$(d rand_read_iops)" "$PERF_RAND_READ_MIN"
check_min perf_rand_write "$(d rand_write_iops)" "$PERF_RAND_WRITE_MIN"
restart_boatd "$WORK/boatd.json"

echo "METRIC $(jq -nc --argjson disk "$disk" --argjson disks "$disk_sync" --argjson diskiou "$disk_iou" --arg blk "$guest_blk" \
  --argjson csw "$c_sw" --argjson crw "$c_rw" --argjson crr "$c_rr" \
  --argjson ks "$kvm_snap" --argjson kf "$kvm_from" --argjson kk "$kvm_fork" --argjson kk2 "$kvm_fork2" \
  --argjson nbr "$nb_rss" --argjson nbc "$nb_cg" --argjson br "$b_rss" --argjson bc "$b_cg" \
  --argjson dnq "$disk_noq" --argjson nqs "$noq_snap" --argjson nqf "$noq_from" --argjson nqk "$noq_fork" \
  --argjson kr "$k_rss" --argjson ksm "$ksm_sharing" --argjson fc "$fit_cold" --argjson fw "$fit_warm" --argjson fk "$fit_ksm" --argjson ca "${cold_async:-0}" --argjson cs "${cold_sync:-0}" \
  --argjson ct "$cold_ten" --argjson wt "$warm_ten" --argjson wr "$warm_rss" \
  '{test: "boat-perf", disk: $disk, diskSync: $disks, diskAsyncIoUringGroup: $diskiou, guestBlk: $blk, ceilingSeqWriteBps: $csw, ceilingRandWriteIops: $crw, ceilingRandReadIops: $crr,
    snapshotS: $ks, fromSnapshotS: $kf, forkS: $kk, forkCleanS: $kk2, noQuotas: {disk: $dnq, snapshotS: $nqs, fromSnapshotS: $nqf, forkS: $nqk},
    idleRssMiB: {noBalloon: $nbr, balloon: $br, ksm: $kr, warm: $wr}, idleCgroupMiB: {noBalloon: $nbc, balloon: $bc},
    ksmDedupMiB: $ksm, fitsIn8GiB: {cold: $fc, warm: $fw, coldKsm: $fk}, coldRandReadIops: {async: $ca, sync: $cs}, tenCreatesS: {cold: $ct, warm: $wt}}')"
