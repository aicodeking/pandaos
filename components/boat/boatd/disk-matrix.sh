#!/usr/bin/env bash
# Disk matrix: the host and a 4 vCPU / 8 GiB VM, several boatd disk configurations,
# all on ONE runner in one run, so ratios compare like with like. Not a gate.
#
#   bench/boatbench/stack.sh up GUEST_DIR      # boatd + api from source (production defaults)
#   components/boat/boatd/disk-matrix.sh       # prints MATRIX {json} lines and a table
#
# Jobs: boatbench's own fio set ("ours": direct, libaio, iodepth 32, 15 s), the
# pts/fio-2.1.0 job file (iodepth 64, 60 s) for configs marked pts, and two
# durability jobs (4k random writes with an fsync each; 2 GiB buffered write +
# fsync). Every guest job records the request count and size that reached vda
# (/sys/block/vda/stat), and the CPU time of each Firecracker thread.
#
#   CONFIGS="label@jq filter over boatd.json@pts(0/1)" lines override the list.
set -euo pipefail
WORK=${BENCH_WORK:-/mnt/boatbench}
STATE=${BENCH_STATE:-/var/lib/boatbench}
SOCK=$WORK/run/boatd.sock
OUTF=${MATRIX_OUT:-$WORK/matrix.jsonl}
: >"$OUTF"
log() { printf '\n\033[1m==> %s\033[0m\n' "$*" >&2; }
bd() { curl -fsS --unix-socket "$SOCK" -X "$1" "http://boatd$2" -H 'content-type: application/json' ${3:+-d "$3"}; }
bx() { # ID CMD [TIMEOUT]
  local out
  out=$(bd POST "/v1/vms/$1/exec" "$(jq -nc --arg c "$2" --argjson t "${3:-300}" '{argv: ["bash", "-c", $c], user: "root", timeout_s: $t}')") || return 1
  jq -rs 'map(select(.stream=="stdout").data_b64 | @base64d) | join("")' <<<"$out"
  [ "$(jq -s 'map(select(has("exit")))[0].exit' <<<"$out")" = 0 ] || { jq -rs 'map(select(.stream=="stderr").data_b64 | @base64d) | join("")' <<<"$out" >&2; return 1; }
}
emit() { echo "MATRIX $1"; echo "$1" >>"$OUTF"; }
# The guest installs fio from the runner's apt mirror: archive.ubuntu.com can stall for minutes.
HOST_MIRROR=$(grep -rhoE 'https?://[a-z0-9.-]+\.ubuntu\.com/ubuntu/?' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null |
  grep -vE '//(archive|security)\.ubuntu\.com' | head -1 | sed 's|/*$||' || true)
guest_fio() {
  [ -z "$HOST_MIRROR" ] || printf "sed -i -e 's|http://archive.ubuntu.com/ubuntu|%s|' -e 's|http://security.ubuntu.com/ubuntu|%s|' /etc/apt/sources.list.d/ubuntu.sources 2>/dev/null; " "$HOST_MIRROR" "$HOST_MIRROR"
  echo 'command -v fio >/dev/null || { apt-get install -y -qq fio >/dev/null 2>&1 || { apt-get update -qq >/dev/null && apt-get install -y -qq fio >/dev/null; }; }'
}
BASE=$WORK/boatd.json
[ -f "$WORK/boatd.base.json" ] || cp "$BASE" "$WORK/boatd.base.json"
BASE=$WORK/boatd.base.json
DEFARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"
THP=".bootArgs = ((.bootArgs // \"$DEFARGS\") + \" transparent_hugepage=always\")"
CONFIGS=${CONFIGS:-"base@.@1
sync@.diskEngine = \"Sync\"@0
writeback@.diskCache = \"Writeback\"@1
thp@$THP@1
thp_writeback@$THP | .diskCache = \"Writeback\"@0
pagecache16g@.vmPageCacheMiB = 16384@0
untrusted@.diskEngine = \"Sync\" | .ioMaxMBps = 500 | .ioMaxIOPS = 20000@1
caps_async@.ioMaxMBps = 500 | .ioMaxIOPS = 20000@0
untrusted_2g_80k@.diskEngine = \"Sync\" | .ioMaxMBps = 2000 | .ioMaxIOPS = 80000@0
base_again@.@0"}

# blk-iocost stays on once a config enabled it: every restart turns it off first.
STATE_DEV=$(lsblk -no MAJ:MIN "$(findmnt -no SOURCE "$STATE")" | head -1 | tr -d ' ')
MODEL=${IOCOST_MODEL:-} # io.cost.model measured from the host pass (host_model), for %MODEL% in filters
restart() { # JQ_FILTER
  jq "${1//%MODEL%/$MODEL}" "$BASE" >"$WORK/boatd.matrix.json"
  sudo kill -9 "$(cat "$WORK/boatd.pid")" 2>/dev/null || true
  sleep 0.5
  echo "$STATE_DEV enable=0" | sudo tee /sys/fs/cgroup/io.cost.qos >/dev/null 2>&1 || true
  sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >>"$4" 2>&1' _ \
    "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.matrix.json" "$WORK/boatd.log" &
  for _ in $(seq 600); do curl -fs --unix-socket "$SOCK" http://boatd/v1/health >/dev/null && return; sleep 0.1; done
  echo "boatd did not come back"; tail -30 "$WORK/boatd.log"; exit 1
}

# Firecracker's per-thread CPU ticks: "comm ticks" (comm: firecracker, fc_vmm, fc_vcpu N, io_uring workers...)
fc_ticks() {
  local t
  for t in /proc/"$1"/task/*; do
    sudo awk '{ c=$0; sub(/^[0-9]+ \(/, "", c); sub(/\) .*/, "", c); n=split($0, f, ") "); split(f[n], g, " "); gsub(/ /, "_", c); print c, g[12]+g[13] }' "$t/stat" 2>/dev/null
  done | awk '{ s[$1]+=$2 } END { for (k in s) print k, s[k] }'
}
cpu_delta() { # BEFORE AFTER SECONDS -> {"comm": busy%}
  join -a2 -e0 -o 0,1.2,2.2 <(sort <<<"$1") <(sort <<<"$2") |
    awk -v s="$3" -v hz="$(getconf CLK_TCK)" 'BEGIN { printf "{" } { if (NR>1) printf ","; printf "\"%s\":%.0f", $1, ($3-$2)/hz/s*100 } END { print "}" }'
}

GJ=$(cat <<'PY'
import json, subprocess, sys, time
def stat():
    return [int(x) for x in open("/sys/block/vda/stat").read().split()]
name, args = sys.argv[1], sys.argv[2:]
a, t = stat(), time.time()
r = subprocess.run(["fio", "--output-format=json"] + args, capture_output=True, text=True)
b, t = stat(), time.time() - t
d = [y - x for x, y in zip(a, b)]
try:
    j = json.loads(r.stdout[r.stdout.index("{"):])["jobs"][0]
except Exception:
    print("GJ " + json.dumps({"job": name, "error": r.stderr[-300:]})); sys.exit(0)
kib = lambda sec, n: round(sec * 512 / 1024 / n, 1) if n else None
print("GJ " + json.dumps({"job": name, "s": round(t, 1),
    "read_mbps": round(j["read"]["bw_bytes"] / 1e6, 1), "write_mbps": round(j["write"]["bw_bytes"] / 1e6, 1),
    "read_mibps": round(j["read"]["bw_bytes"] / 2**20, 1), "write_mibps": round(j["write"]["bw_bytes"] / 2**20, 1),
    "read_iops": round(j["read"]["iops"]), "write_iops": round(j["write"]["iops"]),
    "vda_reads": d[0], "vda_read_kib": kib(d[2], d[0]), "vda_writes": d[4], "vda_write_kib": kib(d[6], d[4]),
    "vda_flushes": d[15] if len(d) > 15 else None}))
PY
)

# name|fio args (@F@ is the file) — the same parameters as bench/boatbench/workloads.py DISK.
OURS="--direct=1 --ioengine=libaio --numjobs=1 --time_based --group_reporting --name=bb --filename=@F@ --size=2G --iodepth=32 --runtime=15"
PTS="--direct=1 --ioengine=libaio --numjobs=1 --time_based --group_reporting --name=test --filename=@F@ --size=1g --iodepth=64 --startdelay=20 --ramp_time=5 --runtime=60"
JOBS_OURS="seq_write|$OURS --rw=write --bs=1M
seq_read|$OURS --rw=read --bs=1M
rand_write|$OURS --rw=randwrite --bs=4k
rand_read|$OURS --rw=randread --bs=4k"
JOBS_PTS="pts_rand_read|$PTS --rw=randread --bs=4k
pts_rand_write|$PTS --rw=randwrite --bs=4k
pts_seq_write|$PTS --rw=write --bs=1M"
JOBS_DUR="fsync_rand_write|--name=fs --filename=@F@ --size=256M --direct=0 --ioengine=sync --rw=randwrite --bs=4k --fsync=1 --time_based --runtime=15
seq_write_end_fsync|--name=ef --filename=@F@ --size=2G --direct=0 --ioengine=psync --rw=write --bs=1M --end_fsync=1"

host_run() { # LABEL: the same jobs on the host's state dir, NOCOW like VM disks
  local f="$STATE/matrix.fio" name args out
  sudo rm -f "$f"; sudo touch "$f"; sudo chattr +C "$f" 2>/dev/null || true
  while IFS='|' read -r name args; do
    [ -n "$name" ] || continue
    out=$(sudo fio --output-format=json ${args//@F@/$f} </dev/null | jq -c --arg j "$name" '.jobs[0] | {job: $j,
      read_mbps: (.read.bw_bytes/1e6|.*10|round/10), write_mbps: (.write.bw_bytes/1e6|.*10|round/10),
      read_mibps: (.read.bw_bytes/1048576|.*10|round/10), write_mibps: (.write.bw_bytes/1048576|.*10|round/10),
      read_iops: (.read.iops|round), write_iops: (.write.iops|round)}')
    emit "$(jq -c --arg c "$1" '. + {side: "host", config: $c}' <<<"$out")"
    sudo rm -f "$f"; sudo touch "$f"; sudo chattr +C "$f" 2>/dev/null || true
  done <<<"$JOBS_OURS
$JOBS_PTS
$JOBS_DUR
seq_write_buffered|${OURS/--direct=1/--direct=0} --rw=write --bs=1M
rand_read_buffered|${OURS/--direct=1/--direct=0} --rw=randread --bs=4k"
  sudo rm -f "$f"
}

vm_run() { # LABEL FILTER PTS
  local label=$1 id pid name args before after t r cg jobs
  log "config $label: $2"
  restart "$2"
  id=$(bd POST /v1/vms "$(jq -nc --arg n "m$label" '{name: ($n | gsub("_"; "-")), team: "matrix", size: "default"}')" | jq -r .id)
  pid=$(bd GET "/v1/vms/$id" | jq -r .pid)
  bx "$id" "$(guest_fio)" 400
  bx "$id" "cat >/tmp/gj.py <<'EOF'
$GJ
EOF
mkdir -p /root/bbdisk"
  emit "$(bx "$id" 'q=/sys/block/vda/queue; printf "{\"max_segments\":%s,\"max_sectors_kb\":%s,\"max_segment_size\":%s,\"nr_requests\":%s,\"read_ahead_kb\":%s,\"scheduler\":\"%s\",\"thp\":\"%s\",\"write_cache\":\"%s\",\"cmdline\":\"%s\"}" $(cat $q/max_segments $q/max_sectors_kb $q/max_segment_size $q/nr_requests $q/read_ahead_kb) "$(cat $q/scheduler)" "$(cat /sys/kernel/mm/transparent_hugepage/enabled)" "$(cat $q/write_cache)" "$(cat /proc/cmdline)"' |
    jq -c --arg c "$label" '{side: "guest-queue", config: $c} + .')"
  jobs="$JOBS_OURS
$JOBS_DUR"
  [ "$3" = 1 ] && jobs="$JOBS_OURS
$JOBS_PTS
$JOBS_DUR"
  while IFS='|' read -r name args; do
    [ -n "$name" ] || continue
    before=$(fc_ticks "$pid"); t=$(date +%s.%N)
    r=$(bx "$id" "python3 /tmp/gj.py $name ${args//@F@//root/bbdisk/f}; rm -f /root/bbdisk/f" 400 </dev/null | sed -n 's/^GJ //p') || r='{"error":"exec failed"}'
    [ -n "$r" ] || r='{"error":"no result"}'
    after=$(fc_ticks "$pid")
    cg=$(cpu_delta "$before" "$after" "$(awk -v a="$t" -v b="$(date +%s.%N)" 'BEGIN { print b - a }')")
    emit "$(jq -c --arg c "$label" --argjson cpu "$cg" '. + {side: "vm", config: $c, fc_cpu_pct: $cpu}' <<<"$r")"
  done <<<"$jobs"
  local cgd
  cgd=/sys/fs/cgroup/$(jq -r .cgroupParent "$WORK/boatd.matrix.json")/$id
  emit "$(jq -nc --arg c "$label" --arg io "$(cat "$cgd/io.stat" 2>/dev/null)" --arg iomax "$(cat "$cgd/io.max" 2>/dev/null)" \
    --arg ev "$(tr '\n' ' ' <"$cgd/memory.events" 2>/dev/null)" --arg ms "$(grep -E '^(file|file_dirty|file_writeback|anon) ' "$cgd/memory.stat" 2>/dev/null | tr '\n' ' ')" \
    '{side: "cgroup", config: $c, io_stat: $io, io_max: $iomax, memory_events: $ev, memory_stat: $ms}')"
  bd DELETE "/v1/vms/$id" >/dev/null
}

# Noisy neighbour: VM b's latency-bound and bandwidth jobs alone, then while VM a floods
# the disk (direct 1M writes, iodepth 32). PAIRS="label@jq filter" lines; empty = skip.
PAIRS=${PAIRS-}
VICTIM="fsync_rand_write|--name=fs --filename=@F@ --size=256M --direct=0 --ioengine=sync --rw=randwrite --bs=4k --fsync=1 --time_based --runtime=15
seq_write|$OURS --rw=write --bs=1M"
AGGR="--name=ag --filename=/root/bbdisk/ag --size=4G --direct=1 --ioengine=libaio --iodepth=32 --rw=write --bs=1M --time_based --runtime=75"
newvm() { bd POST /v1/vms "$(jq -nc --arg n "$1" '{name: $n, team: "matrix", size: "default"}')" | jq -r .id; }
prep() {
  bx "$1" "$(guest_fio)" 400
  bx "$1" "cat >/tmp/gj.py <<'EOF'
$GJ
EOF
mkdir -p /root/bbdisk"
}
victim() { # LABEL PHASE ID
  local name args r
  while IFS='|' read -r name args; do
    [ -n "$name" ] || continue
    r=$(bx "$3" "python3 /tmp/gj.py $name ${args//@F@//root/bbdisk/f}; rm -f /root/bbdisk/f" 400 </dev/null | sed -n 's/^GJ //p') || r='{"error":"exec failed"}'
    [ -n "$r" ] || r='{"error":"no result"}'
    emit "$(jq -c --arg c "$1" --arg p "$2" '. + {side: "pair", phase: $p, config: $c}' <<<"$r")"
  done <<<"$VICTIM"
}
pair_run() { # LABEL FILTER
  local a b ag
  log "pair $1: $2"
  restart "$2"
  a=$(newvm "pa-${1//_/-}"); b=$(newvm "pb-${1//_/-}")
  prep "$a"; prep "$b"
  victim "$1" alone "$b"
  bx "$a" "python3 /tmp/gj.py aggressor $AGGR; rm -f /root/bbdisk/ag" 400 </dev/null >"$WORK/aggr.out" &
  ag=$!
  sleep 8
  victim "$1" noisy "$b"
  wait "$ag" || true
  emit "$(sed -n 's/^GJ //p' "$WORK/aggr.out" | jq -c --arg c "$1" '. + {side: "pair", phase: "aggressor", config: $c}')"
  emit "$(jq -nc --arg c "$1" --arg q "$(cat /sys/fs/cgroup/io.cost.qos 2>/dev/null)" --arg w "$(cat "/sys/fs/cgroup/$(jq -r .cgroupParent "$WORK/boatd.matrix.json")/$a/io.weight" 2>/dev/null)" \
    '{side: "pair-cgroup", config: $c, io_cost_qos: $q, io_weight: $w}')"
  bd DELETE "/v1/vms/$a" >/dev/null; bd DELETE "/v1/vms/$b" >/dev/null
}

log "host: $(findmnt -no SOURCE,FSTYPE,OPTIONS "$STATE")"
loopdev=$(findmnt -no SOURCE "$STATE")
emit "$(jq -nc --arg m "$(findmnt -no SOURCE,FSTYPE,OPTIONS "$STATE")" --arg l "$(losetup -l -O NAME,BACK-FILE,DIO 2>/dev/null | tr '\n' ';')" \
  --arg k "$(uname -r)" --arg cpu "$(lscpu | sed -n 's/^Model name: *//p' | head -1)" --arg b "$(findmnt -no SOURCE,FSTYPE -T "$WORK")" \
  --arg dr "$(sysctl -n vm.dirty_ratio) $(sysctl -n vm.dirty_background_ratio)" --arg sched "$(cat /sys/block/"$(basename "$loopdev")"/queue/scheduler 2>/dev/null)" \
  '{side: "hostinfo", mount: $m, loop: $l, kernel: $k, cpu: $cpu, backing: $b, dirty_ratio_bg: $dr, loop_sched: $sched}')"

# The host's own fio numbers as an io.cost.model: what the disk does, not the kernel's SSD default.
host_model() {
  python3 - "$OUTF" <<'PY2'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
h = {r["job"]: r for r in rows if r.get("side") == "host" and r.get("config") == "host0"}
rr, wr = h["rand_read"]["read_iops"], h["rand_write"]["write_iops"]
print("rbps=%d rseqiops=%d rrandiops=%d wbps=%d wseqiops=%d wrandiops=%d" % (
    h["seq_read"]["read_mbps"] * 1e6, rr, rr, h["seq_write"]["write_mbps"] * 1e6, wr, wr))
PY2
}

i=0
while IFS='@' read -r label filter pts; do
  [ -n "$label" ] || continue
  if [ $((i % 4)) = 0 ]; then
    log "host pass $((i / 4))"; host_run "host$((i / 4))" </dev/null
    if [ "$i" = 0 ] && [ -z "$MODEL" ]; then MODEL=$(host_model); emit "$(jq -nc --arg m "$MODEL" '{side: "iocost-model", model: $m}')"; fi
  fi
  vm_run "$label" "$filter" "$pts" </dev/null || emit "$(jq -nc --arg c "$label" '{side: "vm", config: $c, error: "config failed"}')"
  i=$((i + 1))
done <<<"$CONFIGS"
host_run hostlast
while IFS='@' read -r label filter; do
  [ -n "$label" ] || continue
  pair_run "$label" "$filter" </dev/null || emit "$(jq -nc --arg c "$label" '{side: "pair", config: $c, error: "pair failed"}')"
done <<<"$PAIRS"
restart .

log "table (MB/s or IOPS; vda KiB/request)"
python3 - "$OUTF" <<'PY'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
val = lambda r: (r.get("write_mbps") or r.get("read_mbps")) if r["job"].startswith("seq") or "mibps" in r["job"] else (r.get("write_iops") or r.get("read_iops"))
jobs = []
for r in rows:
    if r.get("job") and r["job"] not in jobs: jobs.append(r["job"])
cols = []
for r in rows:
    if r["side"] in ("vm", "host") and r.get("job") and r["config"] not in cols: cols.append(r["config"])
print("job".ljust(22) + "".join(c[:15].rjust(16) for c in cols))
for j in jobs:
    line = j.ljust(22)
    for c in cols:
        m = [r for r in rows if r.get("job") == j and r["config"] == c]
        if not m: line += "-".rjust(16); continue
        r = m[0]; v = r.get("write_mibps") if j == "pts_seq_write" else val(r)
        k = r.get("vda_write_kib") if "write" in j else r.get("vda_read_kib")
        line += (f"{v}" + (f"/{k}k" if k else "")).rjust(16)
    print(line)
PY
