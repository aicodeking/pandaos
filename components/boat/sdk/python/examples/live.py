"""Live SDK round trip, run by ../../e2e-live.sh: create, exec, private port url, fork, stop, rm."""
import http.client
import os
import secrets
import sys
import tempfile
import time
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


def gateway_wait(addr, url, want, token=None, tries=20):
    """The route appears when boatd rewrites routes.json; poll until it answers as wanted."""
    status = 0
    for _ in range(tries):
        status = gateway_get(addr, url, token)
        if status in want:
            return status
        time.sleep(1)
    return status


# Behaviour only a real guest can show (the fake boatd in TestLiveScripts sets
# BOAT_LIVE_FAKE=1). Everything else runs against both.
REAL = not os.environ.get("BOAT_LIVE_FAKE")
# The stub keeps prompts free and offline. The real agents are covered by
# e2e-agent-from-env.sh when CI has keys.
STUB_AGENT = b"""#!/bin/bash
case "$*" in *sleep-forever*) exec sleep 300 ;; esac
echo '{"type":"result","text":"stub"}'
"""

boat = Boat()
prefix = os.environ.get("BOAT_LIVE_PREFIX", "sdk-live") + "-py-" + secrets.token_hex(3)
made = []
envs_made = []
try:
    vm = boat.create_vm(name=prefix, secrets=[])
    made.append(vm.id)
    check(vm.id and vm.name == prefix, "create " + vm.id)

    res = boat.run(vm.id, ["echo", "boat-live"])
    check(res.exit_code == 0 and res.stdout.strip() == b"boat-live", "exec echo")

    boat.run(vm.id, ["bash", "-c", "setsid nohup python3 -m http.server 8080 >/dev/null 2>&1 </dev/null & sleep 1"])
    port = boat.add_port(vm.id, 8080)
    check(port.get("auth") == "private", "port 8080 is private by default")
    pu = boat.port_url(vm.id, 8080, ttl_s=600)
    check("boat_sig=" in pu["url"] and pu.get("token"), "private port url carries a token")
    gw = os.environ.get("BOAT_GATEWAY_ADDR")
    if gw:
        st = gateway_wait(gw, pu["url"], (401, 403))
        check(st in (401, 403), "gateway refuses the route without a token (%d)" % st)
        st = gateway_wait(gw, pu["url"], (200,), pu["token"])
        check(st == 200, "gateway serves the route with the token (%d)" % st)

    child = boat.fork(vm.id, name=prefix + "-fork")
    made.append(child.id)
    check(child.id != vm.id, "fork " + child.id)

    # ---- detached commands
    started = boat.start_command(vm.id, ["bash", "-c", "echo detached-out; exit 7"])
    out, code = b"", None
    for ev in boat.follow_command(vm.id, started.id):
        out += ev.data
        code = ev.exit_code if ev.exit_code is not None else code
    check(code == 7 or not REAL, "follow returns the exit code (%s)" % code)
    check(b"detached-out" in out or not REAL, "follow returns the output")
    check(any(c.id == started.id for c in boat.list_commands(vm.id)), "commands list")
    check(any(c["id"] == started.id for c in boat.command_history(vm.id)), "command history")
    long_cmd = boat.start_command(vm.id, ["sleep", "300"])
    boat.signal_command(vm.id, long_cmd.id, "TERM")
    for _ in range(30):
        st = boat.command_status(vm.id, long_cmd.id)
        if st.state != "running":
            break
        time.sleep(0.5)
    check(st.state == "signaled", "signal stops a command (%s)" % st.state)

    # ---- files
    boat.make_folder(vm.id, "/home/dev/live")
    boat.write_file(vm.id, "/home/dev/live/a.txt", b"needle")
    check(any(e["name"] == "a.txt" for e in boat.list_files(vm.id, "/home/dev/live")), "file list")
    boat.move_file(vm.id, "/home/dev/live/a.txt", "/home/dev/live/b.txt")
    check(boat.read_file(vm.id, "/home/dev/live/b.txt") == b"needle", "file move")
    check(any(m["path"].endswith("b.txt") for m in boat.search_files(vm.id, "/home/dev/live", "b.txt")), "file search")
    check(boat.file_info(vm.id, "/home/dev/live/b.txt")["size"] == (6 if REAL else 5), "file info")
    boat.set_file_mode(vm.id, "/home/dev/live/b.txt", "600")
    boat.delete_file(vm.id, "/home/dev/live/b.txt")
    check(not any(e["name"] == "b.txt" for e in boat.list_files(vm.id, "/home/dev/live")), "file delete")

    # ---- async prompts, with a stub agent on dev's PATH
    boat.write_file(vm.id, "/home/dev/.local/bin/claude", STUB_AGENT)
    boat.set_file_mode(vm.id, "/home/dev/.local/bin/claude", "755")
    slow = boat.start_prompt(vm.id, "sleep-forever on it", new=True)
    check(slow.state == "running" and slow.conversation_id, "async prompt returns a turn")
    next_turn = boat.steer_prompt(vm.id, slow.id, "change of plan")
    check(next_turn.conversation_id == slow.conversation_id and next_turn.id != slow.id, "steer resumes the conversation")
    check(boat.prompt_status(vm.id, slow.id).state == "steered", "the steered turn is marked")
    code = None
    for ev in boat.follow_prompt(vm.id, next_turn.id):
        code = ev.exit_code if ev.exit_code is not None else code
    check(code == 0 or not REAL, "the new turn finishes (%s)" % code)
    argv = [c["cmd"] for c in boat.command_history(vm.id) if c["id"] == next_turn.cmd_id][0]
    check("--resume" in argv, "the new turn resumes the session")
    victim = boat.start_prompt(vm.id, "sleep-forever again", new=True)
    check(boat.interrupt_prompt(vm.id, victim.id).state == "interrupted", "interrupt")
    check(len(boat.conversations(vm.id)) == 2, "conversations")

    # ---- resize: a disk grow works on a running VM
    disk = vm.raw.get("disk_gib") or 8
    check((boat.resize(vm.id, disk_gib=disk + 1).raw.get("disk_gib") or 0) in (disk + 1, 0), "resize grows the disk")

    # ---- snapshots: download, import, share
    snap = boat.create_snapshot(vm.id, name=prefix + "-s")
    path = os.path.join(os.environ.get("E2E_TMP") or tempfile.gettempdir(), prefix + ".tar.zst")
    check(boat.download_snapshot(vm.id, snap["id"], path) > 0, "snapshot download")
    with open(path, "rb") as f:
        imported = boat.import_snapshot(f, name=prefix + "-imp")
    os.remove(path)
    check(imported["id"] != snap["id"], "snapshot import")
    check(any(x["id"] == imported["id"] for x in boat.team_snapshots()), "team snapshots lists the import")
    share = boat.share_snapshot(vm.id, snap["id"], ttl_s=600)
    check(share["code"].startswith("bsh_"), "share code")
    taken = boat.take_snapshot_share(code=share["code"], name=prefix + "-copy")
    check(taken["id"] not in (snap["id"], imported["id"]), "share taken as a copy")
    boat.revoke_snapshot_share(vm.id, snap["id"], share["id"])
    boat.delete_team_snapshot(imported["id"])
    boat.delete_team_snapshot(taken["id"])

    # ---- environments: setup on create, then a build for fast start
    env_name = prefix + "-env"
    envs_made.append(env_name)
    boat.create_environment(env_name, setup_script="echo built > /home/dev/env-ok", secrets=[], env={"LIVE_VAR": "x"})
    ev_vm = boat.create_vm(name=prefix + "-e1", environment=env_name)
    made.append(ev_vm.id)
    check(ev_vm.raw.get("environment") == env_name and ev_vm.raw.get("setup_cmd_id"), "new VM runs the setup")
    code = None
    for ev in boat.follow_command(ev_vm.id, ev_vm.raw["setup_cmd_id"]):
        code = ev.exit_code if ev.exit_code is not None else code
    check(code == 0 or not REAL, "setup succeeded (%s)" % code)
    check(boat.run(ev_vm.id, ["cat", "/home/dev/env-ok"]).stdout.strip() == b"built", "setup's result is on the VM")
    boat.delete(ev_vm.id)
    made.remove(ev_vm.id)
    boat.build_environment(env_name)
    for _ in range(300):
        build = boat.get_environment(env_name)["build"]
        if build["state"] != "building":
            break
        time.sleep(1)
    check(build["state"] == "ready", "environment build (%s %s)" % (build["state"], build.get("error", "")))
    fast = boat.create_vm(name=prefix + "-e2", environment=env_name)
    made.append(fast.id)
    check(fast.raw.get("from_build") is True and not fast.raw.get("setup_cmd_id"), "built env starts with no setup")
    check(boat.run(fast.id, ["cat", "/home/dev/env-ok"]).stdout.strip() == b"built", "built env has the setup's result")
    boat.delete(fast.id)
    made.remove(fast.id)
    boat.delete_environment(env_name)
    envs_made.remove(env_name)

    # ---- key and webhook rotation
    key = boat.create_key(name=prefix + "-k", scopes=["vms:read"])
    rotated = boat.rotate_key(key["id"], overlap_s=600)
    check(rotated["token"] != key["token"], "key rotation")
    check(Boat(boat.url, key["token"]).me()["key_id"] == key["id"], "old key works during the overlap")
    check(Boat(boat.url, rotated["token"]).me()["key_id"] == key["id"], "new key works")
    boat.delete_key(key["id"])
    hook = boat.create_webhook("https://example.invalid/h", events=["live.none"])
    check(boat.rotate_webhook(hook["id"])["secret"] != hook["secret"], "webhook rotation")
    boat.delete_webhook(hook["id"])

    # ---- AI proxy: the VM holds a token, never the key
    if boat.ai_proxy()["available"]:
        fake_key = "sk-live-test-" + secrets.token_hex(8)
        boat.set_ai_credential("anthropic", fake_key)
        try:
            ai_vm = boat.create_vm(name=prefix + "-ai", secrets=[])
            made.append(ai_vm.id)
            tok = boat.run(ai_vm.id, ["bash", "-lc", "printenv ANTHROPIC_API_KEY"]).stdout.strip()
            check(tok.startswith(b"boatvm_"), "VM env holds a proxy token")
            on_disk = boat.run(ai_vm.id, ["sudo", "cat", "/etc/boat/env"]).stdout
            check(fake_key.encode() not in on_disk, "the real key is not on the VM")
            boat.delete(ai_vm.id)
            made.remove(ai_vm.id)
        finally:
            boat.delete_ai_credential("anthropic")
    else:
        print("SKIP - AI proxy checks: this server has no -ai-proxy-url")
    check("totals" in boat.ai_usage(), "AI usage")

    # ---- telemetry config round trip
    check(boat.set_telemetry("https://otlp.example.invalid", {"X-Key": "v"})["configured"], "telemetry set")
    check(boat.telemetry()["header_names"] == ["X-Key"], "telemetry hides header values")
    boat.delete_telemetry()

    check(boat.stop(vm.id).state == "stopped", "stop")
    check(any(c["id"] == started.id for c in boat.command_history(vm.id)), "history works on a stopped VM")
finally:
    for vid in reversed(made):
        try:
            boat.delete(vid)
            print("deleted", vid)
        except BoatError as e:
            print("cleanup", vid, e, file=sys.stderr)
    for name in envs_made:
        try:
            boat.delete_environment(name)
        except BoatError as e:
            print("cleanup env", name, e, file=sys.stderr)
for vid in made:
    try:
        boat.get_vm(vid)
        raise SystemExit("python live: FAIL: %s still exists after rm" % vid)
    except BoatError as e:
        if e.status != 404:
            raise
print("ok - rm")
