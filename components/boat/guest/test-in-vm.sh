#!/usr/bin/env bash
# Boot the guest image in Firecracker and exercise the agent contract.
# Needs: x86_64 Linux, /dev/kvm, sudo (tap + NAT), python3, curl, ssh.
#
#   ./test-in-vm.sh [out/vmlinux] [out/rootfs-full.ext4]
#
# Env: FIRECRACKER (path; otherwise the pinned release is downloaded),
#      VARIANT=full|slim (default: from the rootfs file name), DISK_GB=8.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
KERNEL=${1:-$HERE/out/vmlinux}
ROOTFS=${2:-$HERE/out/rootfs-full.ext4}
VARIANT=${VARIANT:-$( [[ $ROOTFS == *slim* ]] && echo slim || echo full)}
FC_VERSION=v1.17.0
FC_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
TAP=boattap0 HOST_IP=172.16.0.1 GUEST_IP=172.16.0.2 MASK=255.255.255.252
GUEST_MAC=06:00:ac:10:00:02

W=$(mktemp -d)
FC_PID=
cleanup() {
  [[ -n $FC_PID ]] && kill "$FC_PID" 2>/dev/null || true
  sudo ip link del "$TAP" 2>/dev/null || true
  sudo iptables -t nat -D POSTROUTING -s "$GUEST_IP/32" -j MASQUERADE 2>/dev/null || true
  sudo iptables -D FORWARD -i "$TAP" -j ACCEPT 2>/dev/null || true
  sudo iptables -D FORWARD -o "$TAP" -j ACCEPT 2>/dev/null || true
  echo "--- serial console (tail) ---"; tail -n 40 "$W/serial.log" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT
pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }

# ---- firecracker
if [[ -z ${FIRECRACKER:-} ]]; then
  curl -fsSLo "$W/fc.tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
  echo "$FC_SHA256  $W/fc.tgz" | sha256sum -c -
  tar -xzf "$W/fc.tgz" -C "$W"
  FIRECRACKER="$W/release-$FC_VERSION-x86_64/firecracker-$FC_VERSION-x86_64"
fi

# ---- disk: a sparse copy, bigger than the image, to exercise resize2fs
cp --sparse=always "$ROOTFS" "$W/disk.ext4"
truncate -s "${DISK_GB:-8}G" "$W/disk.ext4"

# ---- network: tap + NAT so docker can pull hello-world
sudo ip tuntap add "$TAP" mode tap user "$(id -u)"
sudo ip addr add "$HOST_IP/30" dev "$TAP"
sudo ip link set "$TAP" up
sudo sysctl -qw net.ipv4.ip_forward=1
sudo iptables -t nat -A POSTROUTING -s "$GUEST_IP/32" -j MASQUERADE
sudo iptables -I FORWARD -i "$TAP" -j ACCEPT
sudo iptables -I FORWARD -o "$TAP" -j ACCEPT

cat > "$W/vm.json" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=$GUEST_IP::$HOST_IP:$MASK::eth0:off:1.1.1.1"
  },
  "drives": [{"drive_id": "root", "path_on_host": "$W/disk.ext4", "is_root_device": true, "is_read_only": false}],
  "network-interfaces": [{"iface_id": "eth0", "guest_mac": "$GUEST_MAC", "host_dev_name": "$TAP"}],
  "vsock": {"guest_cid": 3, "uds_path": "$W/vsock.sock"},
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 4096}
}
EOF
"$FIRECRACKER" --no-api --config-file "$W/vm.json" > "$W/serial.log" 2>&1 &
FC_PID=$!

# ---- host-side client for Firecracker's vsock UDS ("CONNECT <port>\n")
cat > "$W/boat.py" <<'EOF'
import base64, json, socket, sys
def connect(path):
    s = socket.socket(socket.AF_UNIX); s.settimeout(900); s.connect(path)
    s.sendall(b"CONNECT 1024\n"); f = s.makefile("rb")
    if not f.readline().startswith(b"OK"): raise SystemExit("vsock connect refused")
    return s, f
def call(path, req):
    s, f = connect(path); s.sendall((json.dumps(req) + "\n").encode())
    if req["op"] == "exec":
        while True:
            m = json.loads(f.readline())
            if "stream" in m:
                out = sys.stdout if m["stream"] == "stdout" else sys.stderr
                out.buffer.write(base64.b64decode(m["data_b64"])); out.flush()
            else:
                print(json.dumps(m), file=sys.stderr); sys.exit(m.get("exit") if "exit" in m else 99)
    if req["op"] == "get_file":
        while True:
            m = json.loads(f.readline())
            if "error" in m or m.get("eof"): print(json.dumps(m), file=sys.stderr); sys.exit("error" in m)
            sys.stdout.buffer.write(base64.b64decode(m["data_b64"]))
    m = json.loads(f.readline()); print(json.dumps(m)); sys.exit(1 if "error" in m else 0)
if sys.argv[2] == "put":   # put <local> <remote> [mode]: 1 MiB chunks
    data = open(sys.argv[3], "rb").read(); s, f = connect(sys.argv[1]); off = 0; C = 1 << 20
    while True:
        chunk = data[off:off + C]; last = off + C >= len(data)
        s.sendall((json.dumps({"op": "put_file", "path": sys.argv[4], "mode": sys.argv[5] if len(sys.argv) > 5 else "0644",
                   "offset": off, "last": last, "data_b64": base64.b64encode(chunk).decode()}) + "\n").encode())
        m = json.loads(f.readline())
        if "error" in m: raise SystemExit(m["error"])
        off += len(chunk)
        if last: break
    print(json.dumps(m))
else:
    call(sys.argv[1], json.loads(sys.argv[2]))
EOF
boat() { python3 "$W/boat.py" "$W/vsock.sock" "$@"; }
vexec() {  # vexec <timeout_s> <user> cmd...
  local t=$1 u=$2; shift 2
  boat "$(python3 -c 'import json,sys; print(json.dumps({"op":"exec","argv":sys.argv[3:],"timeout_s":float(sys.argv[1]),"user":sys.argv[2]}))' "$t" "$u" "$@")"
}

# ---- 1. boot + health
t0=$(date +%s)
for _ in $(seq 180); do boat '{"op":"health"}' >/dev/null 2>&1 && break; sleep 1; done
boat '{"op":"health"}' > "$W/health.json" && grep -q '"ok": true' "$W/health.json" || fail "health"
pass "health after $(( $(date +%s) - t0 ))s: $(cat "$W/health.json")"

# ---- 2. exec: stdout, exit code, timeout
[[ $(vexec 30 dev whoami 2>/dev/null) == dev ]] || fail "exec whoami"
vexec 30 dev sh -c 'exit 7' 2>/dev/null && fail "exit code" || [[ $? == 7 ]] || fail "exit code"
vexec 2 root sleep 60 2>"$W/to.err" && fail "timeout"; grep -q timed_out "$W/to.err" || fail "timeout flag"
pass "exec (user, exit code, timeout)"

# ---- 3. root disk grown
size_gb=$(vexec 30 root sh -c "df -BG --output=size / | tail -1 | tr -dc 0-9")
(( size_gb >= ${DISK_GB:-8} - 1 )) || fail "rootfs not grown (${size_gb}G)"
pass "rootfs grown to ${size_gb}G"

# ---- 4. put/get file (3 MiB, chunked)
head -c 3145728 /dev/urandom > "$W/blob"
boat put "$W/blob" /home/dev/blob.bin 0640 >/dev/null || fail "put_file"
boat '{"op":"get_file","path":"/home/dev/blob.bin"}' > "$W/blob.back" 2>/dev/null || fail "get_file"
cmp -s "$W/blob" "$W/blob.back" || fail "get_file content"
[[ $(vexec 30 root stat -c '%U %a' /home/dev/blob.bin) == "dev 640" ]] || fail "put_file owner/mode"
pass "put_file/get_file 3 MiB round trip, owner dev, mode 640"

# ---- 5. ssh with a key (no password)
ssh-keygen -q -t ed25519 -N '' -f "$W/id"
boat put "$W/id.pub" /home/dev/.ssh/authorized_keys 0600 >/dev/null
SSH=(ssh -i "$W/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile="$W/known" -o ConnectTimeout=10 -o BatchMode=yes)
for _ in $(seq 30); do "${SSH[@]}" "dev@$GUEST_IP" true 2>/dev/null && break; sleep 1; done
[[ $("${SSH[@]}" "dev@$GUEST_IP" 'sudo -n whoami') == root ]] || fail "ssh + passwordless sudo"
sshd_cfg=$(vexec 30 root sshd -T 2>/dev/null)
grep -qx 'passwordauthentication no' <<<"$sshd_cfg" || fail "password login allowed"
grep -qx 'permitrootlogin no' <<<"$sshd_cfg" || fail "root login allowed"
pass "ssh key login + sudo; password login refused"

# ---- 6. set_env
boat '{"op":"set_env","vars":{"BOAT_TEST":"a b"}}' >/dev/null || fail "set_env"
[[ $(vexec 30 dev sh -c 'printf %s "$BOAT_TEST"') == "a b" ]] || fail "set_env in exec"
[[ $("${SSH[@]}" "dev@$GUEST_IP" 'bash -lc "printf %s \"\$BOAT_TEST\""') == "a b" ]] || fail "set_env in login shell"
pass "set_env (exec + login shell)"

# ---- 7. set_identity (as after a fork: same IP here, new name/mac/ids)
mid_before=$(vexec 30 root cat /etc/machine-id)
key_before=$(vexec 30 root cat /etc/ssh/ssh_host_ed25519_key.pub)
boat "{\"op\":\"set_identity\",\"hostname\":\"boat-fork-1\",\"ipv4_cidr\":\"$GUEST_IP/30\",\"gw\":\"$HOST_IP\",\"dns\":[\"8.8.8.8\"],\"mac\":\"$GUEST_MAC\"}" \
  >/dev/null || fail "set_identity"
[[ $(vexec 30 root hostname) == boat-fork-1 ]] || fail "hostname"
[[ $(vexec 30 root cat /etc/machine-id) != "$mid_before" ]] || fail "machine-id not regenerated"
[[ $(vexec 30 root cat /etc/ssh/ssh_host_ed25519_key.pub) != "$key_before" ]] || fail "host keys not regenerated"
vexec 30 root grep -q 8.8.8.8 /etc/resolv.conf || fail "dns"
rm -f "$W/known"; "${SSH[@]}" "dev@$GUEST_IP" true || fail "ssh after set_identity"
pass "set_identity (hostname, machine-id, host keys, dns, network up)"

# ---- 8. docker + agent CLIs
hello=$(vexec 300 dev docker run --rm hello-world 2>&1) || fail "docker run hello-world: $hello"
grep -q "Hello from Docker" <<<"$hello" || fail "docker run hello-world output"
pass "docker run hello-world"
for c in "claude --version" "codex --version" "opencode --version" "gh --version" "bun --version" \
         "node --version" "git --version" "rg --version" "jq --version" "tmux -V" "rustc --version"; do
  v=$(vexec 120 dev bash -lc "$c" 2>/dev/null | head -1) || fail "$c"
  pass "$c: $v"
done

# ---- 9. idle tracking
sleep 3
idle=$(boat '{"op":"health"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["idle_s"])')
python3 -c "import sys; sys.exit(0 if float('$idle') >= 2 else 1)" || fail "idle_s ($idle) did not grow"
pass "idle_s grows when idle ($idle)"

# ---- 10. desktop (full only)
if [[ $VARIANT == full ]]; then
  boat '{"op":"desktop","action":"start"}' > "$W/desk.json" || fail "desktop start: $(cat "$W/desk.json")"
  port=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["port"])' "$W/desk.json")
  tok=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$W/desk.json")
  base="http://$GUEST_IP:$port"
  [[ $(curl -s -o /dev/null -w '%{http_code}' "$base/") == 403 ]] || fail "desktop without token not 403"
  [[ $(curl -s -o /dev/null -w '%{http_code}' -c "$W/jar" "$base/?token=$tok") == 302 ]] || fail "token redirect"
  page=$(curl -sf -b "$W/jar" "$base/") || fail "desktop page via cookie"
  grep -qi '<html' <<<"$page" || fail "desktop page is not html"
  curl -s -m 3 "http://$GUEST_IP:6081/" >/dev/null && fail "kasmvnc backend reachable from host"
  [[ $(boat '{"op":"desktop","action":"status"}') == *'"running": true'* ]] || fail "desktop status"
  vexec 30 dev env DISPLAY=:1 xdpyinfo >/dev/null 2>&1 || fail "X display :1"
  pass "desktop: KasmVNC on :$port, token -> cookie, backend local-only"
  boat '{"op":"desktop","action":"stop"}' >/dev/null
else
  boat '{"op":"desktop","action":"start"}' >/dev/null 2>&1 && fail "slim image has a desktop"
  pass "slim: desktop start refused"
fi

# ---- 11. shutdown -> firecracker exits
boat '{"op":"shutdown"}' >/dev/null
for _ in $(seq 60); do kill -0 "$FC_PID" 2>/dev/null || break; sleep 1; done
kill -0 "$FC_PID" 2>/dev/null && fail "firecracker still running after shutdown"
FC_PID=
pass "shutdown"
echo "ALL PASSED ($VARIANT)"
