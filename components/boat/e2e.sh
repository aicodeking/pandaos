#!/usr/bin/env bash
# Full-stack boat e2e on a Linux x86_64 host with /dev/kvm (GitHub ubuntu
# runners qualify). boatd runs as root, boat-api and boat-gateway as the
# calling user, all as plain processes (no NixOS). Every step drives the real
# `boat` CLI. It changes the host network (bridge boatbr0, taps bt*, nft
# tables "boat") and removes all of it on exit.
#
#   ./e2e.sh GUEST_DIR     GUEST_DIR has vmlinux, rootfs-slim.ext4, rootfs-full.ext4
#
# Env: E2E_WORK (/mnt/boat-e2e), FC_VERSION (v1.17.0), E2E_JAILER (1).
# Prints `METRIC {json}` (timings, image sizes) and exits non-zero on failure.
set -euo pipefail

GUEST=$(cd "${1:?usage: e2e.sh GUEST_DIR}" && pwd)
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=${E2E_WORK:-/mnt/boat-e2e}
STATE=/var/lib/boat-e2e
FC_VERSION=${FC_VERSION:-v1.17.0}
FC_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558 # v1.17.0 x86_64 tgz
JAILER=${E2E_JAILER:-1}
SOCK=$WORK/run/boatd.sock
API=http://127.0.0.1:8740
DOMAIN=boat.test
HOOK_PORT=9911
MAX_VMS=4 # team quota (defaultTeam.maxVMs)

[ -e /dev/kvm ] || { echo "e2e: /dev/kvm missing" >&2; exit 1; }
[ "$(id -u)" != 0 ] || { echo "e2e: run as a normal user with sudo (boat-api/CLI are unprivileged)" >&2; exit 1; }

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { echo "e2e FAIL: $*" >&2; exit 1; }
now() { date +%s.%N; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }

PIDS=()
cleanup() {
  set +e
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  sudo pkill -9 -x firecracker # first: else boatd's SIGTERM handler snapshots every VM
  if [ -f "$WORK/boatd.pid" ]; then
    sudo kill "$(cat "$WORK/boatd.pid")" 2>/dev/null
    for _ in $(seq 100); do sudo kill -0 "$(cat "$WORK/boatd.pid")" 2>/dev/null || break; sleep 0.2; done
  fi
  sudo pkill -9 -x firecracker # -f would match this sudo itself
  sleep 0.5
  for t in $(ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^bt[0-9]+$'); do sudo ip link del "$t"; done
  sudo ip link del boatbr0 2>/dev/null
  sudo nft delete table inet boat 2>/dev/null
  sudo nft delete table bridge boat 2>/dev/null
  for n in $(ip netns list 2>/dev/null | awk '/^boat[0-9]|^e2e-ext/{print $1}'); do sudo ip netns del "$n"; done
  sudo ip link del e2eh 2>/dev/null
  sudo pkill -f 'http.server 8000'
  sudo iptables -D FORWARD -i boatbr0 -j ACCEPT 2>/dev/null
  sudo iptables -D FORWARD -o boatbr0 -j ACCEPT 2>/dev/null
  sudo sh -c 'for f in "$1"/vms/*/firecracker.log; do [ -f "$f" ] && { echo "--- $f (tail)"; tail -n 30 "$f"; }; done' _ "$STATE"
  mountpoint -q "$STATE" && sudo umount -l "$STATE"
  sudo rmdir /sys/fs/cgroup/boat-e2e 2>/dev/null
  for f in boatd api gateway hooks; do
    [ -f "$WORK/$f.log" ] && { echo "--- $f.log (tail)"; tail -n 60 "$WORK/$f.log"; }
  done
}
trap cleanup EXIT

sudo mkdir -p "$WORK" && sudo chown "$(id -u):$(id -g)" "$WORK"
mkdir -p "$WORK/bin" "$WORK/dl" "$WORK/run" "$WORK/api" "$WORK/tmp"

log "tools"
missing=()
for c in jq:jq nft:nftables mkfs.btrfs:btrfs-progs curl:curl ssh:openssh-client ssh-keyscan:openssh-client conntrack:conntrack; do
  command -v "${c%%:*}" >/dev/null || missing+=("${c#*:}")
done
[ ${#missing[@]} = 0 ] || { sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null; }
# br_netfilter would push bridged frames through the inet hooks (PROTOCOL.md).
# Runner only: docker loads it and sets iptables FORWARD to DROP; a NixOS
# boat host has neither. Neutralise both for boatbr0 so VM egress works.
if lsmod | grep -q '^br_netfilter'; then
  sudo modprobe -r br_netfilter 2>/dev/null ||
    sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0 net.bridge.bridge-nf-call-ip6tables=0 net.bridge.bridge-nf-call-arptables=0
fi
if command -v iptables >/dev/null; then
  sudo iptables -I FORWARD -i boatbr0 -j ACCEPT
  sudo iptables -I FORWARD -o boatbr0 -j ACCEPT
fi

log "firecracker $FC_VERSION"
if [ ! -x "$WORK/dl/firecracker" ]; then
  curl -fsSLo "$WORK/dl/fc.tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
  echo "$FC_SHA256  $WORK/dl/fc.tgz" | sha256sum -c -
  tar -xzf "$WORK/dl/fc.tgz" -C "$WORK/dl"
  mv "$WORK/dl/release-$FC_VERSION-x86_64/firecracker-$FC_VERSION-x86_64" "$WORK/dl/firecracker"
  mv "$WORK/dl/release-$FC_VERSION-x86_64/jailer-$FC_VERSION-x86_64" "$WORK/dl/jailer"
fi

log "go build boatd, boat-api, boat-gateway, boat"
(cd "$HERE/boatd" && CGO_ENABLED=0 go build -o "$WORK/bin/boatd" .)
(cd "$HERE/api" && CGO_ENABLED=0 go build -o "$WORK/bin/boat-api" .)
(cd "$HERE/gateway" && CGO_ENABLED=0 go build -o "$WORK/bin/boat-gateway" .)
(cd "$HERE/cli" && CGO_ENABLED=0 go build -o "$WORK/bin/boat" .)
export PATH="$WORK/bin:$PATH"

log "state dir: btrfs (reflink like production) + images"
[ -f "$WORK/state.img" ] || { truncate -s 60G "$WORK/state.img"; mkfs.btrfs -q "$WORK/state.img"; }
sudo mkdir -p "$STATE"
mountpoint -q "$STATE" || sudo mount -o loop "$WORK/state.img" "$STATE"
sudo rm -rf "$STATE/vms" "$STATE/snapshots" "$STATE/usage.jsonl"
for v in slim full; do
  sudo mkdir -p "$STATE/images/$v"
  sudo cp --sparse=always "$GUEST/vmlinux" "$STATE/images/$v/vmlinux"
  sudo cp --sparse=always "$GUEST/rootfs-$v.ext4" "$STATE/images/$v/rootfs.ext4"
done

log "boatd (root)"
jailer='""' jargs='[]'
if [ "$JAILER" = 1 ]; then
  sudo mkdir -p /sys/fs/cgroup/boat-e2e
  jailer="\"$WORK/dl/jailer\""
fi
cat >"$WORK/boatd.json" <<JSON
{"stateDir":"$STATE","runDir":"$WORK/run","socket":"$SOCK","socketGroup":"$(id -gn)",
 "firecracker":"$WORK/dl/firecracker","jailer":$jailer,"jailerArgs":$jargs,"cgroupParent":"boat-e2e","reflink":"always","btrfsQgroups":true,
 "bootTimeoutS":90,"defaultImage":"slim","defaultSize":"e2e","ipv6Prefix":"fd42:b0a7::/64","reapIntervalS":5,
 "sizes":{"e2e":{"vcpu":2,"mem_mib":2048,"disk_gib":8}},
 "defaultTeam":{"maxVMs":$MAX_VMS}}
JSON
sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >"$4" 2>&1' _ \
  "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.json" "$WORK/boatd.log" &
for _ in $(seq 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
[ -S "$SOCK" ] || fail "boatd did not start"
curl -fsS --unix-socket "$SOCK" http://boatd/v1/health >/dev/null || fail "boatd socket not usable by $(id -un)"

log "boat-api + boat-gateway ($(id -un)), bootstrap admin key"
head -c 32 /dev/urandom >"$WORK/desktop.key" && chmod 600 "$WORK/desktop.key"
KEY=$(boat-api admin init -data "$WORK/api" -user e2e -team e2e | jq -r .token)
[[ $KEY == boat_* ]] || fail "admin init: $KEY"
boat-api serve -data "$WORK/api" -listen 127.0.0.1:8740 -boatd "$SOCK" -rate 0 \
  -images slim,full -allow-private-webhooks -ssh-user dev \
  -desktop-url "http://desktop.$DOMAIN:8742" -desktop-key-file "$WORK/desktop.key" >"$WORK/api.log" 2>&1 &
PIDS+=($!)
boat-gateway -listen 127.0.0.1:8742 -ask-listen "" -domain "$DOMAIN" \
  -routes "$WORK/run/routes.json" -desktop-key "$WORK/desktop.key" >"$WORK/gateway.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -fs "$API/healthz" >/dev/null && break; sleep 0.1; done

export BOAT_CONFIG=$WORK/cli.json
boat login --url "$API" --key "$KEY" >/dev/null
boat whoami
team=$(boat whoami --json | jq -r .team.id)
[ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N "" -f ~/.ssh/id_ed25519

# diag NAME IP: guest + host network/ssh state, for a failed connectivity check
diag() {
  set +e
  echo "--- diag $1 ($2)"
  boat exec "$1" -- sudo bash -c 'systemctl --no-pager status ssh | head -20; ss -ltnp; ip addr; ip route;
    ls -la /home/dev/.ssh; cat /home/dev/.ssh/authorized_keys; ls -l /etc/ssh/ssh_host_*; journalctl -b --no-pager -u ssh | tail -20'
  ping -c 2 -W 1 "$2"; ip -br addr; ip neigh; sudo nft list ruleset | head -80
  ssh -vv -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=3 "dev@$2" true 2>&1 | tail -25
  set -e
}

# vm_json NAME -> the VM object from `boat list`
vm_json() { boat list --json | jq -c --arg n "$1" 'select(.name == $n)'; }
field() { vm_json "$1" | jq -r ".$2"; }
# run NAME CMD -> stdout of `bash -lc CMD` in the VM (exit status checked by set -e)
run() { boat exec "$1" -- bash -c "$2"; }
api_err_status() { jq -r '.error.status' <<<"$1" 2>/dev/null; }

log "webhook listener (HMAC checked with the Python SDK's verify_webhook)"
hook=$(boat webhooks create "http://127.0.0.1:$HOOK_PORT/hook" --events 'vm.*' --json)
hook_id=$(jq -r .id <<<"$hook")
SDK=$HERE/sdk/python HOOK_SECRET=$(jq -r .secret <<<"$hook") HOOK_PORT=$HOOK_PORT HOOK_OUT=$WORK/hooks.jsonl \
  python3 -c '
import json, os, sys
sys.path.insert(0, os.environ["SDK"])
from http.server import BaseHTTPRequestHandler, HTTPServer
from boat import verify_webhook
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        ok = verify_webhook(os.environ["HOOK_SECRET"], self.headers.get("X-Boat-Signature", ""), body)
        ev = json.loads(body)
        with open(os.environ["HOOK_OUT"], "a") as f:
            f.write(json.dumps({"valid": ok, "header_event": self.headers.get("X-Boat-Event"), "event": ev}) + "\n")
        self.send_response(204 if ok else 401)
        self.end_headers()
HTTPServer(("127.0.0.1", int(os.environ["HOOK_PORT"])), H).serve_forever()
' >"$WORK/hooks.log" 2>&1 &
PIDS+=($!)

log "secret (set before create: injected into the VM env)"
boat secrets set E2E_SECRET 's3cr3t value' >/dev/null

log "new -> running"
t=$(now)
vm=$(boat new --name e2e-a --json)
create_s=$(secs "$t" "$(now)")
id_a=$(jq -r .id <<<"$vm") ip_a=$(jq -r .ip <<<"$vm")
[ "$(jq -r '.state + " " + (.agent_ready|tostring)' <<<"$vm")" = "running true" ] || fail "new: $vm"
ssh_ok() { ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 "dev@$1" true; }
boat ssh e2e-a --json >/dev/null # authorizes ~/.ssh/id_ed25519.pub for dev
for _ in $(seq 300); do ssh_ok "$ip_a" && break; sleep 0.2; done
ssh_ok "$ip_a" || { diag e2e-a "$ip_a"; fail "ssh dev@$ip_a"; }
ssh_ready_s=$(secs "$t" "$(now)")
echo "create ${create_s}s, ssh-ready ${ssh_ready_s}s"

log "exec uname"
run e2e-a 'uname -a' | tee /dev/stderr | grep -q '^Linux e2e-a ' || fail "exec uname"

log "VM egress (NAT) and DNS"
run e2e-a 'curl -fsS -o /dev/null --max-time 20 https://github.com' || fail "VM has no internet"

log "secrets in the exec env"
[ "$(run e2e-a 'printf %s "$E2E_SECRET"')" = 's3cr3t value' ] || fail "secret not in exec env"

log "ssh via the returned endpoint (boat ssh)"
info=$(boat ssh e2e-a --json)
[ "$(jq -r '.host + " " + (.port|tostring) + " " + .user' <<<"$info")" = "$ip_a 22 dev" ] || fail "ssh endpoint: $info"
out=$(boat ssh e2e-a -- hostname </dev/null) # the CLI execs ssh with the returned argv
[ "$out" = e2e-a ] || fail "ssh hostname: $out"

log "files: put / get"
head -c 300000 /dev/urandom >"$WORK/tmp/blob"
echo "marker-$RANDOM" >"$WORK/tmp/marker"
boat scp "$WORK/tmp/blob" e2e-a:/home/dev/blob >/dev/null
boat scp "$WORK/tmp/marker" e2e-a:/home/dev/marker >/dev/null
boat scp e2e-a:/home/dev/blob "$WORK/tmp/blob.back" >/dev/null
cmp "$WORK/tmp/blob" "$WORK/tmp/blob.back" || fail "file round trip"
[ "$(run e2e-a 'stat -c %U /home/dev/marker')" = dev ] || fail "put_file owner"

log "ports add + curl a guest http server"
run e2e-a 'mkdir -p /tmp/www && echo port-ok >/tmp/www/index.html && setsid nohup python3 -m http.server 8080 -d /tmp/www >/dev/null 2>&1 </dev/null & sleep 1'
port=$(boat ports add e2e-a 8080 --public --json)
hp=$(jq -r .host_port <<<"$port") hn=$(jq -r .hostname <<<"$port")
[ "$hn" = "8080-$id_a" ] && [ "$hp" -ge 20000 ] || fail "ports add: $port"
host_ip=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
[ "$(curl -fsS --max-time 10 "http://$host_ip:$hp/")" = port-ok ] || fail "public port $host_ip:$hp"
[ "$(curl -fsS --max-time 10 -H "Host: $hn.vm.$DOMAIN" http://127.0.0.1:8742/)" = port-ok ] || fail "gateway route $hn"
boat ports ls e2e-a --json | jq -e 'select(.guest_port == 8080)' >/dev/null || fail "ports ls"

log "stop -> start (resume; disk and memory preserved)"
boot_id=$(run e2e-a 'cat /proc/sys/kernel/random/boot_id')
t=$(now)
st=$(boat stop e2e-a --json | jq -r .state)
stop_s=$(secs "$t" "$(now)")
[ "$st" = stopped ] && [ "$(field e2e-a has_snapshot)" = true ] || fail "stop: $st"
t=$(now)
st=$(boat start e2e-a --json | jq -r .state)
resume_s=$(secs "$t" "$(now)")
[ "$st" = running ] || fail "start: $st"
[ "$(run e2e-a 'cat /home/dev/marker')" = "$(cat "$WORK/tmp/marker")" ] || fail "file lost across stop/start"
[ "$(run e2e-a 'cat /proc/sys/kernel/random/boot_id')" = "$boot_id" ] || fail "start cold-booted instead of resuming"
[ "$(curl -fsS --max-time 10 "http://$host_ip:$hp/")" = port-ok ] || fail "guest http server lost across stop/start"
echo "stop ${stop_s}s, resume ${resume_s}s"

log "fork"
t=$(now)
child=$(boat fork e2e-a --name e2e-b --json)
fork_s=$(secs "$t" "$(now)")
ip_b=$(jq -r .ip <<<"$child")
[ "$(jq -r '.state + " " + (.agent_ready|tostring)' <<<"$child")" = "running true" ] && [ "$ip_b" != "$ip_a" ] || fail "fork: $child"
[ "$(run e2e-b 'cat /home/dev/marker')" = "$(cat "$WORK/tmp/marker")" ] || fail "fork lost the file"
[ "$(run e2e-b hostname)" = e2e-b ] && [ "$(run e2e-a hostname)" = e2e-a ] || fail "fork hostname"
run e2e-b 'ip -4 -o addr show eth0' | grep -q " $ip_b/" || fail "fork not re-IPed"
[ "$(run e2e-b 'cat /etc/machine-id')" != "$(run e2e-a 'cat /etc/machine-id')" ] || fail "fork kept machine-id"
for _ in $(seq 100); do ssh_ok "$ip_b" && break; sleep 0.2; done
hk_a=$(ssh-keyscan -T 5 -t ed25519 "$ip_a" 2>/dev/null | awk '{print $3}')
hk_b=$(ssh-keyscan -T 5 -t ed25519 "$ip_b" 2>/dev/null | awk '{print $3}')
[ -n "$hk_a" ] && [ -n "$hk_b" ] && [ "$hk_a" != "$hk_b" ] || fail "ssh host keys: a=$hk_a b=$hk_b"
ssh_ok "$ip_a" && ssh_ok "$ip_b" || fail "parent and fork not both reachable"
[ "$(boat list --json | jq -s '[.[] | select(.state == "running")] | length')" = 2 ] || fail "parent+fork not both running"
echo "fork ${fork_s}s"

log "snapshot save + new from snapshot"
snap=$(boat snapshots e2e-a create --name s1 --json)
sid=$(jq -r .id <<<"$snap")
[ -n "$sid" ] && [ "$sid" != null ] || fail "snapshot: $snap"
boat snapshots e2e-a ls --json | jq -e --arg s "$sid" 'select(.id == $s)' >/dev/null || fail "snapshots ls"
run e2e-a 'echo after-snapshot >/home/dev/later'
t=$(now)
c=$(boat new --snapshot "$sid" --name e2e-c --json)
snap_new_s=$(secs "$t" "$(now)")
[ "$(jq -r .state <<<"$c")" = running ] || fail "new --snapshot: $c"
[ "$(run e2e-c 'cat /home/dev/marker')" = "$(cat "$WORK/tmp/marker")" ] || fail "snapshot VM lost the file"
run e2e-c 'test ! -e /home/dev/later' || fail "snapshot VM has a file written after the snapshot"
[ "$(run e2e-c hostname)" = e2e-c ] || fail "snapshot VM hostname"

log "prompt with a stub agent (fake claude on dev's PATH)"
printf '#!/bin/sh\necho "STUB-CLAUDE $*"\necho "secret=$E2E_SECRET"\n' >"$WORK/tmp/claude"
boat scp "$WORK/tmp/claude" e2e-a:/home/dev/.local/bin/claude >/dev/null
run e2e-a 'chmod +x ~/.local/bin/claude'
out=$(boat prompt e2e-a "say hi to the e2e")
grep -qx 'STUB-CLAUDE -p --dangerously-skip-permissions -- say hi to the e2e' <<<"$out" || fail "prompt argv: $out"
grep -qx 'secret=s3cr3t value' <<<"$out" || fail "prompt env: $out"

log "desktop (full image) over HTTP via boat-gateway"
t=$(now)
boat new --name e2e-d --image full --json >/dev/null
id_d=$(field e2e-d id)
d=$(boat desktop e2e-d --json)
desktop_s=$(secs "$t" "$(now)")
u=$(jq -r .url <<<"$d")
[[ $u == "http://"*"$id_d"*"exp="* ]] || fail "desktop url: $d"
gw=(--connect-to "::127.0.0.1:8742" --max-time 20) # any host -> the gateway
# Follow the link like a browser: the signed link redirects to the VM's own
# desktop origin (<id>.desktop.D), which sets the session cookie.
hdr=$(curl -sS -L -D - -o /dev/null "${gw[@]}" "$u" || true)
cookie=$( (grep -iE '^set-cookie: (__Host-)?boat_gw=' <<<"$hdr" || true) | tail -1 | sed 's/^[^:]*: //; s/;.*//' | tr -d '\r')
[ -n "$cookie" ] || fail "desktop link gave no cookie: $hdr"
# The cookie is Secure (Caddy serves https in production); send it by hand over http.
desk=""
for cand in "http://$id_d.desktop.$DOMAIN:8742/" "http://desktop.$DOMAIN:8742/$id_d/"; do
  code=$(curl -sS -o "$WORK/tmp/desktop.html" -w '%{http_code}' -b "$cookie" "${gw[@]}" "$cand")
  if [ "$code" = 200 ] && grep -qi '<html' "$WORK/tmp/desktop.html"; then desk=$cand; break; fi
done
[ -n "$desk" ] || fail "desktop page: HTTP $code $(head -c 300 "$WORK/tmp/desktop.html")"
[ "$(curl -s -o /dev/null -w '%{http_code}' "${gw[@]}" "$desk")" != 200 ] || fail "desktop open without the cookie"
echo "desktop ${desktop_s}s (create + desktop start)"

log "quota: $MAX_VMS VMs active, the next create is refused"
[ "$(boat list --json | jq -s '[.[] | select(.state != "stopped" and .state != "error")] | length')" = "$MAX_VMS" ] || fail "expected $MAX_VMS active VMs"
set +e
err=$(boat new --name e2e-over --json 2>&1 >/dev/null)
rc=$?
set -e
[ "$rc" = 1 ] && [ "$(api_err_status "$err")" = 429 ] || fail "quota not enforced: rc=$rc $err"
[ -z "$(vm_json e2e-over)" ] || fail "over-quota VM exists"

log "rm"
for n in e2e-d e2e-c e2e-b; do boat rm "$n" --json >/dev/null; done
[ "$(boat list --json | jq -s length)" = 1 ] || fail "rm left VMs"

log "TTL auto-stop"
boat new --name e2e-t --ttl 20s --json >/dev/null
id_t=$(field e2e-t id)
for _ in $(seq 60); do [ "$(field e2e-t state)" = stopped ] && break; sleep 2; done
[ "$(field e2e-t state)" = stopped ] || fail "TTL VM still $(field e2e-t state) after 120s"

log "webhooks: signed deliveries"
for _ in $(seq 30); do
  jq -e --arg v "$id_t" 'select(.event.data.vm_id == $v and .event.data.reason == "ttl")' "$WORK/hooks.jsonl" >/dev/null 2>&1 && break
  sleep 1
done
n=$(jq -s 'length' "$WORK/hooks.jsonl")
[ "$n" -gt 0 ] || fail "no webhook delivered"
[ "$(jq -s 'map(select(.valid | not)) | length' "$WORK/hooks.jsonl")" = 0 ] || fail "webhook with a bad HMAC"
jq -e --arg v "$id_t" 'select(.event.data.vm_id == $v and .event.data.reason == "ttl")' "$WORK/hooks.jsonl" >/dev/null ||
  fail "no ttl stop webhook for $id_t"
jq -e --arg t "$team" 'select(.event.team_id == $t and .header_event == .event.type)' "$WORK/hooks.jsonl" >/dev/null || fail "webhook team/type"
boat webhooks deliveries "$hook_id" --json | jq -e 'select(.ok == true)' >/dev/null || fail "delivery log"
echo "$n webhooks, all HMAC-valid"

log "usage metering"
usage=$(boat usage --json)
echo "$usage" | jq -c .teams
vm_seconds=$(jq -r --arg t "$team" '.teams[] | select(.team == $t) | .vm_seconds' <<<"$usage")
awk -v s="$vm_seconds" 'BEGIN { exit !(s > 0) }' || fail "usage vm_seconds not > 0: $usage"

boat rm e2e-t --json >/dev/null
boat rm e2e-a --json >/dev/null
[ "$(boat list --json | jq -s length)" = 0 ] || fail "VMs left"
[ -z "$(sudo ls "$STATE/vms")" ] || fail "VM dirs left"

# boatd host-level phases: perf, fork memory/entropy, jail, isolation, ipv6,
# idle stop, restart reconcile, disk floor, docker.
# shellcheck source=boatd/e2e-host.sh
. "$HERE/boatd/e2e-host.sh"

mib() { echo $(( $(stat -c %s "$1") / 1048576 )); }
zst() { if [ -f "$1" ]; then mib "$1"; else echo null; fi; }
echo "METRIC $(jq -nc \
  --argjson c "$create_s" --argjson s "$ssh_ready_s" --argjson f "$fork_s" --argjson st "$stop_s" \
  --argjson r "$resume_s" --argjson sn "$snap_new_s" --argjson d "$desktop_s" \
  --argjson k "$(mib "$GUEST/vmlinux")" --argjson sl "$(mib "$GUEST/rootfs-slim.ext4")" \
  --argjson fu "$(mib "$GUEST/rootfs-full.ext4")" --argjson slz "$(zst "$GUEST/rootfs-slim.ext4.zst")" \
  --argjson fuz "$(zst "$GUEST/rootfs-full.ext4.zst")" --argjson vs "$vm_seconds" \
  --argjson hs1 "$stop2_s" --argjson hs2 "$stop3_s" --argjson hr "$resume2_s" --argjson hf "$fork2_s" \
  '{test: "boat-e2e", createReadyS: $c, sshReadyS: $s, forkS: $f, stopS: $st, resumeS: $r,
    newFromSnapshotS: $sn, desktopS: $d, kernelMB: $k, slimImageMB: $sl, fullImageMB: $fu,
    slimZstMB: $slz, fullZstMB: $fuz, usageVmSeconds: $vs,
    hostStopFirstS: $hs1, hostStopDiffS: $hs2, hostResumeS: $hr, hostForkS: $hf}')"
echo "e2e: PASS"
