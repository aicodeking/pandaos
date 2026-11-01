#!/usr/bin/env bash
# Edge e2e for boat on a Linux x86_64 host with /dev/kvm (GitHub runners):
# the real HTTPS front (Caddy with its internal CA, `tls internal`) and the
# real ssh jump host (OpenSSH on 127.0.0.1:2222 with the module's settings and
# jump-keys filter) in front of boatd + boat-api + boat-gateway, all built
# from source and run as plain processes (no NixOS).
#
#   ./e2e-edge.sh GUEST_DIR     GUEST_DIR has vmlinux, rootfs-slim.ext4, rootfs-full.ext4
#
# Checks: private port route 401 without a token, 200 with one (header and
# link -> host-only cookie); public port 200; per-VM desktop origins (VM A's
# cookie and link are refused on VM B's origin); `ssh -J jump@localhost:2222`
# into a VM, no shell on the jump host, grant gone after `boat rm`. Then, if
# present: guest/desktop-check.sh (websocket + a frame), sdk/e2e-live.sh,
# api/e2e-auth.sh, against this stack.
#
# Env: E2E_WORK (/mnt/boat-edge), FC_VERSION (v1.17.0).
set -euo pipefail

GUEST=$(cd "${1:?usage: e2e-edge.sh GUEST_DIR}" && pwd)
HERE=$(cd "$(dirname "$0")/.." && pwd) # components/boat
WORK=${E2E_WORK:-/mnt/boat-edge}
STATE=/var/lib/boat-edge
FC_VERSION=${FC_VERSION:-v1.17.0}
FC_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558 # v1.17.0 x86_64 tgz
SOCK=$WORK/run/boatd.sock
API=http://127.0.0.1:8740
D=boat.test        # api., desktop., *.desktop.
VMD=boatvms.test   # port routes: a separate registrable domain, as recommended
KEYF=$WORK/desktop.key

[ -e /dev/kvm ] || { echo "edge: /dev/kvm missing" >&2; exit 1; }
[ "$(id -u)" != 0 ] || { echo "edge: run as a normal user with sudo" >&2; exit 1; }

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { echo "edge FAIL: $*" >&2; exit 1; }
warn() { echo "::warning::edge: $*"; }

PIDS=()
cleanup() {
  set +e
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  [ -f "$WORK/caddy.pid" ] && sudo kill "$(cat "$WORK/caddy.pid")" 2>/dev/null
  [ -f "$WORK/sshd/pid" ] && sudo kill "$(cat "$WORK/sshd/pid")" 2>/dev/null
  sudo pkill -9 -x firecracker
  if [ -f "$WORK/boatd.pid" ]; then
    sudo kill "$(cat "$WORK/boatd.pid")" 2>/dev/null
    for _ in $(seq 100); do sudo kill -0 "$(cat "$WORK/boatd.pid")" 2>/dev/null || break; sleep 0.2; done
  fi
  sudo pkill -9 -x firecracker
  sleep 0.5
  for t in $(ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^bt[0-9]+$'); do sudo ip link del "$t"; done
  sudo ip link del boatbr0 2>/dev/null
  sudo nft delete table inet boat 2>/dev/null
  sudo nft delete table bridge boat 2>/dev/null
  sudo iptables -D FORWARD -i boatbr0 -j ACCEPT 2>/dev/null
  sudo iptables -D FORWARD -o boatbr0 -j ACCEPT 2>/dev/null
  mountpoint -q "$STATE" && sudo umount -l "$STATE"
  sudo rmdir /sys/fs/cgroup/boat-edge 2>/dev/null
  for f in boatd api gateway caddy sshd/log; do
    [ -f "$WORK/$f.log" ] && { echo "--- $f.log (tail)"; sudo tail -n 40 "$WORK/$f.log"; }
  done
  [ -f "$WORK/sshd/log" ] && { echo "--- sshd log (tail)"; sudo tail -n 40 "$WORK/sshd/log"; }
}
trap cleanup EXIT

sudo mkdir -p "$WORK" && sudo chown "$(id -u):$(id -g)" "$WORK"
mkdir -p "$WORK/bin" "$WORK/dl" "$WORK/run" "$WORK/api" "$WORK/tmp" "$WORK/ssh" "$WORK/sshd" "$WORK/caddy"

log "tools (caddy, sshd, jq, nft, btrfs)"
missing=()
for c in jq:jq nft:nftables mkfs.btrfs:btrfs-progs curl:curl ssh:openssh-client caddy:caddy /usr/sbin/sshd:openssh-server; do
  command -v "${c%%:*}" >/dev/null || missing+=("${c#*:}")
done
if [ ${#missing[@]} -gt 0 ]; then
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null
fi
# The distro caddy unit would hold :80/:443.
sudo systemctl disable --now caddy.service 2>/dev/null || true
caddy version
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

log "go build"
for c in boatd:boatd api:boat-api gateway:boat-gateway cli:boat; do
  (cd "$HERE/${c%%:*}" && CGO_ENABLED=0 go build -o "$WORK/bin/${c#*:}" .)
done
export PATH="$WORK/bin:$PATH"

log "state (btrfs) + images"
[ -f "$WORK/state.img" ] || { truncate -s 40G "$WORK/state.img"; mkfs.btrfs -q "$WORK/state.img"; }
sudo mkdir -p "$STATE"
mountpoint -q "$STATE" || sudo mount -o loop "$WORK/state.img" "$STATE"
sudo rm -rf "$STATE/vms" "$STATE/snapshots" "$STATE/usage.jsonl"
for v in slim full; do
  sudo mkdir -p "$STATE/images/$v"
  sudo cp --sparse=always "$GUEST/vmlinux" "$STATE/images/$v/vmlinux"
  sudo cp --sparse=always "$GUEST/rootfs-$v.ext4" "$STATE/images/$v/rootfs.ext4"
done

log "boatd (root, jailer)"
sudo mkdir -p /sys/fs/cgroup/boat-edge
cat >"$WORK/boatd.json" <<JSON
{"stateDir":"$STATE","runDir":"$WORK/run","socket":"$SOCK","socketGroup":"$(id -gn)",
 "firecracker":"$WORK/dl/firecracker","jailer":"$WORK/dl/jailer","cgroupParent":"boat-edge",
 "reflink":"always","bootTimeoutS":90,"defaultImage":"slim","defaultSize":"e2e",
 "sizes":{"e2e":{"vcpu":2,"mem_mib":2048,"disk_gib":8}},"defaultTeam":{"maxVMs":8}}
JSON
sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >"$4" 2>&1' _ \
  "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.json" "$WORK/boatd.log" &
for _ in $(seq 600); do [ -S "$SOCK" ] && break; sleep 0.1; done
[ -S "$SOCK" ] || fail "boatd did not start in 60s"

log "boat-api + boat-gateway"
head -c 32 /dev/urandom >"$KEYF" && chmod 600 "$KEYF"
KEY=$(boat-api admin init -data "$WORK/api" -user edge -team edge | jq -r .token)
[[ $KEY == boat_* ]] || fail "admin init: $KEY"
api_args=(-data "$WORK/api" -listen 127.0.0.1:8740 -boatd "$SOCK" -rate 0 -images "slim,full" -ssh-user dev
  -desktop-url "https://desktop.$D" -desktop-key-file "$KEYF"
  -ssh-jump "jump@localhost:2222" -ssh-jump-keys-file "$WORK/ssh/jump_authorized_keys")
help=$(boat-api serve -h 2>&1 || true)
if [[ $help == *-vm-domain* ]]; then api_args+=(-vm-domain "$VMD"); fi
if [[ $help == *-ai-proxy-listen* ]]; then api_args+=(-ai-proxy-listen 10.200.0.1:8744); fi
boat-api serve "${api_args[@]}" >"$WORK/api.log" 2>&1 &
PIDS+=($!)
boat-gateway -listen 127.0.0.1:8742 -ask-listen 127.0.0.1:8743 -domain "$D" -vm-domain "$VMD" \
  -desktop-domain "desktop.$D" -routes "$WORK/run/routes.json" -desktop-key "$KEYF" >"$WORK/gateway.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -fs "$API/healthz" >/dev/null && break; sleep 0.1; done

log "caddy (tls internal) on :443"
cat >"$WORK/Caddyfile" <<EOF
{
	admin off
	skip_install_trust
	auto_https disable_redirects
	storage file_system $WORK/caddy
}
api.$D {
	tls internal
	route {
		handle /internal/* {
			respond 404
		}
		handle /v1/* {
			reverse_proxy 127.0.0.1:8740
		}
		handle /healthz {
			reverse_proxy 127.0.0.1:8740
		}
		handle {
			root * $HERE/dashboard/web
			header Content-Security-Policy "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
			try_files {path} /index.html
			file_server
		}
	}
}
desktop.$D, *.desktop.$D, *.$VMD {
	tls internal
	reverse_proxy 127.0.0.1:8742
}
EOF
caddy fmt --overwrite "$WORK/Caddyfile" >/dev/null 2>&1 || true
caddy validate --config "$WORK/Caddyfile" --adapter caddyfile
sudo bash -c 'echo $$ >"$1"; exec caddy run --config "$2" --adapter caddyfile >"$3" 2>&1' _ \
  "$WORK/caddy.pid" "$WORK/Caddyfile" "$WORK/caddy.log" &
CA=$WORK/caddy/pki/authorities/local/root.crt
for _ in $(seq 100); do sudo test -s "$CA" && break; sleep 0.2; done
sudo test -s "$CA" || fail "caddy made no local CA"
sudo cp "$CA" "$WORK/ca.pem" && sudo chown "$(id -u)" "$WORK/ca.pem"
CA=$WORK/ca.pem
# https_code URL [curl args...] -> HTTP status over real TLS (verified against Caddy's CA)
hc() { local u=$1; shift; curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -o /dev/null -w '%{http_code}' "$@" "$u"; }
for _ in $(seq 50); do [ "$(hc "https://api.$D/healthz" || true)" = 200 ] && break; sleep 0.2; done
[ "$(hc "https://api.$D/healthz")" = 200 ] || fail "api over TLS"
[ "$(hc "https://api.$D/internal/x")" = 404 ] || fail "/internal/* reachable"

log "ssh jump host on 127.0.0.1:2222 (module settings + jump-keys filter)"
id jump >/dev/null 2>&1 || sudo useradd -r -M -s /usr/sbin/nologin jump
sudo usermod -p '*' jump # not locked ('!'), still no password
sudo install -d -m 0755 /etc/boat-edge
{ echo '#!/bin/bash'; cat "$HERE/gateway/jump-keys.sh"; } | sudo tee /etc/boat-edge/jump-keys >/dev/null
sudo chmod 0755 /etc/boat-edge/jump-keys
ssh-keygen -q -t ed25519 -N "" -f "$WORK/sshd/host_ed25519" <<<y >/dev/null 2>&1 || true
sudo chown root:root "$WORK/sshd/host_ed25519"
cat >"$WORK/sshd/config" <<EOF
Port 2222
ListenAddress 127.0.0.1
HostKey $WORK/sshd/host_ed25519
PidFile $WORK/sshd/pid
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowUsers jump
AuthorizedKeysFile none
AuthorizedKeysCommand /etc/boat-edge/jump-keys $WORK/ssh/jump_authorized_keys $WORK/run/routes.json
AuthorizedKeysCommandUser $(id -un)
AllowTcpForwarding local
AllowAgentForwarding no
AllowStreamLocalForwarding no
X11Forwarding no
PermitTTY no
PermitTunnel no
GatewayPorts no
ForceCommand /bin/false
EOF
sudo mkdir -p /run/sshd
sudo /usr/sbin/sshd -t -f "$WORK/sshd/config"
sudo /usr/sbin/sshd -f "$WORK/sshd/config" -E "$WORK/sshd/log"
[ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N "" -f ~/.ssh/id_ed25519
cat >"$WORK/ssh_config" <<EOF
Host *
	BatchMode yes
	StrictHostKeyChecking no
	UserKnownHostsFile /dev/null
	LogLevel ERROR
	ConnectTimeout 5
EOF

export BOAT_CONFIG=$WORK/cli.json
boat login --url "$API" --key "$KEY" >/dev/null
field() { boat list --json | jq -r --arg n "$1" "select(.name == \$n) | .$2"; }
run() { boat exec "$1" -- bash -c "$2"; }
hmac_hex() { python3 -c 'import hmac,hashlib,sys; print(hmac.new(open(sys.argv[1],"rb").read(), sys.argv[2].encode(), hashlib.sha256).hexdigest())' "$KEYF" "$1"; }

log "VM A (slim): http on 8080 (private) and 8081 (public)"
boat new --name edge-a --json >/dev/null
id_a=$(field edge-a id) ip_a=$(field edge-a ip)
run edge-a 'mkdir -p /tmp/www && echo edge-ok >/tmp/www/index.html &&
  for p in 8080 8081; do setsid nohup python3 -m http.server $p -d /tmp/www >/dev/null 2>&1 </dev/null & done; sleep 1'
boat ports add edge-a 8080 --json >/dev/null
boat ports add edge-a 8081 --public --json >/dev/null
for _ in $(seq 50); do jq -e --arg i "$id_a" '.vms[$i].ports | index(8080)' "$WORK/run/routes.json" >/dev/null 2>&1 && break; sleep 0.2; done
priv="https://8080-$id_a.$VMD/"
pub="https://8081-$id_a.$VMD/"

log "private route: 401 without a token, 200 with one"
[ "$(hc "$priv")" = 401 ] || fail "private route open without a token: $(hc "$priv")"
exp=$(($(date +%s) + 300))
rt="$exp.$(hmac_hex "route.8080-$id_a.$exp")"
[ "$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -H "X-Boat-Route-Token: $rt" "$priv")" = edge-ok ] || fail "private route with a header token"
wrong="$exp.$(hmac_hex "route.8081-$id_a.$exp")"
[ "$(hc "$priv" -H "X-Boat-Route-Token: $wrong")" = 401 ] || fail "token for another port accepted"
# Link -> host-only cookie -> plain requests.
jar=$WORK/tmp/route.jar
hc "${priv}?boat_exp=$exp&boat_sig=${rt#*.}" -c "$jar" >/dev/null
grep -q '__Host-boat_route' "$jar" || fail "route link set no cookie"
grep '__Host-boat_route' "$jar" | awk '{exit !($1 == "#HttpOnly_8080-'"$id_a.$VMD"'" && $2 == "FALSE")}' ||
  fail "route cookie is not host-only: $(grep __Host "$jar")"
[ "$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -b "$jar" "$priv")" = edge-ok ] || fail "private route with the cookie"
if url=$(boat ports url edge-a 8080 --json 2>/dev/null | jq -er .url); then
  [ "$(hc "$url" -L -c "$WORK/tmp/r2.jar" -b "$WORK/tmp/r2.jar")" = 200 ] || fail "boat ports url link"
else
  warn "no 'boat ports url' yet; route token minted by the test"
fi

log "public route"
if jq -e --arg i "$id_a" '.vms[$i] | has("public")' "$WORK/run/routes.json" >/dev/null; then
  [ "$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" "$pub")" = edge-ok ] || fail "public route"
else
  warn "boatd routes.json has no \"public\" list yet: public routes are private (fail-closed)"
  [ "$(hc "$pub")" = 401 ] || fail "route without a public list is not private"
fi

log "VM D (full): desktop on its own origin"
boat new --name edge-d --image full --json >/dev/null
id_d=$(field edge-d id)
link=$(boat desktop edge-d --json | jq -r .url)
[[ $link == "https://desktop.$D/$id_d?exp="* ]] || fail "desktop link: $link"
djar=$WORK/tmp/desktop.jar
final=$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -L -c "$djar" -b "$djar" -o /dev/null -w '%{url_effective}' "$link")
[ "$final" = "https://$id_d.desktop.$D/" ] || fail "desktop link did not end on the VM origin without its signature: $final"
grep -q "$id_d.desktop.$D.*__Host-boat_gw" "$djar" || fail "no desktop cookie for $id_d.desktop.$D"
awk -v h="desktop.$D" '{ d = $1; sub(/^#HttpOnly_/, "", d) } d == h { f = 1 } END { exit !f }' "$djar" &&
  fail "a cookie was set on the shared desktop.$D origin"
code=$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -b "$djar" -o "$WORK/tmp/desktop.html" -w '%{http_code}' "https://$id_d.desktop.$D/")
[ "$code" = 200 ] && grep -qi '<html' "$WORK/tmp/desktop.html" || fail "desktop page: HTTP $code"
log "desktop origin isolation: VM D's session/link on VM A's origin"
ck=$(awk '$6 == "__Host-boat_gw" {print $6 "=" $7}' "$djar" | head -1)
[ "$(hc "https://$id_a.desktop.$D/" -b "$ck")" = 401 ] || fail "VM D's cookie opened VM A's desktop origin"
[ "$(hc "https://$id_a.desktop.$D/?${link#*\?}")" = 403 ] || fail "VM D's link accepted on VM A's origin"
[ "$(hc "https://$id_d.desktop.$D/")" = 401 ] || fail "desktop open without a cookie"
log "cross-origin: a page on VM A must not use the browser's session for VM D (same-site, so SameSite does not help)"
WS=(-H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" -H "Sec-WebSocket-Protocol: binary")
[ "$(hc "https://$id_d.desktop.$D/" -b "$ck" -H "Origin: https://$id_a.desktop.$D")" = 403 ] || fail "request with VM A's Origin reached VM D's desktop"
[ "$(hc "https://$id_d.desktop.$D/websockify" -b "$ck" -H "Origin: https://$id_a.desktop.$D" "${WS[@]}")" = 403 ] || fail "websocket with VM A's Origin reached VM D's desktop"
[ "$(hc "https://$id_d.desktop.$D/" -b "$ck" -H "Sec-Fetch-Site: same-site" -H "Sec-Fetch-Mode: no-cors")" = 403 ] || fail "same-site subresource request reached VM D's desktop"
# Control: the same request from its own origin gets past the gateway (HTTP/1.1, as websockets require).
code=$(curl -sS --http1.1 --cacert "$CA" --connect-to "::127.0.0.1:443" -o /dev/null -w '%{http_code}' --max-time 5 \
  -b "$ck" -H "Origin: https://$id_d.desktop.$D" "${WS[@]}" "https://$id_d.desktop.$D/websockify" || true)
[ "$code" = 101 ] || fail "websocket from VM D's own origin: HTTP $code"

if [ -x "$HERE/guest/desktop-check.sh" ]; then
  log "guest/desktop-check.sh: websocket + one frame"
  link=$(boat desktop edge-d --json | jq -r .url)
  CURL_OPTS="--cacert $CA --connect-to ::127.0.0.1:443" SSL_CA=$CA \
    ANIMATE_CMD="boat exec edge-d -- boat-desktop-animate 10" \
    "$HERE/guest/desktop-check.sh" "$link" || fail "desktop-check.sh"
else
  warn "components/boat/guest/desktop-check.sh not present yet"
fi

log "ssh -J jump@localhost:2222 dev@<vm>"
info=$(boat ssh edge-a --json) # grants ~/.ssh/id_ed25519.pub on dev and on the jump host
[ "$(jq -r .jump <<<"$info")" = "jump@localhost:2222" ] || fail "ssh info has no jump: $info"
for _ in $(seq 50); do grep -q "permitopen=\"$ip_a:22\"" "$WORK/ssh/jump_authorized_keys" 2>/dev/null && break; sleep 0.2; done
grep -q "permitopen=\"$ip_a:22\"" "$WORK/ssh/jump_authorized_keys" || fail "boat-api wrote no jump grant for $ip_a"
sshj() { ssh -F "$WORK/ssh_config" -J jump@localhost:2222 "$@"; }
out=""
for _ in $(seq 30); do out=$(sshj "dev@$ip_a" hostname 2>&1) && break; sleep 1; done
[ "$out" = edge-a ] || fail "ssh -J: $out"
# The CLI's own ProxyCommand path. e2e.sh ran on this runner first, and VM IPs
# are reused: drop its known_hosts entry for this address.
ssh-keygen -q -R "$ip_a" >/dev/null 2>&1 || true
[ "$(boat ssh edge-a -- hostname </dev/null)" = edge-a ] || fail "boat ssh via the jump host"
ssh -F "$WORK/ssh_config" -p 2222 jump@localhost true 2>/dev/null && fail "jump user got a shell"
sshj "dev@10.200.255.254" true 2>/dev/null && fail "jump host forwarded to an address with no grant"
ssh -F "$WORK/ssh_config" -p 2222 "$(id -un)@localhost" true 2>/dev/null && fail "port 2222 admitted a non-jump user"

log "upgrade: stop boatd (snapshots VMs), start a newer build (resumes them): processes and routes survive"
boot_a=$(run edge-a 'cat /proc/sys/kernel/random/boot_id')
pid=$(cat "$WORK/boatd.pid")
sudo kill "$pid"
for _ in $(seq 600); do sudo kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
sudo kill -0 "$pid" 2>/dev/null && fail "boatd did not stop within 120s"
# A different binary from the same source stands in for the next version.
(cd "$HERE/boatd" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORK/bin/boatd.next" .)
cmp -s "$WORK/bin/boatd" "$WORK/bin/boatd.next" && fail "the next boatd build is the same binary"
sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >>"$4" 2>&1' _ \
  "$WORK/boatd.pid" "$WORK/bin/boatd.next" "$WORK/boatd.json" "$WORK/boatd.log" &
for _ in $(seq 600); do curl -fs --unix-socket "$SOCK" http://boatd/v1/health >/dev/null && break; sleep 0.2; done
[ "$(sudo readlink "/proc/$(cat "$WORK/boatd.pid")/exe")" = "$WORK/bin/boatd.next" ] || fail "the next boatd build is not the one running"
for _ in $(seq 150); do [ "$(field edge-a state)" = running ] && break; sleep 0.4; done
[ "$(field edge-a state)" = running ] || fail "edge-a is $(field edge-a state) after the upgrade"
[ "$(run edge-a 'cat /proc/sys/kernel/random/boot_id')" = "$boot_a" ] || fail "edge-a cold-booted instead of resuming"
rt_new_exp=$(($(date +%s) + 300))
rt_new="$rt_new_exp.$(hmac_hex "route.8080-$id_a.$rt_new_exp")"
[ "$(curl -sS --cacert "$CA" --connect-to "::127.0.0.1:443" -H "X-Boat-Route-Token: $rt_new" "$priv")" = edge-ok ] || fail "guest server or route lost across the upgrade"

if [ -x "$HERE/sdk/e2e-live.sh" ] || [ -x "$HERE/api/e2e-auth.sh" ]; then
  export BOAT_URL=$API BOAT_API_KEY=$KEY BOAT_API_DATA=$WORK/api BOAT_VM_DOMAIN=$VMD E2E_TMP=$WORK/tmp
  export BOAT_GATEWAY_ADDR=127.0.0.1:8742
fi
for s in sdk/e2e-live.sh api/e2e-auth.sh sdk/e2e-agent-from-env.sh; do
  if [ -x "$HERE/$s" ]; then
    log "$s"
    "$HERE/$s" || fail "$s"
  else
    warn "components/boat/$s not present yet"
  fi
done

if [ -f "$HERE/dashboard/web/index.html" ]; then
  log "dashboard: served read-only at https://api.$D/, same origin as the API (real browser)"
  page=$(curl -sS -D - --cacert "$CA" --connect-to "::127.0.0.1:443" "https://api.$D/")
  grep -qi "frame-ancestors 'none'" <<<"$page" && grep -qi '<html' <<<"$page" || fail "dashboard page: $page"
  [ "$(hc "https://api.$D/v1/me" -H "Authorization: Bearer $KEY")" = 200 ] || fail "API behind the dashboard"
  grep -q "api.$D" /etc/hosts || echo "127.0.0.1 api.$D" | sudo tee -a /etc/hosts >/dev/null
  BOAT_URL=https://api.$D BOAT_KEY=$KEY BOAT_INSECURE=1 BOAT_E2E_REQUIRE_BROWSER=1 "$HERE/dashboard/e2e.sh" || fail "dashboard/e2e.sh"
else
  warn "components/boat/dashboard/web not present yet"
fi

log "rm: the VM's jump grant and routes go away"
boat rm edge-a --json >/dev/null
sshj "dev@$ip_a" true 2>/dev/null && fail "jump grant survived boat rm"
[ "$(hc "$priv" -H "X-Boat-Route-Token: $rt")" = 404 ] || fail "route survived boat rm"
boat rm edge-d --json >/dev/null
echo "METRIC $(jq -nc '{test: "boat-edge-e2e", pass: true}')"
echo "edge: PASS"
