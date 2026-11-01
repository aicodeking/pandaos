"""Live SDK round trip, run by ../../e2e-live.sh: create, exec, private port url, fork, stop, rm."""
import http.client
import os
import secrets
import sys
import urllib.parse

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from boat import Boat, BoatError  # noqa: E402


def check(cond, what):
    if not cond:
        raise SystemExit("python live: FAIL: " + what)
    print("ok -", what)


def gateway_get(addr, url, token=None):
    u = urllib.parse.urlsplit(url)
    conn = http.client.HTTPConnection(addr, timeout=20)
    headers = {"Host": u.netloc}
    if token:
        headers["X-Boat-Route-Token"] = token
    conn.request("GET", u.path or "/", headers=headers)  # no ?boat_sig: test the header path
    return conn.getresponse().status


boat = Boat()
prefix = os.environ.get("BOAT_LIVE_PREFIX", "sdk-live") + "-py-" + secrets.token_hex(3)
made = []
try:
    vm = boat.create_vm(name=prefix, secrets=[])
    made.append(vm.id)
    check(vm.id and vm.name == prefix, "create " + vm.id)

    res = boat.run(vm.id, ["echo", "boat-live"])
    check(res.exit_code == 0 and res.stdout.strip() == b"boat-live", "exec echo")

    boat.run(vm.id, ["bash", "-c", "nohup python3 -m http.server 8080 >/dev/null 2>&1 &"])
    port = boat.add_port(vm.id, 8080)
    check(port.get("auth") == "private", "port 8080 is private by default")
    pu = boat.port_url(vm.id, 8080, ttl_s=600)
    check("boat_sig=" in pu["url"] and pu.get("token"), "private port url carries a token")
    gw = os.environ.get("BOAT_GATEWAY_ADDR")
    if gw:
        check(gateway_get(gw, pu["url"]) in (401, 403), "gateway refuses the route without a token")
        check(gateway_get(gw, pu["url"], pu["token"]) == 200, "gateway serves the route with the token")

    child = boat.fork(vm.id, name=prefix + "-fork")
    made.append(child.id)
    check(child.id != vm.id, "fork " + child.id)

    check(boat.stop(vm.id).state == "stopped", "stop")
finally:
    for vid in reversed(made):
        try:
            boat.delete(vid)
            print("deleted", vid)
        except BoatError as e:
            print("cleanup", vid, e, file=sys.stderr)
for vid in made:
    try:
        boat.get_vm(vid)
        raise SystemExit("python live: FAIL: %s still exists after rm" % vid)
    except BoatError as e:
        if e.status != 404:
            raise
print("ok - rm")
