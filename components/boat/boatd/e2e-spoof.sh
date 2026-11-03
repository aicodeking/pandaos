# shellcheck shell=bash
# shellcheck disable=SC2154 # p1, c1, v6 and the helpers come from e2e-host.sh
# Real frames from a guest. First the positive control: without the rules and the pins
# the same frames do repoint the host, so a pass later on means something.
log "host e2e: NDP and ARP spoofing, with real frames from a guest"
cat >"$WORK/tmp/spoof.py" <<'PY'
import socket, struct, sys, time
def mac(m): return bytes.fromhex(m.replace(":", ""))
def csum(b):
    if len(b) % 2: b += b"\0"
    s = sum(struct.unpack("!%dH" % (len(b) // 2), b))
    while s >> 16: s = (s & 0xffff) + (s >> 16)
    return ~s & 0xffff
me = open("/sys/class/net/eth0/address").read().strip()
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3))
s.bind(("eth0", 0))
if sys.argv[1] == "na":  # na SRC TARGET TLLA: an unsolicited NA with the override flag
    src, dst = socket.inet_pton(socket.AF_INET6, sys.argv[2]), socket.inet_pton(socket.AF_INET6, "ff02::1")
    na = struct.pack("!BBHI", 136, 0, 0, 0x20000000) + socket.inet_pton(socket.AF_INET6, sys.argv[3]) + bytes([2, 1]) + mac(sys.argv[4])
    c = csum(src + dst + struct.pack("!I", len(na)) + b"\0\0\0\x3a" + na)
    na = na[:2] + struct.pack("!H", c) + na[4:]
    ip6 = struct.pack("!IHBB", 0x60000000, len(na), 58, 255) + src + dst
    frame = mac("33:33:00:00:00:01") + mac(me) + b"\x86\xdd" + ip6 + na
elif sys.argv[1] == "arp":  # arp SENDER_IP TARGET_IP SENDER_MAC: a gratuitous reply
    frame = b"\xff" * 6 + mac(me) + b"\x08\x06" + struct.pack("!HHBBH", 1, 0x800, 6, 4, 2) + mac(sys.argv[4]) + \
        socket.inet_aton(sys.argv[2]) + b"\0" * 6 + socket.inet_aton(sys.argv[3])
else:  # sniff VICTIM_MAC SECONDS: frames arriving here for that MAC, and for this VM
    victim, mine, end = mac(sys.argv[2]), mac(me), time.time() + float(sys.argv[3])
    s.settimeout(0.2)
    theirs = own = 0
    while time.time() < end:
        try:
            d = s.recv(65535)
        except socket.timeout:
            continue
        theirs += d[:6] == victim
        own += d[:6] == mine
    print(theirs, own)
    sys.exit()
for _ in range(3):
    s.send(frame)
    time.sleep(0.05)
PY
curl -fsS --unix-socket "$SOCK" -X PUT --data-binary "@$WORK/tmp/spoof.py" "http://boatd/v1/vms/$p1/files?path=/tmp/spoof.py&mode=755" >/dev/null
a_ip=$(vmf "$p1" ip) a_mac=$(vmf "$p1" mac) a_v6=$(vmf "$p1" ipv6) a_tap=$(vmf "$p1" tap)
v_ip=$(vmf "$c1" ip) v_mac=$(vmf "$c1" mac) v_v6=$(vmf "$c1" ipv6) v_tap=$(vmf "$c1" tap)
fake=02:de:ad:be:ef:01
neigh6() { ip -6 neigh show "$1" dev boatbr0; }
neigh4() { ip neigh show "$1" dev boatbr0; }
spoof_count() { # TAP PATTERN: packets dropped by that rule
  sudo nft list table bridge boat | awk -v t="\"$1\"" -v p="$2" 'index($0, t) && index($0, p) { for (i = 1; i < NF; i++) if ($i == "packets") print $(i + 1) }'
}
pinned() { grep -q "$2" <<<"$1" && grep -q PERMANENT <<<"$1" || fail "$3: $1"; }
pinned "$(neigh6 "$v_v6")" "$v_mac" "the VM's v6 neighbour is not pinned"
pinned "$(neigh4 "$a_ip")" "$a_mac" "the VM's v4 neighbour is not pinned"

# Control: no L2 rules, ordinary neighbour entries.
sudo nft flush chain bridge boat prerouting
sudo ip -6 neigh replace "$v_v6" lladdr "$v_mac" dev boatbr0 nud reachable
sudo ip neigh replace "$a_ip" lladdr "$a_mac" dev boatbr0 nud reachable
sleep 1.5 # ARP's locktime
bx "$p1" "python3 /tmp/spoof.py na $a_v6 $v_v6 $a_mac; python3 /tmp/spoof.py arp $a_ip 10.200.0.1 $fake"
sleep 0.5
grep -q "$a_mac" <<<"$(neigh6 "$v_v6")" || fail "control: the NA did not repoint the host, so this test proves nothing: $(neigh6 "$v_v6")"
grep -q "$fake" <<<"$(neigh4 "$a_ip")" || fail "control: the ARP did not repoint the host, so this test proves nothing: $(neigh4 "$a_ip")"
echo "control: without the rules and pins a guest repoints the host for another VM's address"

# The real thing. A restart reloads the rules and re-pins the VMs it adopts.
restart_boatd "$WORK/boatd.json"
pinned "$(neigh6 "$v_v6")" "$v_mac" "not re-pinned after a restart"
pinned "$(neigh4 "$a_ip")" "$a_mac" "not re-pinned after a restart"
na0=$(spoof_count "$a_tap" nd-neighbor-advert) arp0=$(spoof_count "$a_tap" 'arp saddr ether')
bx "$p1" "python3 /tmp/spoof.py na $a_v6 $v_v6 $a_mac; python3 /tmp/spoof.py arp $a_ip 10.200.0.1 $fake"
sleep 1
na1=$(spoof_count "$a_tap" nd-neighbor-advert) arp1=$(spoof_count "$a_tap" 'arp saddr ether')
[ "${na1:-0}" -gt "${na0:-0}" ] || fail "the NA never reached its drop rule ($na0 -> $na1)"
[ "${arp1:-0}" -gt "${arp0:-0}" ] || fail "the ARP never reached its drop rule ($arp0 -> $arp1)"
pinned "$(neigh6 "$v_v6")" "$v_mac" "an NA repointed another VM's address"
pinned "$(neigh4 "$a_ip")" "$a_mac" "an ARP repointed a VM's own address"
# Past the rule, a target it may name: the permanent entry alone holds.
bx "$p1" "python3 /tmp/spoof.py na $a_v6 $a_v6 $fake"
sleep 0.5
pinned "$(neigh6 "$a_v6")" "$a_mac" "an NA for its own address changed a permanent entry"
ping -6 -c 2 -W 2 "$v_v6" >/dev/null || fail "the victim is no longer reachable"
echo "spoofing: NA $((na1 - na0)) and ARP $((arp1 - arp0)) frames dropped, permanent entries unchanged"

# Pinned entries mean no resolution, so nothing refreshes the bridge's table: a MAC that
# aged out would flood the VM's traffic to every port. It must be static, and flooding off.
bridge fdb show dev "$v_tap" | grep -i "$v_mac" | grep -q static || fail "no static fdb entry: $(bridge fdb show dev "$v_tap")"
bridge -d link show dev "$v_tap" | grep -q 'learning off' && bridge -d link show dev "$v_tap" | grep -q ' flood off' || fail "port flags: $(bridge -d link show dev "$v_tap")"
bx "$p1" "python3 /tmp/spoof.py sniff $v_mac 4" >"$WORK/tmp/sniff" &
sniffer=$!
sleep 1
ping -c 5 -i 0.2 -W 1 "$v_ip" >/dev/null || fail "host cannot ping the victim"
ping -c 5 -i 0.2 -W 1 "$a_ip" >/dev/null || fail "host cannot ping the sniffer"
wait "$sniffer" || fail "sniffer"
read -r theirs own <"$WORK/tmp/sniff"
[ "$own" -gt 0 ] || fail "control: the sniffer saw none of its own traffic: $(cat "$WORK/tmp/sniff")"
[ "$theirs" = 0 ] || fail "a VM received $theirs frames meant for another"
