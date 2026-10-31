#!/usr/bin/env python3
"""Validate-mode cheats: reach a browser task's end state without the browser.

  cheatpost.py k=v [k=v...]     POST the fields to the form server with a Firefox User-Agent
  cheatpost.py --token FILE     scrape the token page and write the token to FILE

Every checker must still fail: the server credits only POSTs whose client
process is the Firefox binary, not just a Firefox User-Agent.
"""
import re
import sys
import urllib.parse
import urllib.request

URL = "http://127.0.0.1:8000"
UA = {"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:159.0) Gecko/20100101 Firefox/159.0"}

if sys.argv[1:2] == ["--token"]:
    page = urllib.request.urlopen(urllib.request.Request(URL + "/token.html", headers=UA)).read().decode()
    with open(sys.argv[2], "w") as f:
        f.write(re.search(r"<textarea[^>]*>([^<]*)</textarea>", page).group(1).strip() + "\n")
else:
    body = urllib.parse.urlencode([tuple(a.split("=", 1)) for a in sys.argv[1:]]).encode()
    urllib.request.urlopen(urllib.request.Request(URL + "/submit", data=body, headers=UA)).read()
