#!/usr/bin/env bash
# boatd end-to-end test on a REAL Linux x86_64 host with /dev/kvm (GitHub
# ubuntu-24.04 runners qualify). It needs root and changes the host network:
# it adds bridge boatbr0, taps bt*, and nft tables "boat". Everything is
# removed on exit.
#
#   create -> ssh-ready -> exec -> stop -> start (resume) -> fork -> exec in fork -> delete
#
# It prints timings and fails if they exceed the limits below.
# Env:
#   FC_VERSION (v1.12.1)  E2E_JAILER (1)  E2E_WORK (/tmp/boat-e2e)
#   BOAT_AGENT (../guest/agent/boat-agent.py)
#   E2E_CREATE_SSH_MAX (30)  E2E_FORK_MAX (10)  E2E_RESUME_MAX (5)  [seconds]
set -euo pipefail

[ "$(id -u)" = 0 ] || exec sudo -E env PATH="$PATH" bash "$0" "$@"
[ -e /dev/kvm ] || { echo "e2e: /dev/kvm missing" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || { echo "e2e: x86_64 only" >&2; exit 1; }

HERE=$(cd "$(dirname "$0")" && pwd)
FC_VERSION=${FC_VERSION:-v1.12.1}
CI_VERSION=${FC_VERSION%.*}
WORK=${E2E_WORK:-/tmp/boat-e2e}
STATE=/var/lib/boat-e2e
SOCK=$WORK/run/boatd.sock
JAILER=${E2E_JAILER:-1}
AGENT=${BOAT_AGENT:-$HERE/../guest/agent/boat-agent.py}
CREATE_SSH_MAX=${E2E_CREATE_SSH_MAX:-30}
FORK_MAX=${E2E_FORK_MAX:-10}
RESUME_MAX=${E2E_RESUME_MAX:-5}
S3=https://s3.amazonaws.com/spec.ccfc.min

need() { command -v "$1" >/dev/null || { apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$2" >/dev/null; }; }
need jq jq; need unsquashfs squashfs-tools; need nft nftables; need mkfs.ext4 e2fsprogs; need curl curl
need mkfs.btrfs btrfs-progs || true

BOATD_PID=
cleanup() {
  set +e
  [ -n "$BOATD_PID" ] && kill "$BOATD_PID" && wait "$BOATD_PID" 2>/dev/null
  pkill -9 -f "firecracker --id vm-" ; sleep 0.5
  for t in $(ip -o link show | awk -F': ' '{print $2}' | grep -E '^bt[0-9]+' | cut -d@ -f1); do ip link del "$t"; done
  ip link del boatbr0 2>/dev/null
  nft delete table inet boat 2>/dev/null; nft delete table bridge boat 2>/dev/null
  mountpoint -q "$STATE" && umount "$STATE"
  rmdir /sys/fs/cgroup/boat-e2e/* /sys/fs/cgroup/boat-e2e 2>/dev/null
  [ -f "$WORK/boatd.log" ] && { echo "--- boatd.log (tail)"; tail -n 40 "$WORK/boatd.log"; }
}
trap cleanup EXIT

mkdir -p "$WORK/dl" "$WORK/run"
cd "$WORK/dl"

# --- pinned firecracker + jailer
if [ ! -x firecracker ]; then
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz" | tar xz
  mv "release-$FC_VERSION-x86_64/firecracker-$FC_VERSION-x86_64" firecracker
  mv "release-$FC_VERSION-x86_64/jailer-$FC_VERSION-x86_64" jailer
fi

# --- Firecracker CI quickstart kernel + Ubuntu rootfs (same artifacts as the getting-started guide)
if [ ! -f vmlinux ]; then
  key=$(curl -fsSL "$S3/?prefix=firecracker-ci/$CI_VERSION/x86_64/vmlinux-&list-type=2" \
    | grep -oP "(?<=<Key>)firecracker-ci/$CI_VERSION/x86_64/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3}(?=</Key>)" | sort -V | tail -1)
  curl -fsSL -o vmlinux "$S3/$key"
fi
if [ ! -f rootfs.ext4 ]; then
  key=$(curl -fsSL "$S3/?prefix=firecracker-ci/$CI_VERSION/x86_64/ubuntu-&list-type=2" \
    | grep -oP "(?<=<Key>)firecracker-ci/$CI_VERSION/x86_64/ubuntu-[0-9]+\.[0-9]+\.squashfs(?=</Key>)" | sort -V | tail -1)
  curl -fsSL -o rootfs.squashfs "$S3/$key"
  rm -rf squashfs-root && unsquashfs -q rootfs.squashfs
  [ -f id_ed25519 ] || ssh-keygen -q -t ed25519 -N "" -f id_ed25519
  mkdir -p squashfs-root/root/.ssh && cp id_ed25519.pub squashfs-root/root/.ssh/authorized_keys
  # guest agent: the real one when the rootfs has a full python3, else the Go stand-in
  if [ -f "$AGENT" ] && ls squashfs-root/usr/lib/python3*/json/__init__.py >/dev/null 2>&1; then
    install -m 0755 "$AGENT" squashfs-root/usr/local/bin/boat-agent
    exec_start="/usr/bin/python3 /usr/local/bin/boat-agent"; echo "e2e: guest agent = boat-agent.py"
  else
    (cd "$HERE" && CGO_ENABLED=0 GOOS=linux go build -o "$WORK/dl/squashfs-root/usr/local/bin/boat-agent" ./e2e/testagent)
    exec_start=/usr/local/bin/boat-agent; echo "e2e: guest agent = e2e/testagent (no python3 json in rootfs)"
  fi
  cat > squashfs-root/etc/systemd/system/boat-agent.service <<UNIT
[Unit]
Description=boat guest agent
[Service]
Environment=BOAT_EXEC_USER=root
ExecStart=$exec_start
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
  ln -sf /etc/systemd/system/boat-agent.service squashfs-root/etc/systemd/system/multi-user.target.wants/boat-agent.service
  chown -R root:root squashfs-root
  truncate -s 1G rootfs.ext4 && mkfs.ext4 -q -d squashfs-root -F rootfs.ext4
fi

# --- state dir on btrfs (reflink copies like production), else plain copies
mkdir -p "$STATE"
REFLINK=auto
if command -v mkfs.btrfs >/dev/null; then
  [ -f "$WORK/state.img" ] || { truncate -s 40G "$WORK/state.img"; mkfs.btrfs -q "$WORK/state.img"; }
  mountpoint -q "$STATE" || mount -o loop "$WORK/state.img" "$STATE"
  REFLINK=always
fi
rm -rf "$STATE/vms" "$STATE/snapshots" "$STATE/usage.jsonl"
mkdir -p "$STATE/images/e2e"
cp --reflink=auto "$WORK/dl/vmlinux" "$WORK/dl/rootfs.ext4" "$STATE/images/e2e/"

# --- boatd
(cd "$HERE" && go build -o "$WORK/boatd" .)
jailer_json='""'; jargs='[]'
if [ "$JAILER" = 1 ]; then
  mkdir -p /sys/fs/cgroup/boat-e2e
  jailer_json="\"$WORK/dl/jailer\""; jargs='["--parent-cgroup","boat-e2e"]'
fi
cat > "$WORK/boatd.json" <<JSON
{"stateDir":"$STATE","runDir":"$WORK/run","socket":"$SOCK","firecracker":"$WORK/dl/firecracker",
 "jailer":$jailer_json,"jailerArgs":$jargs,"reflink":"$REFLINK","bootTimeoutS":60,
 "sizes":{"e2e":{"vcpu":2,"mem_mib":1024,"disk_gib":4}},"defaultSize":"e2e","defaultImage":"e2e"}
JSON
"$WORK/boatd" -config "$WORK/boatd.json" >"$WORK/boatd.log" 2>&1 &
BOATD_PID=$!
for _ in $(seq 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
[ -S "$SOCK" ] || { echo "e2e: boatd did not start" >&2; exit 1; }

api() { curl -fsS --unix-socket "$SOCK" -X "$1" "http://boatd$2" -H 'content-type: application/json' ${3:+-d "$3"}; }
now() { date +%s.%N; }
secs() { echo "$2 - $1" | bc -l | xargs printf '%.2f'; }
fail() { echo "e2e FAIL: $*" >&2; exit 1; }
# exec_out ID CMD -> stdout of the command; asserts exit 0
exec_out() {
  local out; out=$(api POST "/v1/vms/$1/exec" "$(jq -nc --arg c "$2" '{argv:["sh","-c",$c],user:"root",timeout_s:60}')")
  [ "$(echo "$out" | jq -s 'map(select(has("exit")))[0].exit')" = 0 ] || fail "exec '$2' on $1: $out"
  echo "$out" | jq -rs 'map(select(.stream=="stdout").data_b64 | @base64d) | join("")'
}
ssh_ok() { ssh -q -i "$WORK/dl/id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=1 "root@$1" true; }
check() { # check NAME VALUE MAX
  printf 'e2e: %-22s %6ss (max %ss)\n' "$1" "$2" "$3"
  [ "$(echo "$2 <= $3" | bc -l)" = 1 ] || fail "$1 took $2s > $3s"
}
need bc bc

# --- create -> ssh-ready
t0=$(now)
vm=$(api POST /v1/vms '{"name":"e2e-a","team":"e2e"}')
id=$(echo "$vm" | jq -r .id); ip=$(echo "$vm" | jq -r .ip)
[ "$(echo "$vm" | jq -r .state)" = running ] || fail "create: $vm"
[ "$(echo "$vm" | jq -r .agent_ready)" = true ] || fail "agent not ready: $vm"
for _ in $(seq 300); do ssh_ok "$ip" && break; sleep 0.1; done
ssh_ok "$ip" || fail "ssh to $ip"
check create_to_ssh_ready "$(secs "$t0" "$(now)")" "$CREATE_SSH_MAX"

[ "$(exec_out "$id" 'echo hello > /root/marker; cat /root/marker')" = hello ] || fail "exec output"
boot_id=$(exec_out "$id" 'cat /proc/sys/kernel/random/boot_id')

# --- stop (snapshot) -> start (restore)
t=$(now); vm=$(api POST "/v1/vms/$id/stop")
[ "$(echo "$vm" | jq -r '.state + " " + (.has_snapshot|tostring)')" = "stopped true" ] || fail "stop: $vm"
printf 'e2e: %-22s %6ss\n' stop_snapshot "$(secs "$t" "$(now)")"
t=$(now); vm=$(api POST "/v1/vms/$id/start")
check resume "$(secs "$t" "$(now)")" "$RESUME_MAX"
[ "$(echo "$vm" | jq -r .state)" = running ] || fail "start: $vm"
[ "$(exec_out "$id" 'cat /proc/sys/kernel/random/boot_id')" = "$boot_id" ] || fail "start rebooted instead of restoring"
[ "$(exec_out "$id" 'cat /root/marker')" = hello ] || fail "disk lost across stop/start"

# --- fork (running parent) -> exec in the fork
t=$(now); child=$(api POST "/v1/vms/$id/fork" '{"name":"e2e-b"}')
check fork "$(secs "$t" "$(now)")" "$FORK_MAX"
cid=$(echo "$child" | jq -r .id); cip=$(echo "$child" | jq -r .ip)
[ "$(echo "$child" | jq -r .state)" = running ] && [ "$cip" != "$ip" ] || fail "fork: $child"
[ "$(echo "$child" | jq -r .agent_ready)" = true ] || fail "fork identity not applied: $child"
[ "$(exec_out "$cid" 'cat /root/marker')" = hello ] || fail "fork lost the disk"
[ "$(exec_out "$cid" 'hostname')" = e2e-b ] || fail "fork hostname"
exec_out "$cid" 'ip -4 -o addr show eth0' | grep -q "$cip/" || fail "fork not re-IPed"
[ "$(exec_out "$cid" 'cat /etc/machine-id')" != "$(exec_out "$id" 'cat /etc/machine-id')" ] || fail "fork kept machine-id"
for _ in $(seq 50); do ssh_ok "$cip" && break; sleep 0.1; done
ssh_ok "$cip" || fail "ssh to fork $cip"
ssh_ok "$ip" || fail "parent unreachable after fork"
exec_out "$id" 'true' >/dev/null

# --- isolation: no allowIntraTeam, so even same-team VMs cannot reach each other
exec_out "$cid" 'command -v ping' >/dev/null || fail "rootfs has no ping"
if exec_out "$cid" "ping -c1 -W1 $ip >/dev/null 2>&1 && echo reach || echo blocked" | grep -q reach; then
  fail "fork can reach its parent (isolation)"
fi

# --- delete
api POST "/v1/vms/$cid/delete" >/dev/null
api DELETE "/v1/vms/$id" >/dev/null
[ "$(api GET /v1/vms | jq '.vms | length')" = 0 ] || fail "vms left"
[ -z "$(ls "$STATE/vms")" ] || fail "vm dirs left"
api GET "/v1/usage?team=e2e" | jq -c '.teams'
echo "e2e: PASS"
