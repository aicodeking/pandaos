# Online AuthorizedKeysCommand: jump-keys LOOPBACK_URL %t %k %u.
# No files, routes snapshot, proxy, redirect, cache or offline fallback.
if command -v boat-gateway >/dev/null 2>&1; then
  # Also records the calling sshd connection for boat-jump-reaper (r28).
  exec boat-gateway jump-keys "$@"
fi
# The fallback below does not record sessions, so the reaper could never end
# them: refuse when called by sshd (its parent); keep it for manual runs/tests.
if [ -r "/proc/$PPID/cmdline" ] && tr '\0' ' ' < "/proc/$PPID/cmdline" | grep -q '^sshd'; then
  exit 1
fi
exec python3 -I - "$@" <<'PY'
import base64
import http.client
import ipaddress
import json
import re
import signal
import sys

def deny(*_):
    raise RuntimeError('deny')

try:
    signal.signal(signal.SIGALRM, deny)
    signal.setitimer(signal.ITIMER_REAL, 2.0)
    url, key_type, key_blob, user = sys.argv[1:]
    match = re.fullmatch(r'http://127\.0\.0\.1:([0-9]{1,5})/internal/ssh/authorize', url)
    if not match or not 0 < int(match[1]) < 65536 or user != 'jump':
        deny()
    if not re.fullmatch(r'(ssh|ecdsa|sk)-[A-Za-z0-9@._+-]+', key_type):
        deny()
    raw = base64.b64decode(key_blob, validate=True)
    if len(raw) > 16384 or base64.b64encode(raw).decode() != key_blob:
        deny()
    n = int.from_bytes(raw[:4], 'big')
    if n != len(key_type) or raw[4:4+n] != key_type.encode() or len(raw) <= 4+n:
        deny()
    request = json.dumps(dict(user=user, key_type=key_type, key_blob=key_blob)).encode()
    # Direct numeric loopback: http.client ignores proxy environment variables.
    conn = http.client.HTTPConnection('127.0.0.1', int(match[1]), timeout=0.5)
    conn.connect()
    conn.sock.settimeout(2.0)  # SIGALRM bounds the entire operation, including slow bodies.
    conn.request('POST', '/internal/ssh/authorize', body=request,
                 headers={'Content-Type': 'application/json', 'Cache-Control': 'no-store'})
    response = conn.getresponse()
    if response.status != 200:
        deny()
    lengths = response.headers.get_all('Content-Length', [])
    transfers = response.headers.get_all('Transfer-Encoding', [])
    if (len(lengths) > 1 or len(transfers) > 1 or (lengths and transfers)
            or (lengths and (not re.fullmatch(r'[0-9]+', lengths[0]) or int(lengths[0]) > 65536))
            or (transfers and transfers[0].lower() != 'chunked')):
        deny()
    body = response.read(65537)
    if lengths and len(body) != int(lengths[0]):
        deny()
    if len(body) > 65536:
        deny()
    # Reject duplicate JSON fields rather than choosing an ambiguous interpretation.
    def unique(pairs):
        result = {}
        for k, v in pairs:
            if k in result:
                deny()
            result[k] = v
        return result
    result = json.loads(body, object_pairs_hook=unique)
    if not isinstance(result, dict) or set(result) != {'authorized_keys'}:
        deny()
    lines = result['authorized_keys']
    if not isinstance(lines, list) or len(lines) > 200:
        deny()
    suffix = ' ' + key_type + ' ' + key_blob
    for line in lines:
        if not isinstance(line, str) or not line.endswith(suffix):
            deny()
        options = line[:-len(suffix)]
        targets = re.fullmatch(r'restrict,port-forwarding((?:,permitopen="[0-9.]+:22")+)', options)
        if not targets:
            deny()
        for ip in re.findall(r'permitopen="([0-9.]+):22"', targets[1]):
            addr = ipaddress.IPv4Address(ip)
            if str(addr) != ip or addr.is_unspecified or addr.is_multicast or addr.is_loopback or addr.is_link_local or int(addr) == 0xffffffff:
                deny()
    # No output until the complete body AND every line has passed validation.
    conn.close()
    signal.setitimer(signal.ITIMER_REAL, 0)
    sys.stdout.write(''.join(line + '\n' for line in lines))
except Exception:
    signal.setitimer(signal.ITIMER_REAL, 0)
    sys.exit(1)
PY
