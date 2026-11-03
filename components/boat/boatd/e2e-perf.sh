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
# Gates: measured on GitHub's EPYC 7763 runner (4 shared vCPU, loop btrfs, host fio
# 329-449 MB/s) in runs 41600185173 and the run named in PROTOCOL.md, plus margin. They
# catch regressions, not runner noise; rand IOPS here move by a factor of 2 between runs.
PERF_SNAPSHOT_MAX=${E2E_PERF_SNAPSHOT_MAX:-18}    # 3.8 GiB to write: 9-12.7 s
PERF_FROM_MAX=${E2E_PERF_FROM_MAX:-3}             # 1.0-1.6 s
PERF_FORK_MAX=${E2E_PERF_FORK_MAX:-4}             # right after that snapshot: 1.7-2.5 s
PERF_WARM_TEN_MAX=${E2E_PERF_WARM_TEN_MAX:-12}    # 7.9 s; 10 x 4 vCPU on 4 host CPUs
PERF_RSS_MAX=${E2E_PERF_RSS_MAX:-700}             # idle default VM: 528 MiB
PERF_WARM_RSS_MAX=${E2E_PERF_WARM_RSS_MAX:-450}   # idle warm VM: 259 MiB
PERF_FREED_RSS_MAX=${E2E_PERF_FREED_RSS_MAX:-1000} # 3 GiB used, freed, 20 s later: 535 MiB
PERF_SEQ_WRITE_MIN=${E2E_PERF_SEQ_WRITE_MIN:-250}  # MB/s: 754-1085
PERF_RAND_READ_MIN=${E2E_PERF_RAND_READ_MIN:-15000} # IOPS: 27k-76k
PERF_RAND_WRITE_MIN=${E2E_PERF_RAND_WRITE_MIN:-15000} # IOPS: 41k-56k
check_min() { # NAME VALUE MIN
  printf 'e2e: %-14s %s (min %s)\n' "$1" "$2" "$3"
  awk -v a="$2" -v b="$3" 'BEGIN { exit !(a >= b) }' || perf_fail+="$1 was $2 < $3; "
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
(cd "$HERE/../../bench/boatbench" && python3 -c 'import workloads; print(workloads.DISK)') >"$WORK/tmp/disk.sh"

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
put_disk() { curl -fsS --unix-socket "$SOCK" -X PUT --data-binary "@$WORK/tmp/disk.sh" "http://boatd/v1/vms/$1/files?path=/tmp/disk.sh&mode=755" >/dev/null; }
put_disk "$pv"
disk=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p')
echo "disk (Async): $disk"
cold_async=$(cold_read "$pv")
echo "cold rand read (Async): $cold_async IOPS"
echo "extents after boatbench's fio: $(vm_extents "$pv")"

perf_del "$pv"
perf_cfg '.diskEngine = "Sync"' sync
pv=$(perf_vm pv)
put_disk "$pv"
disk_sync=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p')
echo "disk (Sync, same runner): $disk_sync"
cold_sync=$(cold_read "$pv")
echo "cold rand read (Sync): $cold_sync IOPS"
perf_del "$pv"
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
echo "ksm: $ksm_sharing MiB deduplicated into $ksm_shared MiB, RSS column does not show it (cgroup: $k_cg MiB per VM before scan)"
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
check_max perf_rss "$b_rss" "$PERF_RSS_MAX"
check_max perf_warm_rss "$warm_rss" "$PERF_WARM_RSS_MAX"
check_max perf_freed_rss "${back% *}" "$PERF_FREED_RSS_MAX"
check_min perf_seq_write "$(d seq_write_mbps)" "$PERF_SEQ_WRITE_MIN"
check_min perf_rand_read "$(d rand_read_iops)" "$PERF_RAND_READ_MIN"
check_min perf_rand_write "$(d rand_write_iops)" "$PERF_RAND_WRITE_MIN"
restart_boatd "$WORK/boatd.json"

echo "METRIC $(jq -nc --argjson disk "$disk" --argjson disks "$disk_sync" --arg blk "$guest_blk" \
  --argjson csw "$c_sw" --argjson crw "$c_rw" --argjson crr "$c_rr" \
  --argjson ks "$kvm_snap" --argjson kf "$kvm_from" --argjson kk "$kvm_fork" --argjson kk2 "$kvm_fork2" \
  --argjson nbr "$nb_rss" --argjson nbc "$nb_cg" --argjson br "$b_rss" --argjson bc "$b_cg" \
  --argjson kr "$k_rss" --argjson ksm "$ksm_sharing" --argjson fc "$fit_cold" --argjson fw "$fit_warm" --argjson fk "$fit_ksm" --argjson ca "${cold_async:-0}" --argjson cs "${cold_sync:-0}" \
  --argjson ct "$cold_ten" --argjson wt "$warm_ten" --argjson wr "$warm_rss" \
  '{test: "boat-perf", disk: $disk, diskSync: $disks, guestBlk: $blk, ceilingSeqWriteBps: $csw, ceilingRandWriteIops: $crw, ceilingRandReadIops: $crr,
    snapshotS: $ks, fromSnapshotS: $kf, forkS: $kk, forkCleanS: $kk2,
    idleRssMiB: {noBalloon: $nbr, balloon: $br, ksm: $kr, warm: $wr}, idleCgroupMiB: {noBalloon: $nbc, balloon: $bc},
    ksmDedupMiB: $ksm, fitsIn8GiB: {cold: $fc, warm: $fw, coldKsm: $fk}, coldRandReadIops: {async: $ca, sync: $cs}, tenCreatesS: {cold: $ct, warm: $wt}}')"
