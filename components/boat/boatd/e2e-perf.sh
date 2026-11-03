# shellcheck shell=bash
# shellcheck disable=SC2154 # WORK, STATE, HERE and the helpers come from e2e.sh / e2e-host.sh
# Performance of a 4 vCPU / 8 GiB VM, measured the way bench/boatbench does (its
# fio script is imported, not copied). Sourced by e2e-host.sh with no VMs left.
# Prints `METRIC {...}`; timings past their gate fail the run after everything ran.
#
#   ceiling   fio on the host's state dir: what the guest cannot beat on this runner
#   disk      fio in a VM; extents of its disk; guest scheduler and discard
#   snapshot  named snapshot, new-from-snapshot, fork of a VM with 2 GiB of dirty RAM
#             and a fragmented disk (fio randwrite)
#   density   idle RSS per default VM with and without the balloon, KSM saving
#   warm      10 concurrent creates, cold and from a warm template

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
put_disk() { curl -fsS --unix-socket "$SOCK" -X PUT --data-binary "@$WORK/tmp/disk.sh" "http://boatd/v1/vms/$1/files?path=/tmp/disk.sh&mode=755" >/dev/null; }
put_disk "$pv"
disk=$(bx "$pv" 'bash /tmp/disk.sh' 300 | sed -n 's/^BB_JSON //p')
echo "disk: $disk"
echo "extents after boatbench's fio: $(vm_extents "$pv")"

log "perf e2e: snapshot, new-from-snapshot, fork (4/8, 2 GiB dirty, fragmented disk)"
perf_snap() { # LABEL: one measurement set on a fresh VM; sets snap_s fromsnap_s forkN_s
  local v sn f; v=$(perf_vm "s$1")
  put_disk "$v"
  bx "$v" 'bash /tmp/disk.sh' 300 >/dev/null
  perf_dirty "$v"
  echo "$1: dirtying 4 GiB of tmpfs took $(bx "$v" 's=$(date +%s.%N); head -c 4G /dev/zero >/dev/shm/d/w; e=$(date +%s.%N); rm /dev/shm/d/w; awk -v a="$s" -v b="$e" "BEGIN {print b-a}"')s"
  echo "$1: extents $(vm_extents "$v"), guest $(bx "$v" 'free -m | awk "/Mem:/ {print \"used=\"\$3,\"cache=\"\$6}"')"
  local t; t=$(now); sn=$(bd POST "/v1/vms/$v/snapshots" '{"name":"x"}' | jq -r .id); snap_s=$(secs "$t" "$(now)")
  echo "$1: snapshot ${snap_s}s: $(last_snap_log)"
  t=$(now); f=$(bd POST /v1/vms "{\"name\":\"n$1\",\"team\":\"perf\",\"snapshot\":\"$sn\"}" | jq -r .id); fromsnap_s=$(secs "$t" "$(now)")
  [ "$(vmf "$f" agent_ready)" = true ] || fail "new-from-snapshot not ready"
  echo "$1: new-from-snapshot ${fromsnap_s}s: $(sudo grep -E "$f: (boot|guest)" "$WORK/boatd.log" | sed -E 's/^.*'"$f"': //' | tr '\n' ' ')"
  perf_del "$f"
  t=$(now); f=$(bd POST "/v1/vms/$v/fork" "{\"name\":\"f$1\"}" | jq -r .id); fork_s=$(secs "$t" "$(now)")
  [ "$(vmf "$f" agent_ready)" = true ] || fail "fork not ready"
  [ "$(bx "$f" 'ls -l /dev/shm/d/dirty | wc -l')" = 1 ] || fail "fork lost tmpfs"
  echo "$1: fork ${fork_s}s: $(last_snap_log)"
  perf_del "$f"
  bd DELETE "/v1/vms/$v/snapshots/$sn" >/dev/null
  perf_del "$v"
}
perf_snap kvm
kvm_snap=$snap_s kvm_from=$fromsnap_s kvm_fork=$fork_s
perf_cfg '.snapshotTracking = "mincore"' mincore
perf_snap mincore
min_snap=$snap_s min_from=$fromsnap_s min_fork=$fork_s
perf_cfg '.snapshotTracking = "mincore" | .snapshotDropCaches = true' mincoredrop
perf_snap mincoredrop
mind_snap=$snap_s mind_from=$fromsnap_s mind_fork=$fork_s
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
fit_before=$((8192 / nb_cg)) fit_after=$((8192 / b_cg))
echo "8 GiB budget: $fit_before idle VMs without the balloon, $fit_after with it"

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
restart_boatd "$WORK/boatd.json"

echo "METRIC $(jq -nc --argjson disk "$disk" --arg blk "$guest_blk" \
  --argjson csw "$c_sw" --argjson crw "$c_rw" --argjson crr "$c_rr" \
  --argjson ks "$kvm_snap" --argjson kf "$kvm_from" --argjson kk "$kvm_fork" \
  --argjson ms "$min_snap" --argjson mf "$min_from" --argjson mk "$min_fork" \
  --argjson ds "$mind_snap" --argjson df "$mind_from" --argjson dk "$mind_fork" \
  --argjson nbr "$nb_rss" --argjson nbc "$nb_cg" --argjson br "$b_rss" --argjson bc "$b_cg" \
  --argjson kr "$k_rss" --argjson ksm "$ksm_sharing" --argjson fb "$fit_before" --argjson fa "$fit_after" \
  --argjson ct "$cold_ten" --argjson wt "$warm_ten" \
  '{test: "boat-perf", disk: $disk, guestBlk: $blk, ceilingSeqWriteBps: $csw, ceilingRandWriteIops: $crw, ceilingRandReadIops: $crr,
    kvm: {snapshotS: $ks, fromSnapshotS: $kf, forkS: $kk}, mincore: {snapshotS: $ms, fromSnapshotS: $mf, forkS: $mk}, mincoreDrop: {snapshotS: $ds, fromSnapshotS: $df, forkS: $dk},
    idleRssMiB: {noBalloon: $nbr, balloon: $br, ksm: $kr}, idleCgroupMiB: {noBalloon: $nbc, balloon: $bc},
    ksmDedupMiB: $ksm, fitsIn8GiB: {before: $fb, after: $fa}, tenCreatesS: {cold: $ct, warm: $wt}}')"
