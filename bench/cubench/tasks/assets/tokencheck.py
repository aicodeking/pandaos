#!/usr/bin/env python3
"""Clipboard-task checker: FILE holds the server's in-memory token, and the
browser page saw a copy (ev=copy from Firefox), so the token can only have
come through the clipboard. usage: tokencheck.py FORM_LOG FILE"""
import hashlib
import sys

try:
    log = open(sys.argv[1]).read().splitlines()
    got = open(sys.argv[2]).read().strip()
except OSError as e:
    sys.exit(f"missing: {e}")
want = [line.split("=", 1)[1] for line in log if line.startswith("tokensha=")]
if not want or hashlib.sha256(got.encode()).hexdigest() != want[-1]:
    sys.exit(f"{sys.argv[2]} does not hold the token shown in the browser ({len(got)} chars)")
if "ev=copy|ua=firefox" not in log:
    sys.exit("the browser never saw a copy")
print("token pasted from the browser")
