# shellcheck shell=bash
# shellcheck disable=SC2154,SC2024 # host_ip, stop vars etc. come from e2e.sh; root writes no file here
# boatd e2e, part 2: limits, recovery and parity. Sourced by e2e-host.sh after
# docker, with no VMs left and the e2e-ext netns up. Each phase may restart boatd
# with a modified config and puts the normal one back.
#
#   default size  perf at 4 vCPU / 8 GiB with 2 GiB of dirty memory; nested KVM spike
#   resize        disk grow while running and across a restore; vCPU/RAM on a stopped VM
#   agent         /v1/vms/{id}/agent allowlist against the real guest
#   export        tar.zst download, signed import keeps memory, unsigned does not
#   RA            a tenant router advertisement changes nothing on the host
#   unknown port  a bt* port with no rules passes nothing
#   ports         public:false closes a published port, per-VM cap
#   egress        the cap measured with a transfer, against an uncapped control
#   ct limit      connections refused past ctPerVM, against an uncapped control
#   disk limit    Firecracker drive limiter measured; io.max set on the VM's cgroup
#   serial log    a noisy guest cannot grow firecracker.log past its cap
#   public IPv4   1:1 NAT in and out, with a documentation-range address
#   systemd       crash under KillMode=mixed: VMs outside the unit survive with
#                 memory; VMs inside it are killed and cold-boot on restart
#   ENOSPC        a really full small btrfs: refusals, clean stop, recovery

DEFAULT_CREATE_MAX=${E2E_DEFAULT_CREATE_MAX:-20}
DEFAULT_STOP_MAX=${E2E_DEFAULT_STOP_MAX:-40}

cfg_with() { jq "$1" "$WORK/boatd.json" >"$WORK/boatd-$2.json"; echo "$WORK/boatd-$2.json"; }
put_file() { curl -fsS --unix-socket "$SOCK" -X PUT --data-binary "@$3" "http://boatd/v1/vms/$1/files?path=$2&mode=755" >/dev/null; }
wait_http() { for _ in $(seq 50); do curl -fs --max-time 2 -o /dev/null "$1" && return; sleep 0.2; done; fail "no answer from $1"; }
mbps() { awk -v b="$1" 'BEGIN { printf "%.0f", b * 8 / 1000000 }'; }

log "limits e2e: default size (4 vCPU, 8 GiB, dirty memory)"
t=$(now); d1=$(newvm '{"name":"d1","team":"lim","size":"default"}'); dcreate_s=$(secs "$t" "$(now)")
[ "$(vmf "$d1" vcpu) $(vmf "$d1" mem_mib) $(vmf "$d1" agent_ready)" = "4 8192 true" ] || fail "d1: $(bd GET "/v1/vms/$d1")"
bx "$d1" 'mkdir -p /dev/shm/d; head -c 2G /dev/urandom >/dev/shm/d/dirty; sync'
dsum=$(bx "$d1" 'sha256sum /dev/shm/d/dirty | cut -c1-64')
t=$(now); bd POST "/v1/vms/$d1/stop" >/dev/null; dstop_s=$(secs "$t" "$(now)")
[ "$(vmf "$d1" has_snapshot)" = true ] || fail "d1 stop without snapshot"
t=$(now); bd POST "/v1/vms/$d1/start" '{}' >/dev/null; dresume_s=$(secs "$t" "$(now)")
[ "$(bx "$d1" 'sha256sum /dev/shm/d/dirty | cut -c1-64')" = "$dsum" ] || fail "d1 lost 2 GiB of memory across stop/start"
bx "$d1" 'head -c 200M /dev/urandom >/dev/shm/d/dirty2'
t=$(now); bd POST "/v1/vms/$d1/stop" >/dev/null; dstop2_s=$(secs "$t" "$(now)")
bd POST "/v1/vms/$d1/start" '{}' >/dev/null
t=$(now); d2=$(bd POST "/v1/vms/$d1/fork" '{"name":"d2"}' | jq -r .id); dfork_s=$(secs "$t" "$(now)")
[ "$(bx "$d2" 'sha256sum /dev/shm/d/dirty | cut -c1-64')" = "$dsum" ] || fail "fork lost the parent's memory"
echo "default size: create ${dcreate_s}s, first stop ${dstop_s}s (2 GiB dirty), resume ${dresume_s}s, stop ${dstop2_s}s (200 MiB dirty), fork ${dfork_s}s"
check_max default_create "$dcreate_s" "$DEFAULT_CREATE_MAX"
check_max default_stop1 "$dstop_s" "$DEFAULT_STOP_MAX"
check_max default_resume "$dresume_s" "$RESUME_MAX"
check_max default_stop2 "$dstop2_s" "$STOP_MAX"
check_max default_fork "$dfork_s" "$FORK_MAX"
# Nested KVM spike: what the host offers and what the guest sees (a measurement, not a gate).
nested_host=$(cat /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested 2>/dev/null | head -1 || true)
nested_guest=$(bx "$d1" 'grep -cwE "vmx|svm" /proc/cpuinfo || true')
kvm_guest=$(bx "$d1" 'test -e /dev/kvm && echo yes || echo no')
echo "nested KVM: host nested=${nested_host:-absent}, guest cpus with vmx/svm=$nested_guest, guest /dev/kvm=$kvm_guest"
bd DELETE "/v1/vms/$d2" >/dev/null

log "limits e2e: resize"
bx "$d1" 'df --output=size -BG / | tail -1' | tr -d ' G' >"$WORK/tmp/df0"
bd POST "/v1/vms/$d1/resize" '{"disk_gib":20}' >/dev/null
[ "$(bx "$d1" 'blockdev --getsize64 /dev/vda')" = $((20 << 30)) ] || fail "guest does not see the grown disk"
[ "$(bx "$d1" 'df --output=size -BG / | tail -1' | tr -d ' G')" -gt "$(cat "$WORK/tmp/df0")" ] || fail "guest filesystem did not grow"
[ "$(bdcode POST "/v1/vms/$d1/resize" '{"disk_gib":10}')" = 400 ] || fail "disk shrink accepted"
[ "$(bdcode POST "/v1/vms/$d1/resize" '{"size":"e2e"}')" = 409 ] || fail "RAM change on a running VM accepted"
bd POST "/v1/vms/$d1/stop" >/dev/null
bd POST "/v1/vms/$d1/resize" '{"disk_gib":24}' >/dev/null # across a restore: memory stays, the guest is told
bd POST "/v1/vms/$d1/start" '{}' >/dev/null
[ "$(bx "$d1" 'sha256sum /dev/shm/d/dirty | cut -c1-64')" = "$dsum" ] || fail "disk grow lost the stop snapshot"
[ "$(bx "$d1" 'blockdev --getsize64 /dev/vda')" = $((24 << 30)) ] || fail "restored guest does not see the grown disk"
bd POST "/v1/vms/$d1/stop" >/dev/null
[ "$(bdcode POST "/v1/vms/$d1/resize" '{"size":"e2e"}')" = 409 ] || fail "memory discarded without asking"
bd POST "/v1/vms/$d1/resize" '{"size":"e2e","discard_memory":true}' >/dev/null
bd POST "/v1/vms/$d1/start" '{}' >/dev/null
[ "$(bx "$d1" nproc)" = 2 ] || fail "resize: vCPUs"
mem_kib=$(bx "$d1" "awk '/MemTotal/ {print \$2}' /proc/meminfo")
[ "$mem_kib" -lt $((2200 * 1024)) ] || fail "resize: RAM $mem_kib"
[ "$(bx "$d1" 'blockdev --getsize64 /dev/vda')" = $((24 << 30)) ] || fail "a smaller size must keep the larger disk"

log "limits e2e: agent passthrough"
[ "$(bdcode POST "/v1/vms/$d1/agent" '{"op":"shutdown"}')" = 400 ] || fail "agent passthrough allows shutdown"
[ "$(bdcode POST "/v1/vms/$d1/agent" '{"op":"exec","argv":["id"]}')" = 400 ] || fail "agent passthrough allows exec"
code=$(bdcode POST "/v1/vms/$d1/agent" '{"op":"file_list","path":"/etc/boat"}')
if grep -q 'unknown op' "$WORK/tmp/bd.out"; then
  echo "::warning::this guest image has no cmd_*/file_* ops yet; passthrough checked only as far as the guest"
else
  [ "$code" = 200 ] && jq -e '.ok and (.entries | type == "array")' "$WORK/tmp/bd.out" >/dev/null || fail "file_list: $code $(cat "$WORK/tmp/bd.out")"
  cid=$(bd POST "/v1/vms/$d1/agent" '{"op":"cmd_start","argv":["bash","-c","echo agent-ok; sleep 300"]}' | jq -r .cmd_id)
  [ -n "$cid" ] && [ "$cid" != null ] || fail "cmd_start gave no cmd_id"
  sleep 1
  bd POST "/v1/vms/$d1/agent" "{\"op\":\"cmd_logs\",\"cmd_id\":\"$cid\"}" | jq -r .data_b64 | base64 -d | grep -q agent-ok || fail "cmd_logs"
  bd POST "/v1/vms/$d1/agent" "{\"op\":\"cmd_signal\",\"cmd_id\":\"$cid\",\"signal\":\"KILL\"}" >/dev/null
  [ "$(bdcode POST "/v1/vms/$d1/agent" '{"op":"cmd_status","cmd_id":"nope"}')" = 404 ] || fail "unknown cmd_id: $(cat "$WORK/tmp/bd.out")"
fi

log "limits e2e: snapshot export and import"
bx "$d1" 'echo export-marker >/root/marker; sync'
boot_d1=$(bx "$d1" 'cat /proc/sys/kernel/random/boot_id')
exp=$(bd POST "/v1/vms/$d1/snapshots" '{"name":"exp"}' | jq -r .id)
t=$(now)
curl -fsS --unix-socket "$SOCK" -o "$WORK/tmp/exp.tar.zst" "http://boatd/v1/vms/$d1/snapshots/$exp/download"
export_s=$(secs "$t" "$(now)")
tar --zstd -tf "$WORK/tmp/exp.tar.zst" | grep -qx rootfs.ext4 || fail "export has no disk"
echo "export ${export_s}s, $(($(stat -c %s "$WORK/tmp/exp.tar.zst") >> 20)) MiB"
[ "$(bdcode GET "/v1/vms/vm-nope/snapshots/$exp/download")" = 404 ] || fail "export of another VM's snapshot"
imported() { curl -fsS --unix-socket "$SOCK" -X POST --data-binary "@$1" "http://boatd/v1/snapshots/import?team=lim&name=$2"; }
in1=$(imported "$WORK/tmp/exp.tar.zst" signed)
[ "$(jq -r .has_mem <<<"$in1")" = true ] || fail "signed import lost its memory: $in1"
i1=$(newvm "{\"name\":\"i1\",\"team\":\"lim\",\"snapshot\":\"$(jq -r .id <<<"$in1")\"}")
[ "$(bx "$i1" 'cat /proc/sys/kernel/random/boot_id')" = "$boot_d1" ] || fail "signed import did not restore memory"
bd DELETE "/v1/vms/$i1" >/dev/null
# The same archive without its signature is just somebody's upload.
mkdir -p "$WORK/tmp/x" && tar --zstd -xSf "$WORK/tmp/exp.tar.zst" -C "$WORK/tmp/x"
rm "$WORK/tmp/x/export.json"
tar --zstd -cSf "$WORK/tmp/unsigned.tar.zst" -C "$WORK/tmp/x" rootfs.ext4 vmlinux vmstate mem
rm -rf "$WORK/tmp/x"
in2=$(imported "$WORK/tmp/unsigned.tar.zst" unsigned)
[ "$(jq -r .has_mem <<<"$in2")" = false ] || fail "unsigned import kept its memory: $in2"
i2=$(newvm "{\"name\":\"i2\",\"team\":\"lim\",\"snapshot\":\"$(jq -r .id <<<"$in2")\"}")
[ "$(bx "$i2" 'cat /root/marker')" = export-marker ] || fail "unsigned import lost the disk"
[ "$(bx "$i2" 'cat /proc/sys/kernel/random/boot_id')" != "$boot_d1" ] || fail "unsigned import restored memory"
# A path-traversal archive is refused and leaves nothing behind.
tar -cf "$WORK/tmp/evil.tar" -P --transform 's,.*,../../evil,' "$WORK/tmp/df0"
zstd -qf "$WORK/tmp/evil.tar" -o "$WORK/tmp/evil.tar.zst"
[ "$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCK" -X POST --data-binary "@$WORK/tmp/evil.tar.zst" "http://boatd/v1/snapshots/import?team=lim")" = 400 ] || fail "traversal archive accepted"
rm -f "$WORK"/tmp/*.tar.zst "$WORK/tmp/evil.tar"
for s in "$(jq -r .id <<<"$in1")" "$(jq -r .id <<<"$in2")"; do bd DELETE "/v1/snapshots/$s" >/dev/null; done
bd DELETE "/v1/vms/$i2" >/dev/null

log "limits e2e: tenant router advertisement"
cat >"$WORK/tmp/ra.py" <<'PY'
import socket, struct
s = socket.socket(socket.AF_INET6, socket.SOCK_RAW, socket.IPPROTO_ICMPV6)
s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_MULTICAST_HOPS, 255)
s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, b"eth0")
ifi = socket.if_nametoindex("eth0")
ra = struct.pack("!BBHBBHII", 134, 0, 0, 64, 0, 1800, 0, 0)
pfx = struct.pack("!BBBBIII", 3, 4, 64, 0xC0, 86400, 14400, 0) + socket.inet_pton(socket.AF_INET6, "fd77:dead::")
for _ in range(5):
    s.sendto(ra + pfx, ("ff02::1", 0, 0, ifi))
PY
put_file "$d1" /tmp/ra.py "$WORK/tmp/ra.py"
routes_before=$(ip -6 route show | grep -v '^fe80' | sort)
ra_count() { sudo nft list table bridge boat | awk '/nd-router-advert/ { for (i = 1; i < NF; i++) if ($i == "packets") print $(i + 1) }'; }
ra0=$(ra_count)
[ "$(cat /proc/sys/net/ipv6/conf/boatbr0/accept_ra)" = 0 ] || fail "accept_ra on boatbr0"
bx "$d1" 'python3 /tmp/ra.py' || fail "guest could not send its RA"
sleep 2
ra1=$(ra_count)
[ "${ra1:-0}" -gt "${ra0:-0}" ] || fail "the RA never reached the drop rule ($ra0 -> $ra1): the test proves nothing"
[ "$(ip -6 route show | grep -v '^fe80' | sort)" = "$routes_before" ] || fail "a tenant RA changed the host's routes: $(ip -6 route show)"
ip -6 addr show dev boatbr0 | grep -q fd77 && fail "a tenant RA configured an address on the host"
echo "RA: $((ra1 - ra0)) frames dropped, host routes unchanged"

log "limits e2e: a bridge port without rules passes nothing"
sudo ip netns add e2e-rogue
sudo ip link add bt65000 type veth peer name eth0 netns e2e-rogue
sudo ip -n e2e-rogue addr add 10.200.250.250/16 dev eth0
sudo ip -n e2e-rogue link set eth0 up
sudo ip link set bt65000 master boatbr0 up
sleep 1
if sudo ip netns exec e2e-rogue ping -c 2 -W 1 10.200.0.1 >/dev/null 2>&1; then fail "an unknown bt* port reached the gateway"; fi
bx "$d1" 'ping -c 1 -W 2 10.200.0.1 >/dev/null' || fail "control: a known VM cannot ping the gateway"
sudo ip link del bt65000
sudo ip netns del e2e-rogue

log "limits e2e: ports"
cat >"$WORK/tmp/srv.sh" <<'SH'
#!/bin/bash
mkdir -p /dev/shm/www && cd /dev/shm/www
[ -f big ] || head -c 300M /dev/urandom >big
echo ok >index.html
setsid nohup python3 -m http.server 8082 --bind 0.0.0.0 >/dev/null 2>&1 </dev/null &
SH
put_file "$d1" /tmp/srv.sh "$WORK/tmp/srv.sh"
bx "$d1" '/tmp/srv.sh'
ip_d1=$(vmf "$d1" ip)
wait_http "http://$ip_d1:8082/"
hp=$(bd POST "/v1/vms/$d1/ports" '{"port":8082,"public":true}' | jq -r .host_port)
curl -fsS --max-time 5 -o /dev/null "http://$host_ip:$hp/" || fail "published port"
[ "$(bd POST "/v1/vms/$d1/ports" '{"port":8082,"public":false}' | jq -r '.host_port // 0')" = 0 ] || fail "public:false kept the host port"
if curl -fsS --max-time 3 -o /dev/null "http://$host_ip:$hp/" 2>/dev/null; then fail "port still open after public:false"; fi
jq -e --arg id "$d1" '.vms[$id].public | index(8082) | not' "$WORK/run/routes.json" >/dev/null || fail "routes.json still lists 8082 as public"
for pn in $(seq 1 40); do
  code=$(bdcode POST "/v1/vms/$d1/ports" "{\"port\":$((9000 + pn))}")
  [ "$code" = 201 ] || break
done
[ "$code" = 429 ] || fail "no per-VM port cap: last code $code"
echo "per-VM port cap hit after $pn ports"
bd GET "/v1/vms/$d1" | jq -r '.ports[].guest_port' | while read -r gp; do bd DELETE "/v1/vms/$d1/ports?port=$gp" >/dev/null; done

log "limits e2e: egress cap, measured"
transfer() { curl -fsS --max-time 60 -o /dev/null -w '%{speed_download}' "http://$ip_d1:8082/big"; }
capped=$(transfer)
restart_boatd "$(cfg_with 'del(.egressMbit)' nocap)"
uncapped=$(transfer)
restart_boatd "$WORK/boatd.json"
capped_mbit=$(mbps "$capped") uncapped_mbit=$(mbps "$uncapped")
echo "egress: cap ${EGRESS_MBIT} Mbit/s, measured ${capped_mbit} Mbit/s; without the cap ${uncapped_mbit} Mbit/s"
[ "$capped_mbit" -le $((EGRESS_MBIT * 115 / 100)) ] || fail "egress ${capped_mbit} Mbit/s over the ${EGRESS_MBIT} Mbit/s cap"
[ "$capped_mbit" -ge $((EGRESS_MBIT * 30 / 100)) ] || fail "egress ${capped_mbit} Mbit/s: the cap strangles traffic"
[ "$uncapped_mbit" -ge $((EGRESS_MBIT * 150 / 100)) ] || fail "control: uncapped ${uncapped_mbit} Mbit/s is too slow to show the cap works"

log "limits e2e: per-VM connection limit"
cat >"$WORK/tmp/conns.py" <<'PY'
import socket, sys, time
socks, ok = [], 0
for _ in range(int(sys.argv[1])):
    s = socket.socket()
    s.settimeout(1.5)
    try:
        s.connect(("1.1.1.1", 443))
        socks.append(s)
        ok += 1
    except OSError:
        pass
print(ok)
PY
put_file "$d1" /tmp/conns.py "$WORK/tmp/conns.py"
free_conns=$(bx "$d1" 'python3 /tmp/conns.py 30')
[ "$free_conns" -ge 28 ] || fail "control: only $free_conns of 30 connections without the limit"
restart_boatd "$(cfg_with '.ctPerVM = 10' ct)"
limited=$(bx "$d1" 'python3 /tmp/conns.py 30')
restart_boatd "$WORK/boatd.json"
echo "connections: $free_conns/30 without the limit, $limited/30 with ctPerVM=10"
[ "$limited" -le 12 ] || fail "ctPerVM=10 let $limited connections through"

log "limits e2e: disk limits"
restart_boatd "$(cfg_with '.diskMBps = 50 | .diskIOPS = 0' disk)"
l1=$(newvm '{"name":"l1","team":"lim"}')
speed=$(bx "$l1" 'dd if=/dev/zero of=/home/dev/x bs=1M count=400 oflag=direct 2>&1 | tail -1' | sed -E 's/.*, ([0-9.]+) MB\/s.*/\1/')
bd DELETE "/v1/vms/$l1" >/dev/null
restart_boatd "$WORK/boatd.json"
echo "drive limiter: 50 MiB/s configured, guest measured ${speed} MB/s"
awk -v s="$speed" 'BEGIN { exit !(s > 10 && s <= 65) }' || fail "drive limiter: guest wrote at $speed MB/s"
l2=$(newvm '{"name":"l2","team":"lim"}')
grep -q 'wbps=838860800' "/sys/fs/cgroup/boat-e2e/$l2/io.max" || fail "io.max: '$(cat "/sys/fs/cgroup/boat-e2e/$l2/io.max")'"
bd DELETE "/v1/vms/$l2" >/dev/null

log "limits e2e: serial log cap"
flog=$STATE/vms/$d1/firecracker.log
size0=$(sudo stat -c %s "$flog")
bx "$d1" "timeout 8 bash -c 'while :; do head -c 65536 /dev/zero | tr \"\\0\" x >/dev/ttyS0; done' || true"
serial_rate=$(( ($(sudo stat -c %s "$flog") - size0) / 8 ))
sudo truncate -s 20M "$flog" # past the cap, without waiting at the guest's rate
for _ in $(seq 20); do [ "$(sudo stat -c %s "$flog")" -lt $((16 << 20)) ] && break; sleep 0.25; done
[ "$(sudo stat -c %s "$flog")" -lt $((16 << 20)) ] || fail "firecracker.log not capped: $(sudo stat -c %s "$flog")"
sudo test -f "$flog.1" || fail "no rotated tail"
echo "serial: a noisy guest writes $((serial_rate >> 10)) KiB/s; the log rotates at 16 MiB within 1 s"

log "limits e2e: public IPv4 pool"
sudo ip -n e2e-ext addr add 198.51.100.1/32 dev lo
sudo ip route add 198.51.100.1/32 via 10.250.0.2
restart_boatd "$(cfg_with '.publicIPv4 = ["192.0.2.10"]' pub)"
p4=$(newvm '{"name":"p4","team":"lim"}')
[ "$(vmf "$p4" public_ip)" = 192.0.2.10 ] || fail "no public address: $(bd GET "/v1/vms/$p4")"
put_file "$p4" /tmp/srv.sh "$WORK/tmp/srv.sh"
bx "$p4" '/tmp/srv.sh; sleep 1'
[ "$(sudo ip netns exec e2e-ext curl -fsS --max-time 8 http://192.0.2.10:8082/)" = ok ] || fail "public IPv4 inbound"
: >"$WORK/ext.log"
sudo ip netns exec e2e-ext python3 -u -m http.server 8001 --bind 0.0.0.0 >>"$WORK/ext.log" 2>&1 &
sleep 1
bx "$p4" 'curl -fsS --max-time 8 -o /dev/null http://198.51.100.1:8001/' || fail "public IPv4 outbound"
grep -q '^192\.0\.2\.10 ' "$WORK/ext.log" || fail "outbound source is not the public IP: $(cat "$WORK/ext.log")"
sudo pkill -f 'http.server 8001' || true
bd DELETE "/v1/vms/$p4" >/dev/null
restart_boatd "$WORK/boatd.json"
sudo ip route del 198.51.100.1/32 || true
[ -z "$(vmf "$d1" public_ip | grep -v null)" ] || fail "an existing VM got the pool address"

log "limits e2e: crash under real systemd (KillMode=mixed, Delegate=yes)"
sd_start() { # CONFIG
  sudo kill -9 "$(cat "$WORK/boatd.pid")" 2>/dev/null || true
  sudo systemd-run --quiet --unit=boatd-e2e --collect -p KillMode=mixed -p Delegate=yes -p Type=exec \
    -p "StandardOutput=append:$WORK/boatd.log" -p "StandardError=append:$WORK/boatd.log" "$WORK/bin/boatd" -config "$1"
  for _ in $(seq 100); do curl -fs --unix-socket "$SOCK" http://boatd/v1/health >/dev/null && return; sleep 0.1; done
  fail "boatd did not start under systemd"
}
sd_crash() { # SIGKILL the main process, like a crash; wait until systemd has cleaned the unit up
  sudo kill -9 "$(systemctl show -p MainPID --value boatd-e2e)"
  for _ in $(seq 100); do [ "$(systemctl is-active boatd-e2e 2>/dev/null)" = active ] || break; sleep 0.1; done
  sleep 1
  sudo systemctl reset-failed boatd-e2e 2>/dev/null || true
}
# VMs in a cgroup outside the unit (the default): the crash does not touch them.
sd_start "$WORK/boatd.json"
crash_vm=$(newvm '{"name":"cr","team":"lim"}')
bx "$crash_vm" 'echo crash-marker >/root/marker; sync'
cr_boot=$(bx "$crash_vm" 'cat /proc/sys/kernel/random/boot_id')
cr_pid=$(vmf "$crash_vm" pid)
sd_crash
sudo kill -0 "$cr_pid" 2>/dev/null || fail "a crash of boatd killed a VM outside its unit"
sd_start "$WORK/boatd.json"
[ "$(vmf "$crash_vm" state)" = running ] || fail "VM not adopted after the crash"
[ "$(bx "$crash_vm" 'cat /proc/sys/kernel/random/boot_id')" = "$cr_boot" ] || fail "VM lost its memory across the crash"
# VMs inside the unit (how a delegated parent behaves): systemd kills them with it,
# and the next boatd brings them back cold, with their disk.
inside=system.slice/boatd-e2e.service/vms
sudo systemctl stop boatd-e2e # snapshots d1 and cr; the delete below needs no running VM
sd_start "$(cfg_with ".cgroupParent = \"$inside\"" inside)"
bd DELETE "/v1/vms/$crash_vm" >/dev/null
crash_vm=$(newvm '{"name":"cr2","team":"lim"}')
bx "$crash_vm" 'echo crash-marker >/root/marker; sync'
cr_boot=$(bx "$crash_vm" 'cat /proc/sys/kernel/random/boot_id')
pid_inside=$(vmf "$crash_vm" pid)
grep -q "boatd-e2e.service" "/proc/$pid_inside/cgroup" || fail "control: the VM is not inside the unit: $(cat "/proc/$pid_inside/cgroup")"
sd_crash
if sudo kill -0 "$pid_inside" 2>/dev/null; then fail "control: KillMode=mixed left the VM alive; this test proves nothing"; fi
sd_start "$WORK/boatd-inside.json"
for _ in $(seq 150); do [ "$(vmf "$crash_vm" state) $(vmf "$crash_vm" agent_ready)" = "running true" ] && break; sleep 0.2; done
[ "$(vmf "$crash_vm" state) $(vmf "$crash_vm" agent_ready)" = "running true" ] || fail "crashed VM not resumed: $(bd GET "/v1/vms/$crash_vm")"
[ "$(bx "$crash_vm" 'cat /root/marker')" = crash-marker ] || fail "crashed VM lost its disk"
[ "$(bx "$crash_vm" 'cat /proc/sys/kernel/random/boot_id')" != "$cr_boot" ] || fail "expected a cold boot"
bd DELETE "/v1/vms/$crash_vm" >/dev/null
sudo systemctl stop boatd-e2e
sudo systemctl reset-failed boatd-e2e 2>/dev/null || true
sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >>"$4" 2>&1' _ "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.json" "$WORK/boatd.log" &
for _ in $(seq 300); do curl -fs --unix-socket "$SOCK" http://boatd/v1/health >/dev/null && break; sleep 0.1; done
for _ in $(seq 150); do [ "$(vmf "$d1" state)" = running ] && break; sleep 0.2; done
bd DELETE "/v1/vms/$d1" >/dev/null

log "limits e2e: a really full disk (small btrfs)"
SMALL=/var/lib/boat-e2e-small
truncate -s 8G "$WORK/small.img" && mkfs.btrfs -q -f "$WORK/small.img"
sudo mkdir -p "$SMALL" && sudo mount -o loop "$WORK/small.img" "$SMALL"
sudo mkdir -p "$SMALL/images/slim"
sudo cp --sparse=always "$STATE/images/slim/vmlinux" "$STATE/images/slim/rootfs.ext4" "$SMALL/images/slim/"
sudo sync
restart_boatd "$(cfg_with ".stateDir = \"$SMALL\" | .minFreeGiB = 1 | .minFreePct = 0" small)"
fill=$(newvm '{"name":"fill","team":"lim"}')
# Past what the host has: the guest gets I/O errors, not the host.
bx "$fill" 'head -c 7G /dev/urandom >/home/dev/fill; sync' >/dev/null 2>&1 || true
sudo sync
avail=$(df -B1M --output=avail "$SMALL" | tail -1 | tr -d ' ')
echo "small btrfs after the fill: ${avail} MiB available"
[ "$avail" -lt 1024 ] || fail "the fill left $avail MiB: the disk never got full"
curl -fsS --unix-socket "$SOCK" http://boatd/v1/health >/dev/null || fail "boatd died on ENOSPC"
[ "$(bdcode POST /v1/vms '{"name":"over","team":"lim"}')" = 507 ] || fail "create on a full disk: $(cat "$WORK/tmp/bd.out")"
[ "$(bdcode POST "/v1/vms/$fill/snapshots" '{}')" = 507 ] || fail "snapshot on a full disk: $(cat "$WORK/tmp/bd.out")"
st=$(bd POST "/v1/vms/$fill/stop" | jq -r '.state + " " + (.has_snapshot|tostring)')
[ "$st" = "stopped false" ] || fail "stop on a full disk: $st"
bd DELETE "/v1/vms/$fill" >/dev/null
sudo btrfs filesystem sync "$SMALL"
ok=""
for _ in $(seq 60); do
  [ "$(df -B1M --output=avail "$SMALL" | tail -1 | tr -d ' ')" -gt 1100 ] && { ok=1; break; }
  sleep 1
done
[ -n "$ok" ] || fail "space not reclaimed after the delete"
again=$(newvm '{"name":"again","team":"lim"}')
[ "$(vmf "$again" agent_ready)" = true ] || fail "no recovery after ENOSPC: $(bd GET "/v1/vms/$again")"
bd DELETE "/v1/vms/$again" >/dev/null
restart_boatd "$WORK/boatd.json"
sudo umount -l "$SMALL"
rm -f "$WORK/small.img"
