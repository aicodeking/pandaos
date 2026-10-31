#!/usr/bin/env python3
"""Serve WWW_DIR on 127.0.0.1:8000; log each POST /submit as one line to LOG.

The line is the decoded fields sorted by name, `k=v|k=v`, which is what the
task checkers grep for. usage: formserver.py WWW_DIR LOG
"""
import functools
import http.server
import sys
import urllib.parse

www, logfile = sys.argv[1], sys.argv[2]


class Handler(http.server.SimpleHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/submit":
            self.send_error(404)
            return
        n = int(self.headers.get("content-length") or 0)
        fields = urllib.parse.parse_qsl(self.rfile.read(n).decode(), keep_blank_values=True)
        line = "|".join(f"{k}={v.strip()}" for k, v in sorted(fields))
        with open(logfile, "a") as f:
            f.write(line + "\n")
        body = b"<!doctype html><title>Thanks</title><h1>Thank you, your form was submitted.</h1>"
        self.send_response(200)
        self.send_header("content-type", "text/html")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


http.server.ThreadingHTTPServer(("127.0.0.1", 8000),
                                functools.partial(Handler, directory=www)).serve_forever()
