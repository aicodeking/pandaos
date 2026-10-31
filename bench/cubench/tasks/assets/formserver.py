#!/usr/bin/env python3
"""Serve WWW_DIR on 127.0.0.1:8000; log each POST /submit as one line to LOG.

The line is the decoded fields sorted by name, then `ua=firefox` or
`ua=other`: `k=v|k=v|ua=firefox`, which is what the task checkers grep for.
`ua=firefox` needs a Firefox User-Agent AND a client socket owned by a
process whose executable is Firefox (looked up through /proc/net/tcp and
/proc/*/fd while the connection is open), so curl or python3 with a forged
User-Agent from a terminal still logs `ua=other`.

/token.html shows a random token made at server start and kept only in this
process's memory (never on disk, no-store); the log gets its sha256 as
`tokensha=...`, so a checker can confirm a pasted copy without the token
existing anywhere the agent could read it from.

usage: formserver.py WWW_DIR LOG
"""
import functools
import os
import hashlib
import html
import http.server
import secrets
import sys
import urllib.parse

www, logfile = sys.argv[1], sys.argv[2]
TOKEN = "cb-" + secrets.token_hex(24)


def log(line):
    with open(logfile, "a") as f:
        f.write(line + "\n")


def client_exe(port):
    """Executable of the local process that owns the TCP connection from `port` to us."""
    inode = None
    for table in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            rows = open(table).read().splitlines()[1:]
        except OSError:
            continue
        for row in rows:
            f = row.split()
            if int(f[1].rsplit(":", 1)[1], 16) == port and int(f[2].rsplit(":", 1)[1], 16) == 8000:
                inode = f[9]
    if not inode:
        return ""
    target = f"socket:[{inode}]"
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            for fd in os.listdir(f"/proc/{pid}/fd"):
                if os.readlink(f"/proc/{pid}/fd/{fd}") == target:
                    return os.readlink(f"/proc/{pid}/exe")
        except OSError:
            continue
    return ""


class Handler(http.server.SimpleHTTPRequestHandler):
    def end_headers(self):
        self.send_header("cache-control", "no-store")
        super().end_headers()

    def _send(self, body, ctype="text/html; charset=utf-8"):
        self.send_response(200)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.split("?")[0] == "/token.html":
            with open(f"{www}/token.html", encoding="utf-8") as f:
                self._send(f.read().replace("@TOKEN@", html.escape(TOKEN)).encode())
            return
        super().do_GET()

    def do_POST(self):
        if self.path != "/submit":
            self.send_error(404)
            return
        n = int(self.headers.get("content-length") or 0)
        fields = urllib.parse.parse_qsl(self.rfile.read(n).decode(), keep_blank_values=True)
        exe = os.path.basename(client_exe(self.client_address[1]))
        ua = "firefox" if "Firefox/" in self.headers.get("user-agent", "") and exe.startswith("firefox") else "other"
        log("|".join(f"{k}={v.strip()}" for k, v in sorted(fields)) + f"|ua={ua}")
        self._send(b"<!doctype html><title>Thanks</title><h1>Thank you, your form was submitted.</h1>")


log("tokensha=" + hashlib.sha256(TOKEN.encode()).hexdigest())
http.server.ThreadingHTTPServer(("127.0.0.1", 8000),
                                functools.partial(Handler, directory=www)).serve_forever()
