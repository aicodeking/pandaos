#!/usr/bin/env python3
"""Measure what a user's browser really shows of a boat desktop (python3 stdlib + Chrome).

    desktop-browser-fps.py LINK [SECONDS=5]

Opens LINK in headless Chrome the way a user does (redirects, meta refresh,
cookies, the real KasmVNC web client) and over SECONDS counts:
  desktop_browser_fps           rAF ticks where the page's canvas pixels changed;
                                this is the frame rate a viewer sees
  desktop_browser_decoded_fps   frames out of WebCodecs VideoDecoder (0 in image mode)
  desktop_browser_raf_fps       the browser's own paint rate, the ceiling on the first
  desktop_browser_stream        WxH of decoded frames, or image:WxH of the canvas
  desktop_browser_stream_mbps   bytes the websocket delivered

Env: CHROME (path), CURL_OPTS (--resolve/--connect-to become Chrome's resolver rules),
WARMUP_S (10: let the stream settle), MIN_FPS (fail below this; default 0).
Certificate errors are ignored: this measures frames, it does not test TLS.
"""
import base64
import json
import os
import shlex
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
from urllib.parse import urlsplit

CDP_CALL_TIMEOUT_S = 30
CONNECT_TIMEOUT_S = 60
SAMPLE_W, SAMPLE_H = 160, 90
VIEW_W, VIEW_H = 1920, 1080
MIN_DESKTOP_W = 1280          # smaller canvases are client UI (graphs), not the desktop

# Runs in every document before the client does.
PROBE = """
(() => {
  const b = window.__boat = {decoded: 0, bytes: 0, w: 0, h: 0, raf: 0, changed: 0, cw: 0, ch: 0, errors: []};
  const V = window.VideoDecoder;
  if (V) {
    window.VideoDecoder = class extends V {
      constructor(init) {
        super({...init, output: f => {
          b.decoded++; b.w = f.codedWidth; b.h = f.codedHeight; init.output(f);
        }});
      }
    };
  }
  const WS = window.WebSocket;
  window.WebSocket = class extends WS {
    constructor(...args) {
      super(...args);
      this.addEventListener('message', e => { b.bytes += e.data.byteLength || e.data.size || e.data.length || 0; });
    }
  };
  addEventListener('error', e => b.errors.push(String(e.message)));
  const sample = document.createElement('canvas');
  sample.width = %d; sample.height = %d;
  let ctx = null, last = null;
  const biggest = () => {
    let best = null, area = 0;
    for (const c of document.querySelectorAll('canvas')) {
      if (c.width * c.height > area) { best = c; area = c.width * c.height; }
    }
    return best;
  };
  const tick = () => {
    b.raf++;
    const c = biggest();
    if (c && c.width >= %d) {
      ctx = ctx || sample.getContext('2d', {willReadFrequently: true});
      ctx.drawImage(c, 0, 0, %d, %d);
      const d = ctx.getImageData(0, 0, %d, %d).data;
      let h = 0;
      for (let i = 0; i < d.length; i += 4) h = (Math.imul(h, 31) + d[i] + (d[i+1] << 8) + (d[i+2] << 16)) | 0;
      if (h !== last) { b.changed++; last = h; }
      b.cw = c.width; b.ch = c.height;
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
})();
""" % (SAMPLE_W, SAMPLE_H, MIN_DESKTOP_W, SAMPLE_W, SAMPLE_H, SAMPLE_W, SAMPLE_H)


def die(msg):
    print("desktop-browser-fps: " + msg, file=sys.stderr)
    sys.exit(1)


def find_chrome():
    for c in (os.environ.get("CHROME"), "google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
        if c and shutil.which(c):
            return shutil.which(c)
    return None


def resolver_rules():
    """Chrome's form of curl's --resolve and --connect-to."""
    opts = shlex.split(os.environ.get("CURL_OPTS", ""))
    rules = []
    for i, o in enumerate(opts[:-1]):
        if o == "--resolve":                     # HOST:PORT:ADDR
            host, _, addr = opts[i + 1].split(":", 2)
            rules.append("MAP %s %s" % (host, addr.split(",")[0]))
        elif o == "--connect-to":                # H1:P1:H2:P2
            h1, _, h2, p2 = opts[i + 1].rsplit(":", 3)
            rules.append("MAP %s %s" % (h1 or "*", h2 + (":" + p2 if p2 else "")))
    return ",".join(rules)


class Cdp:
    """Just enough of a client websocket for the DevTools protocol."""

    def __init__(self, ws_url):
        u = urlsplit(ws_url)
        self.sock = socket.create_connection((u.hostname, u.port), timeout=CDP_CALL_TIMEOUT_S)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(("GET %s HTTP/1.1\r\nHost: %s:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                           "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n"
                           % (u.path, u.hostname, u.port, key)).encode())
        head = b""
        while b"\r\n\r\n" not in head:
            head += self.sock.recv(4096)
        head, self.buf = head.split(b"\r\n\r\n", 1)
        if b" 101 " not in head.split(b"\r\n")[0]:
            die("devtools websocket: %r" % head[:100])
        self.next_id = 0

    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(1 << 16)
            if not chunk:
                die("devtools connection closed")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def _recv(self):
        data = b""
        while True:
            b0, b1 = self._read(2)
            n = b1 & 0x7F
            if n == 126:
                n, = struct.unpack(">H", self._read(2))
            elif n == 127:
                n, = struct.unpack(">Q", self._read(8))
            data += self._read(n)
            if b0 & 0x80:
                return json.loads(data)

    def call(self, method, **params):
        self.next_id += 1
        payload = json.dumps({"id": self.next_id, "method": method, "params": params}).encode()
        n, mask = len(payload), os.urandom(4)
        hdr = bytes([0x81]) + (bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n)
                               if n < 65536 else bytes([0x80 | 127]) + struct.pack(">Q", n))
        self.sock.sendall(hdr + mask + bytes(c ^ mask[i % 4] for i, c in enumerate(payload)))
        while True:
            msg = self._recv()
            if msg.get("id") == self.next_id:
                if "error" in msg:
                    die("%s: %s" % (method, msg["error"]))
                return msg["result"]

    def eval(self, expr):
        r = self.call("Runtime.evaluate", expression=expr, returnByValue=True)
        if "exceptionDetails" in r:
            die("page: %s" % json.dumps(r["exceptionDetails"])[:300])
        return r["result"].get("value")


def start_chrome(chrome, profile):
    argv = [chrome, "--headless=new", "--no-sandbox", "--no-first-run", "--remote-debugging-port=0",
            "--user-data-dir=" + profile, "--window-size=%d,%d" % (VIEW_W, VIEW_H),
            "--ignore-certificate-errors", "--disable-background-timer-throttling",
            "--disable-renderer-backgrounding", "--disable-backgrounding-occluded-windows",
            "--mute-audio", "about:blank"]
    rules = resolver_rules()
    if rules:
        argv.insert(1, "--host-resolver-rules=" + rules)
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=open(os.path.join(profile, "chrome.log"), "wb"))
    port_file = os.path.join(profile, "DevToolsActivePort")
    end = time.monotonic() + 30
    while time.monotonic() < end:
        if os.path.exists(port_file):
            with open(port_file) as f:
                port = int(f.readline())
            pages = json.load(urllib.request.urlopen("http://127.0.0.1:%d/json/list" % port, timeout=10))
            for p in pages:
                if p.get("type") == "page":
                    return proc, Cdp(p["webSocketDebuggerUrl"])
        if proc.poll() is not None:
            break
        time.sleep(0.1)
    with open(os.path.join(profile, "chrome.log"), errors="replace") as f:
        die("chrome did not start: %s" % f.read()[-500:])


def counters(cdp):
    return cdp.eval("JSON.stringify({...window.__boat, now: performance.now()})")


def wait_for_stream(cdp):
    end = time.monotonic() + CONNECT_TIMEOUT_S
    before = None
    while time.monotonic() < end:
        raw = cdp.eval("window.__boat ? JSON.stringify(window.__boat) : ''")
        if raw:
            b = json.loads(raw)
            if b["cw"] and before is not None and b["changed"] > before:
                return
            before = b["changed"] if b["cw"] else None
        time.sleep(1)
    die("no desktop frames in %d s at %s; page errors: %s"
        % (CONNECT_TIMEOUT_S, cdp.eval("location.href"), cdp.eval("JSON.stringify((window.__boat||{}).errors)")))


def measure(link, secs):
    chrome = find_chrome()
    if not chrome:
        die("no Chrome/Chromium found (set CHROME)")
    profile = tempfile.mkdtemp(prefix="boat-fps-")
    proc = None
    try:
        proc, cdp = start_chrome(chrome, profile)
        cdp.call("Page.enable")
        cdp.call("Runtime.enable")
        cdp.call("Page.addScriptToEvaluateOnNewDocument", source=PROBE)
        cdp.call("Emulation.setDeviceMetricsOverride", width=VIEW_W, height=VIEW_H,
                 deviceScaleFactor=1, mobile=False)  # the client sizes the desktop to its viewport
        cdp.call("Page.navigate", url=link)
        wait_for_stream(cdp)
        time.sleep(float(os.environ.get("WARMUP_S", "10")))
        a = json.loads(counters(cdp))
        time.sleep(secs)
        b = json.loads(counters(cdp))
        return a, b, cdp.eval("location.href")
    finally:
        if proc:
            proc.kill()
            proc.wait()
        shutil.rmtree(profile, ignore_errors=True)


def report(a, b):
    dt = (b["now"] - a["now"]) / 1000.0
    rate = lambda k: (b[k] - a[k]) / dt
    # the client's codec probe also decodes, so only frames during the window count
    stream = "%dx%d" % (b["w"], b["h"]) if b["decoded"] > a["decoded"] else "image:%dx%d" % (b["cw"], b["ch"])
    print("METRIC desktop_browser_fps=%.1f" % rate("changed"))
    print("METRIC desktop_browser_decoded_fps=%.1f" % rate("decoded"))
    print("METRIC desktop_browser_raf_fps=%.1f" % rate("raf"))
    print("METRIC desktop_browser_stream=%s" % stream)
    print("METRIC desktop_browser_stream_mbps=%.1f" % (rate("bytes") * 8 / 1e6))
    return rate("changed")


def main():
    if len(sys.argv) < 2:
        die("usage: desktop-browser-fps.py LINK [SECONDS]")
    a, b, where = measure(sys.argv[1], float(sys.argv[2]) if len(sys.argv) > 2 else 5.0)
    print("desktop-browser-fps: measured at %s" % urlsplit(where).netloc)
    fps = report(a, b)
    floor = float(os.environ.get("MIN_FPS", "0"))
    if fps < floor:
        die("%.1f fps is below the floor of %.1f" % (fps, floor))


if __name__ == "__main__":
    main()
