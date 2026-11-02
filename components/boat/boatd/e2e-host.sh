# shellcheck shell=bash
# boatd host-level e2e phases, sourced by ../e2e.sh after the CLI flow (which
# leaves no VMs). Talks to boatd's socket directly. Uses from e2e.sh: SOCK,
# WORK, STATE, log, fail, now, secs, PIDS, host_ip; boatd runs with
# ipv6Prefix=fd42:b0a7::/64 and reapIntervalS=5.
#
#   perf      stop / resume / fork of a 2 GiB VM through boatd (diff snapshots)
#   memory    a fork keeps the parent's processes (in-memory HTTP server) and boot_id
#   entropy   parent, its fork and two siblings from one snapshot: distinct
#             ssh host keys and machine-ids
#   jail      firecracker in its own netns + pid ns, cgroup memory.max set
#   isolation VM<->VM same team / cross team, RFC1918 target, host services,
#             cloud metadata, SMTP: blocked (with positive controls)
#   ipv6      host<->VM ping6; from outside only published ports
#   idle      idle auto-stop
#   restart   boatd SIGKILLed mid-run: running VMs re-adopted
#   floor     free-space floor: create/snapshot refused (507), stop falls back cold
#   docker    docker run hello-world in the full image
#   qgroups   btrfsQgroups: VM dirs are limited subvolumes, fork across them works
#             (last: quota accounting stays on for the whole filesystem and
#             makes reflinks slower, so the timings above are without it)

# shellcheck disable=SC2154 # host_ip, PIDS come from e2e.sh
FORK_MAX=${E2E_FORK_MAX:-2}
STOP_MAX=${E2E_STOP_MAX:-3}
RESUME_MAX=${E2E_RESUME_MAX:-5}

bd() { curl -fsS --unix-socket "$SOCK" -X "$1" "http://boatd$2" -H 'content-type: application/json' ${3:+-d "$3"}; }
bdcode() { curl -s -o "$WORK/tmp/bd.out" -w '%{http_code}' --unix-socket "$SOCK" -X "$1" "http://boatd$2" -H 'content-type: application/json' ${3:+-d "$3"}; }
# bx ID CMD -> stdout of `bash -c CMD` as root in the VM; non-zero unless it exits 0
bx() {
  local out
  out=$(bd POST "/v1/vms/$1/exec" "$(jq -nc --arg c "$2" '{argv: ["bash", "-c", $c], user: "root", timeout_s: 180}')") || return 1
  [ "$(jq -s 'map(select(has("exit")))[0].exit' <<<"$out")" = 0 ] || { jq -rs 'map(select(.stream=="stderr").data_b64 | @base64d) | join("")' <<<"$out" >&2; return 1; }
  jq -rs 'map(select(.stream=="stdout").data_b64 | @base64d) | join("")' <<<"$out"
}
blocked() { if bx "$1" "$2" >/dev/null 2>&1; then fail "not blocked from $1: $2"; fi; }
tcp() { echo "timeout 5 bash -c '</dev/tcp/$1/$2'"; } # a bash TCP connect probe
vmf() { bd GET "/v1/vms/$1" | jq -r ".$2"; }
newvm() { bd POST /v1/vms "$1" | jq -r .id; }
fingerprint() { bx "$1" 'cat /etc/machine-id; cut -d" " -f2 /etc/ssh/ssh_host_ed25519_key.pub' | tr '\n' ' '; }
perf_fail="" # timing misses fail the run after METRIC, so every phase still runs
check_max() { # NAME SECONDS MAX
  printf 'e2e: %-12s %6ss (max %ss)\n' "$1" "$2" "$3"
  awk -v a="$2" -v b="$3" 'BEGIN { exit !(a <= b) }' || perf_fail+="$1 took $2s > $3s; "
}
restart_boatd() { # CONFIG: SIGKILL boatd (no snapshot-all), start it with CONFIG
  sudo kill -9 "$(cat "$WORK/boatd.pid")"
  for _ in $(seq 50); do sudo kill -0 "$(cat "$WORK/boatd.pid")" 2>/dev/null || break; sleep 0.1; done
  sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >>"$4" 2>&1' _ \
    "$WORK/boatd.pid" "$WORK/bin/boatd" "$1" "$WORK/boatd.log" &
  for _ in $(seq 600); do curl -fs --unix-socket "$SOCK" http://boatd/v1/health >/dev/null && return; sleep 0.1; done
  fail "boatd did not come back"
}

log "host e2e: perf (2 GiB VM, through boatd)"
p1=$(newvm '{"name":"p1","team":"iso1"}')
[ "$(vmf "$p1" agent_ready)" = true ] || fail "p1 not ready"
curl -fsS --unix-socket "$SOCK" -X PUT --data-binary @- "http://boatd/v1/vms/$p1/files?path=/tmp/tok.py&mode=755" >/dev/null <<'PY'
import http.server, os, socket
tok = os.urandom(16).hex()  # lives only in this process's memory
class S(http.server.ThreadingHTTPServer):
    address_family = socket.AF_INET6
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(tok.encode())
    def log_message(self, *a):
        pass
S(("::", 8081), H).serve_forever()
PY
bx "$p1" 'setsid nohup python3 /tmp/tok.py >/dev/null 2>&1 </dev/null & sleep 1; curl -fsS http://localhost:8081/' >/dev/null || fail "token server"
tok=$(bx "$p1" 'curl -fsS http://localhost:8081/')
boot_p1=$(bx "$p1" 'cat /proc/sys/kernel/random/boot_id')
bx "$p1" 'head -c 300M /dev/urandom >/dev/shm/dirty; sync' # dirty 300 MiB of guest RAM
t=$(now); bd POST "/v1/vms/$p1/stop" >/dev/null; stop2_s=$(secs "$t" "$(now)")
[ "$(vmf "$p1" has_snapshot)" = true ] || fail "p1 stop without snapshot"
t=$(now); bd POST "/v1/vms/$p1/start" '{}' >/dev/null; resume2_s=$(secs "$t" "$(now)")
[ "$(bx "$p1" 'curl -fsS http://localhost:8081/')" = "$tok" ] || fail "p1 lost its memory across stop/start"
bx "$p1" 'head -c 100M /dev/urandom >/dev/shm/dirty2'
t=$(now); bd POST "/v1/vms/$p1/stop" >/dev/null; stop3_s=$(secs "$t" "$(now)") # diff after restore
bd POST "/v1/vms/$p1/start" '{}' >/dev/null
[ "$(bx "$p1" 'curl -fsS http://localhost:8081/; sha256sum </dev/shm/dirty2 >/dev/null && echo ok')" = "${tok}ok" ] || fail "p1 memory after second diff stop"

log "host e2e: fork keeps memory; entropy of fork and snapshot siblings"
t=$(now); c1=$(bd POST "/v1/vms/$p1/fork" '{"name":"c1"}' | jq -r .id); fork2_s=$(secs "$t" "$(now)")
[ "$(vmf "$c1" agent_ready)" = true ] || fail "c1 identity: $(vmf "$c1" error)"
[ "$(bx "$c1" 'curl -fsS http://localhost:8081/')" = "$tok" ] || fail "fork lost the parent's in-memory server"
[ "$(bx "$c1" 'cat /proc/sys/kernel/random/boot_id')" = "$boot_p1" ] || fail "fork rebooted (boot_id differs)"
sid=$(bd POST "/v1/vms/$p1/snapshots" '{"name":"sib"}' | jq -r .id)
f1=$(newvm "{\"name\":\"f1\",\"team\":\"iso1\",\"snapshot\":\"$sid\"}")
f2=$(newvm "{\"name\":\"f2\",\"team\":\"iso1\",\"snapshot\":\"$sid\"}")
fps=$(for v in "$p1" "$c1" "$f1" "$f2"; do fingerprint "$v"; echo; done)
echo "$fps"
[ "$(tr ' ' '\n' <<<"$fps" | grep -c .)" = 8 ] || fail "missing machine-id/host key"
[ "$(tr ' ' '\n' <<<"$fps" | grep . | sort -u | wc -l)" = 8 ] || fail "machine-ids/host keys not all distinct: $fps"
echo "stop ${stop2_s}s (first, diff since boot) ${stop3_s}s (diff since restore), resume ${resume2_s}s, fork ${fork2_s}s"
check_max fork_cli "$fork_s" "$FORK_MAX"
check_max stop_cli "$stop_s" "$STOP_MAX"
check_max fork "$fork2_s" "$FORK_MAX"
check_max stop_diff "$stop3_s" "$STOP_MAX"

log "host e2e: jail (netns, pid ns, cgroup)"
pid=$(vmf "$p1" pid)
[ "$(sudo readlink "/proc/$pid/ns/net")" != "$(readlink /proc/self/ns/net)" ] || fail "firecracker in the host netns"
[ "$(sudo readlink "/proc/$pid/ns/pid")" != "$(readlink /proc/self/ns/pid)" ] || fail "firecracker in the host pid ns"
sudo ip netns list | grep -q "^boat$(vmf "$p1" index)" || fail "no netns for p1"
[ "$(cat "/sys/fs/cgroup/boat-e2e/$p1/memory.high")" = $(((2048 + 256) * 1048576)) ] || fail "memory.high: $(cat "/sys/fs/cgroup/boat-e2e/$p1/memory.high")"
[ "$(cat "/sys/fs/cgroup/boat-e2e/$p1/memory.max")" = $(((2048 + 256 + 512) * 1048576)) ] || fail "memory.max: $(cat "/sys/fs/cgroup/boat-e2e/$p1/memory.max")"
grep -q '^max 100000$\|^300000 100000$' "/sys/fs/cgroup/boat-e2e/$p1/cpu.max" || fail "cpu.max: $(cat "/sys/fs/cgroup/boat-e2e/$p1/cpu.max")"
[ "$(stat -c %u "/proc/$pid")" -ge 900000 ] || fail "firecracker not running as a jail uid"
[ "$(sudo cat "/proc/$pid/oom_score_adj")" = 500 ] || fail "oom_score_adj: $(sudo cat "/proc/$pid/oom_score_adj")"

rules=$(sudo nft list table inet boat)
grep -q 'meta skuid 900000-965534 drop' <<<"$rules" || fail "jail uid output rule not loaded"
grep -q 'limit rate over' <<<"$(sudo nft list table bridge boat)" || fail "egress rate rule not loaded"
[ "$(cat "/sys/fs/cgroup/boat-e2e/$p1/pids.max")" = 256 ] || fail "pids.max: $(cat "/sys/fs/cgroup/boat-e2e/$p1/pids.max")"

log "host e2e: isolation"
sudo ip netns add e2e-ext
sudo ip link add e2eh type veth peer name e2ee netns e2e-ext
sudo ip addr add 10.250.0.1/24 dev e2eh && sudo ip -6 addr add fd99::1/64 dev e2eh nodad && sudo ip link set e2eh up
sudo ip -n e2e-ext addr add 10.250.0.2/24 dev e2ee && sudo ip -n e2e-ext -6 addr add fd99::2/64 dev e2ee nodad
sudo ip -n e2e-ext link set e2ee up && sudo ip -n e2e-ext link set lo up
sudo ip -n e2e-ext route add default via 10.250.0.1 && sudo ip -n e2e-ext -6 route add default via fd99::1
# Runner only: docker may leave ip6tables FORWARD at DROP (a NixOS boat host has no ip6tables rules).
if command -v ip6tables >/dev/null; then
  sudo ip6tables -I FORWARD -o boatbr0 -j ACCEPT && sudo ip6tables -I FORWARD -i boatbr0 -j ACCEPT
fi
sudo ip netns exec e2e-ext python3 -m http.server 8000 --bind :: >/dev/null 2>&1 &
python3 -m http.server 8743 --bind 0.0.0.0 >/dev/null 2>&1 &
PIDS+=($!)
sleep 1
x1=$(newvm '{"name":"x1","team":"iso2"}')
ip_c1=$(vmf "$c1" ip) ip_x1=$(vmf "$x1" ip)
curl -fsS --max-time 5 -o /dev/null http://10.250.0.2:8000/ || fail "control: host cannot reach the RFC1918 target"
curl -fsS --max-time 5 -o /dev/null "http://$host_ip:8743/" || fail "control: host listener down"
timeout 5 bash -c "</dev/tcp/$ip_c1/22" || fail "control: host cannot reach c1:22"
bx "$p1" "$(tcp github.com 443)" || fail "control: no internet from p1"
blocked "$p1" "$(tcp "$ip_c1" 22)"   # same team, allowIntraTeam off
blocked "$p1" "$(tcp "$ip_x1" 22)"   # other team
blocked "$x1" "$(tcp "$(vmf "$p1" ip)" 22)"
blocked "$p1" "$(tcp 10.250.0.2 8000)" # RFC1918 behind the host
blocked "$p1" "$(tcp 10.200.0.1 8743)" # host services via the gateway
blocked "$p1" "$(tcp "$host_ip" 8743)" # ... via the host's own address
blocked "$p1" "$(tcp 10.200.0.1 8740)" # boat-api port
blocked "$p1" 'curl -fsS --max-time 4 -H Metadata:true "http://169.254.169.254/metadata/instance?api-version=2021-02-01"'
# Control: the host (not subject to the VM policy) reaches the same SMTP port and
# the VM resolves the name, so a pass below is the egress rule, not the network.
timeout 8 bash -c '</dev/tcp/smtp.gmail.com/587' || fail "control: host cannot reach smtp.gmail.com:587"
bx "$p1" 'getent hosts smtp.gmail.com >/dev/null' || fail "control: p1 cannot resolve smtp.gmail.com"
blocked "$p1" "$(tcp smtp.gmail.com 587)"
blocked "$p1" "$(tcp smtp.gmail.com 465)"

log "host e2e: ipv6"
v6=$(vmf "$p1" ipv6)
[[ $v6 == fd42:b0a7::* ]] || fail "p1 has no ipv6: $v6"
bx "$p1" "ip -6 -o addr show eth0" | grep -q "$v6/64" || fail "guest did not apply $v6"
ping -6 -c 2 -W 2 "$v6" >/dev/null || fail "host cannot ping6 p1"
bx "$p1" 'ping -6 -c 2 -W 2 fd42:b0a7::1 >/dev/null' || fail "p1 cannot ping6 the gateway"
blocked "$p1" "$(tcp "$(vmf "$c1" ipv6)" 22)" # VM<->VM over v6
blocked "$p1" "$(tcp fd99::2 8000)"           # ULA egress
if sudo ip netns exec e2e-ext curl -fsS -g --max-time 4 "http://[$v6]:8081/" >/dev/null 2>&1; then
  fail "unpublished port 8081 reachable over ipv6"
fi
bd POST "/v1/vms/$p1/ports" '{"port":8081,"public":true}' >/dev/null
[ "$(sudo ip netns exec e2e-ext curl -fsS -g --max-time 5 "http://[$v6]:8081/")" = "$tok" ] || fail "published port 8081 not reachable over ipv6"
if sudo ip netns exec e2e-ext timeout 4 bash -c "</dev/tcp/$v6/22" 2>/dev/null; then fail "unpublished port 22 reachable over ipv6"; fi

log "host e2e: idle auto-stop"
for v in "$f1" "$f2" "$x1"; do bd DELETE "/v1/vms/$v" >/dev/null; done
i1=$(newvm '{"name":"i1","team":"iso2","idle_stop_s":15}')
for _ in $(seq 60); do [ "$(vmf "$i1" state)" = stopped ] && break; sleep 2; done
[ "$(vmf "$i1" state)" = stopped ] || fail "idle VM still $(vmf "$i1" state) after 120s"
bd DELETE "/v1/vms/$i1" >/dev/null

log "host e2e: boatd restart re-adopts running VMs"
pid_before=$(vmf "$p1" pid)
restart_boatd "$WORK/boatd.json"
[ "$(vmf "$p1" state) $(vmf "$p1" pid)" = "running $pid_before" ] || fail "p1 not re-adopted: $(bd GET "/v1/vms/$p1")"
[ "$(bx "$p1" 'curl -fsS http://localhost:8081/')" = "$tok" ] || fail "p1 unusable after the boatd restart"
[ "$(bx "$c1" hostname)" = c1 ] || fail "c1 unusable after the boatd restart"

log "host e2e: free-space floor"
free_gib=$(($(df -B1G --output=avail "$STATE" | tail -1)))
jq --argjson g $((free_gib + 1)) '.minFreeGiB = $g | .minFreePct = 0' "$WORK/boatd.json" >"$WORK/boatd-floor.json"
restart_boatd "$WORK/boatd-floor.json"
[ "$(bdcode POST /v1/vms '{"name":"fl","team":"iso2"}')" = 507 ] || fail "create below the floor: $(cat "$WORK/tmp/bd.out")"
[ "$(bdcode POST "/v1/vms/$c1/snapshots" '{}')" = 507 ] || fail "snapshot below the floor: $(cat "$WORK/tmp/bd.out")"
[ "$(bdcode POST "/v1/vms/$c1/fork" '{}')" = 507 ] || fail "fork below the floor"
st=$(bd POST "/v1/vms/$c1/stop" | jq -r '.state + " " + (.has_snapshot|tostring)')
[ "$st" = "stopped false" ] || fail "stop below the floor should shut down cold: $st"
restart_boatd "$WORK/boatd.json"
bd GET "/v1/usage?team=iso1" | jq -c '.storage'
bd GET /v1/usage | jq -e '.disk.free_bytes > 0 and .disk.floor_bytes > 0' >/dev/null || fail "usage disk bytes"
bd DELETE "/v1/vms/$c1" >/dev/null

log "host e2e: docker in the full image"
dk=$(newvm '{"name":"dk","team":"iso2","image":"full"}')
bx "$dk" 'for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done; docker run --rm hello-world' | grep -q 'Hello from Docker!' ||
  fail "docker hello-world"
bd DELETE "/v1/vms/$dk" >/dev/null
bd DELETE "/v1/vms/$p1" >/dev/null

# shellcheck source=boatd/e2e-limits.sh
. "$HERE/boatd/e2e-limits.sh"

log "host e2e: btrfs qgroups"
jq '.btrfsQgroups = true' "$WORK/boatd.json" >"$WORK/boatd-q.json"
restart_boatd "$WORK/boatd-q.json"
q1=$(newvm '{"name":"q1","team":"iso2"}')
[ "$(vmf "$q1" agent_ready)" = true ] || fail "q1 not ready: $(vmf "$q1" error)"
qid=$(sudo btrfs subvolume show "$STATE/vms/$q1" | awk '/Subvolume ID:/ {print $3}')
[ -n "$qid" ] || fail "q1's VM dir is not a subvolume"
want=$(((8 << 30) + 2 * (2048 << 20) + (1 << 30))) # disk + 2 x RAM + 1 GiB
sudo btrfs qgroup show -e --raw "$STATE" | grep "^0/$qid " | grep -qw "$want" || fail "qgroup limit: $(sudo btrfs qgroup show -e --raw "$STATE")"
t=$(now); q2=$(bd POST "/v1/vms/$q1/fork" '{"name":"q2"}' | jq -r .id); forkq_s=$(secs "$t" "$(now)") # a reflink across subvolumes
[ "$(bx "$q2" hostname)" = q2 ] || fail "fork across subvolumes"
echo "fork with qgroups ${forkq_s}s"
bd DELETE "/v1/vms/$q2" >/dev/null
bd DELETE "/v1/vms/$q1" >/dev/null
[ -z "$(sudo ls "$STATE/vms")" ] || fail "subvolumes left: $(sudo ls "$STATE/vms")"
sudo pkill -f 'http.server 8000' || true
sudo ip netns del e2e-ext
sudo ip link del e2eh 2>/dev/null || true
[ "$(bd GET /v1/vms | jq '.vms | length')" = 0 ] || fail "host e2e left VMs"
[ -z "$(sudo ip netns list | grep '^boat[0-9]')" ] || fail "netns left: $(sudo ip netns list)"
