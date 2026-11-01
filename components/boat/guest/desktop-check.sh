#!/usr/bin/env bash
# Check a boat desktop link end to end, the way a browser uses it, and measure
# the stream's frame rate.
#
#   desktop-check.sh LINK [SECONDS=5]
#
# 1. curl follows LINK's redirects and meta refreshes with a cookie jar ($CURL_OPTS is added, e.g.
#    "--cacert ca.pem --resolve h:443:127.0.0.1" or "--connect-to h:p:127.0.0.1:p").
# 2. Opens the KasmVNC websocket at <final page>/websockify with the cookies
#    (python3 stdlib; --resolve/--connect-to/--cacert/-k in CURL_OPTS and
#    $SSL_CA are honoured), does the RFB handshake (security None) and asks for
#    raw 8bpp updates.
# 3. Passes once the first FramebufferUpdate arrives, then counts non-empty
#    updates for SECONDS and prints
#      METRIC desktop_first_frame_ms=N
#      METRIC desktop_raw_8bpp_fps=F    server side only, not what a user sees
# 4. If Chrome is here, desktop-browser-fps.py opens LINK in the real web client
#    and prints desktop_browser_fps, which is what a user sees. It fails below
#    MIN_BROWSER_FPS or MIN_DECODED_FPS (defaults below). DESKTOP_BROWSER=0 skips this.
# $ANIMATE_CMD, if set, runs in a loop in the background (e.g.
# "boat exec VM -- boat-desktop-animate 10") so there is something to stream.
set -euo pipefail
LINK=${1:?usage: desktop-check.sh LINK [SECONDS]}
SECS=${2:-5}
HERE=$(cd "$(dirname "$0")" && pwd)
# Regression gates, not targets (README.md has what is measured). The default
# configuration shows 26+ and decodes 34+ on CI; stock client settings show about 20
# and decode nothing, so they fail the second gate.
MIN_BROWSER_FPS=${MIN_BROWSER_FPS:-20}
MIN_DECODED_FPS=${MIN_DECODED_FPS:-25}
W=$(mktemp -d)
trap 'rm -rf "$W"; [[ -n ${anim:-} ]] && kill "$anim" 2>/dev/null; true' EXIT

# The gateway answers the signed link with a 200 meta-refresh page (so its
# session cookie survives the cross-site entry); a browser follows that too.
meta_refresh() {
  python3 -I - "$1" "$2" <<'PY'
import html, re, sys
from urllib.parse import urljoin
m = re.search(r"""<meta[^>]+http-equiv=["']?refresh["']?[^>]*content=["']\s*\d+\s*;\s*url=([^"'>]+)""",
              open(sys.argv[2], errors="replace").read(), re.I)
if m:
    print(urljoin(sys.argv[1], html.unescape(m.group(1).strip())))
PY
}
url=$LINK
for _ in 1 2 3 4 5; do
  # shellcheck disable=SC2086
  read -r final code < <(curl -sS -L --max-redirs 10 --max-time 30 -c "$W/jar" -b "$W/jar" \
    ${CURL_OPTS:-} -o "$W/page.html" -w '%{url_effective} %{http_code}\n' "$url")
  [[ $code == 200 ]] || { echo "desktop-check: $url ended at $final with HTTP $code" >&2; exit 1; }
  url=$(meta_refresh "$final" "$W/page.html")
  [[ -n $url ]] || break
done
[[ -z $url ]] || { echo "desktop-check: too many meta refreshes, last $url" >&2; exit 1; }
grep -qiE '<html|<!doctype html' "$W/page.html" || { echo "desktop-check: $final is not an html page" >&2; exit 1; }
echo "desktop-check: page $final (HTTP $code)"

if [[ -n ${ANIMATE_CMD:-} ]]; then
  ( while :; do bash -c "$ANIMATE_CMD" || sleep 1; done ) >"$W/animate.log" 2>&1 &
  anim=$!
  sleep 2
fi

python3 -I - "$final" "$W/jar" "$SECS" <<'PY'
import base64, os, shlex, socket, ssl, struct, sys, time
from urllib.parse import urljoin, urlsplit

final, jar, secs = sys.argv[1], sys.argv[2], float(sys.argv[3])
opts = shlex.split(os.environ.get("CURL_OPTS", ""))

def opt_values(*names):
    return [opts[i + 1] for i, o in enumerate(opts[:-1]) if o in names]

def die(msg):
    print("desktop-check: " + msg, file=sys.stderr)
    sys.exit(1)

ws_url = urlsplit(urljoin(final, "websockify"))
host, tls = ws_url.hostname, ws_url.scheme == "https"
port = ws_url.port or (443 if tls else 80)
addr = (host, port)
for r in opt_values("--resolve"):                      # HOST:PORT:ADDR[,ADDR]
    h, p, a = r.split(":", 2)
    if h in (host, "*") and int(p) == port:
        addr = (a.split(",")[0].strip("[]"), port)
for c in opt_values("--connect-to"):                   # H1:P1:H2:P2
    h1, p1, h2, p2 = c.rsplit(":", 3)
    if h1 in ("", host) and (p1 == "" or int(p1) == port):
        addr = (h2 or addr[0], int(p2) if p2 else port)

cookies = []
for line in open(jar):
    if line.startswith("#HttpOnly_"):
        line = line[len("#HttpOnly_"):]
    elif line.startswith("#") or not line.strip():
        continue
    f = line.rstrip("\n").split("\t")
    if len(f) == 7:
        dom = f[0].lstrip(".")
        if (host == dom or host.endswith("." + dom)) and ws_url.path.startswith(f[2]):
            cookies.append("%s=%s" % (f[5], f[6]))

t0 = time.monotonic()
sock = socket.create_connection(addr, timeout=20)
if tls:
    cafile = (opt_values("--cacert") or [os.environ.get("SSL_CA")])[-1]
    ctx = ssl.create_default_context(cafile=cafile) if cafile else ssl.create_default_context()
    if "-k" in opts or "--insecure" in opts:
        ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
    sock = ctx.wrap_socket(sock, server_hostname=host)

key = base64.b64encode(os.urandom(16)).decode()
path = ws_url.path + ("?" + ws_url.query if ws_url.query else "")
hostport = host if ws_url.port is None else "%s:%d" % (host, port)
req = ("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
       "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\n"
       "Origin: %s://%s\r\n" % (path, hostport, key, ws_url.scheme, hostport))
if cookies:
    req += "Cookie: %s\r\n" % "; ".join(cookies)
sock.sendall((req + "\r\n").encode())
head = b""
while b"\r\n\r\n" not in head:
    chunk = sock.recv(4096)
    if not chunk:
        die("websocket: connection closed during handshake")
    head += chunk
head, buf = head.split(b"\r\n\r\n", 1)
status = head.split(b"\r\n", 1)[0].decode("latin-1")
if " 101 " not in status + " ":
    die("websocket handshake to %s://%s%s: %s" % (ws_url.scheme, hostport, path, status))

class WS:
    def __init__(self, sock, buf):
        self.sock, self.raw, self.data = sock, bytearray(buf), bytearray()

    def _raw(self, n):
        while len(self.raw) < n:
            chunk = self.sock.recv(1 << 16)
            if not chunk:
                raise EOFError("websocket closed")
            self.raw += chunk
        out = bytes(self.raw[:n]); del self.raw[:n]
        return out

    def _frame(self):
        b0, b1 = self._raw(2)
        n = b1 & 0x7F
        if n == 126:
            n, = struct.unpack(">H", self._raw(2))
        elif n == 127:
            n, = struct.unpack(">Q", self._raw(8))
        mask = self._raw(4) if b1 & 0x80 else None
        payload = self._raw(n)
        if mask:
            payload = bytes(c ^ mask[i % 4] for i, c in enumerate(payload))
        op = b0 & 0x0F
        if op in (0, 1, 2):
            self.data += payload
        elif op == 8:
            raise EOFError("websocket close frame")
        elif op == 9:
            self.send(payload, opcode=10)

    def read(self, n):
        while len(self.data) < n:
            self._frame()
        out = bytes(self.data[:n]); del self.data[:n]
        return out

    def send(self, payload, opcode=2):
        mask = os.urandom(4)
        n = len(payload)
        hdr = bytes([0x80 | opcode]) + (bytes([0x80 | n]) if n < 126 else
              bytes([0x80 | 126]) + struct.pack(">H", n))
        self.sock.sendall(hdr + mask + bytes(c ^ mask[i % 4] for i, c in enumerate(payload)))

ws = WS(sock, buf)
version = ws.read(12)
if not version.startswith(b"RFB "):
    die("not an RFB server: %r" % version)
ws.send(b"RFB 003.008\n")
ntypes = ws.read(1)[0]
if ntypes == 0:
    die("server refused: %r" % ws.read(struct.unpack(">I", ws.read(4))[0]))
types = list(ws.read(ntypes))
if 1 not in types:
    die("security type None not offered (got %r)" % types)
ws.send(b"\x01")
if struct.unpack(">I", ws.read(4))[0] != 0:
    die("security result failed")
ws.send(b"\x01")                                        # ClientInit: shared
fb_w, fb_h = struct.unpack(">HH", ws.read(4))
ws.read(16)
name = ws.read(struct.unpack(">I", ws.read(4))[0]).decode("utf-8", "replace")
ws.send(struct.pack(">B3xBBBBHHHBBB3x", 0, 8, 8, 0, 1, 7, 7, 3, 0, 3, 6))  # 8bpp bgr233
ws.send(struct.pack(">BxHii", 2, 2, 0, -223))           # Raw, DesktopSize
ws.send(struct.pack(">BBHHHH", 3, 0, 0, 0, fb_w, fb_h))

first_ms, frames, t_first, stop = None, 0, None, None
try:
    while True:
        if stop is not None:
            left = stop - time.monotonic()
            if left <= 0:
                break
            sock.settimeout(left)
        mtype = ws.read(1)[0]
        if mtype == 0:
            _, nrects = struct.unpack(">BH", ws.read(3))
            for _ in range(nrects):
                x, y, w, h, enc = struct.unpack(">HHHHi", ws.read(12))
                if enc == 0:
                    ws.read(w * h)
                elif enc == -223:
                    fb_w, fb_h = w, h
                elif enc == -224:
                    break
                else:
                    raise ValueError("unexpected encoding %d" % enc)
            now = time.monotonic()
            if first_ms is None:
                first_ms, t_first, stop = round((now - t0) * 1000), now, now + secs
                print("desktop-check: first frame %dx%d from %r after %d ms"
                      % (fb_w, fb_h, name, first_ms))
            elif nrects:
                frames += 1
            ws.send(struct.pack(">BBHHHH", 3, 1, 0, 0, fb_w, fb_h))
        elif mtype == 1:
            _, _, n = struct.unpack(">BHH", ws.read(5)); ws.read(n * 6)
        elif mtype == 2:
            pass
        elif mtype == 3:
            ws.read(3); ws.read(struct.unpack(">I", ws.read(4))[0] & 0x7FFFFFFF)
        elif mtype == 150:                              # EndOfContinuousUpdates
            pass
        elif mtype == 248:                              # ServerFence
            ws.read(7); ws.read(ws.read(1)[0])
        else:
            raise ValueError("unexpected server message %d" % mtype)
except socket.timeout:
    pass
except (EOFError, ValueError, OSError) as e:
    if first_ms is None:
        die("stream: %s" % e)
    print("desktop-check: measurement cut short: %s" % e, file=sys.stderr)
if first_ms is None:
    die("no framebuffer update")
elapsed = min(time.monotonic(), stop) - t_first
print("METRIC desktop_first_frame_ms=%d" % first_ms)
print("METRIC desktop_raw_8bpp_fps=%.1f" % (frames / max(elapsed, 1e-3)))
PY
if [[ ${DESKTOP_BROWSER:-1} != 0 ]]; then
  if command -v "${CHROME:-google-chrome}" >/dev/null || command -v chromium >/dev/null; then
    MIN_FPS=$MIN_BROWSER_FPS MIN_DECODED_FPS=$MIN_DECODED_FPS \
      python3 -I "$HERE/desktop-browser-fps.py" "$LINK" "$SECS"
  else
    echo "desktop-check: no Chrome here, so no desktop_browser_fps" >&2
  fi
fi
if [[ -n ${anim:-} ]]; then sort -u "$W/animate.log" 2>/dev/null || true; fi
