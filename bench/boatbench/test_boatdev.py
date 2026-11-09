#!/usr/bin/env python3
"""drivers.BoatDev against boat.dev's published OpenAPI, offline.

Every request the driver makes is matched to an operation in boatdev-openapi.json
(cut from https://docs.boat.dev/openapi/boat-v1.yaml by boatdev_openapi_extract.py)
and checked: path, method, required headers, body fields known and required ones
present, values valid. The responses are recorded fixtures that are themselves
validated against the spec's response schemas, so the driver is shown to parse
what the spec says boat.dev sends. Nothing here has run against the live API.

python3 bench/boatbench/test_boatdev.py
"""
import base64
import json
import os
import re
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import drivers  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(HERE, "boatdev-openapi.json")) as f:
    SPEC = json.load(f)
BASE = SPEC["servers"][0]["url"]
SID, FORK_ID, NEW_ID = "bx_23456789", "bx_abcdef23", "bx_mnpqrstu"


def _type_ok(v, t):
    py = {"string": str, "integer": int, "number": (int, float), "boolean": bool, "object": dict,
          "array": list, "null": type(None)}[t]
    if t in ("integer", "number") and isinstance(v, bool):
        return False
    return isinstance(v, py)


def validate(v, s, where="$"):
    """Errors of value v against JSON schema s (the subset the spec uses)."""
    errs = []
    if "allOf" in s:
        for sub in s["allOf"]:
            errs += validate(v, sub, where)
    for key in ("oneOf", "anyOf"):
        if key in s and all(validate(v, sub, where) for sub in s[key]):
            errs.append(f"{where}: matches no {key} branch")
    t = s.get("type")
    if t is not None:
        ts = t if isinstance(t, list) else [t]
        if not any(_type_ok(v, x) for x in ts):
            return errs + [f"{where}: {v!r} is not {t}"]
    if "const" in s and v != s["const"]:
        errs.append(f"{where}: {v!r} != const {s['const']!r}")
    if "enum" in s and v not in s["enum"]:
        errs.append(f"{where}: {v!r} not in {s['enum']}")
    if isinstance(v, str):
        if "pattern" in s and not re.search(s["pattern"], v):
            errs.append(f"{where}: {v!r} !~ {s['pattern']}")
        if "maxLength" in s and len(v) > s["maxLength"]:
            errs.append(f"{where}: longer than {s['maxLength']}")
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        if "minimum" in s and v < s["minimum"] or "maximum" in s and v > s["maximum"]:
            errs.append(f"{where}: {v} outside {s.get('minimum')}..{s.get('maximum')}")
    if isinstance(v, dict):
        for r in s.get("required", []):
            if r not in v:
                errs.append(f"{where}: missing required {r}")
        props = s.get("properties", {})
        for k, x in v.items():
            if k in props:
                errs += validate(x, props[k], f"{where}.{k}")
            elif isinstance(s.get("additionalProperties"), dict):
                errs += validate(x, s["additionalProperties"], f"{where}.{k}")
    if isinstance(v, list) and "items" in s:
        for i, x in enumerate(v):
            errs += validate(x, s["items"], f"{where}[{i}]")
    return errs


def known_fields(s):
    """Property names a request body schema declares (through allOf/oneOf)."""
    out = set(s.get("properties", {}))
    for key in ("allOf", "oneOf", "anyOf"):
        for sub in s.get(key, []):
            out |= known_fields(sub)
    return out


def find_op(method, url):
    assert url.startswith(BASE), url
    path = url[len(BASE):].split("?")[0]
    for name, op in SPEC["operations"].items():
        m, tmpl = name.split(" ", 1)
        if m == method and re.fullmatch(re.sub(r"\{[^}]+\}", "[^/]+", tmpl), path):
            return name, op, tmpl, path
    raise AssertionError(f"{method} {path}: not an operation in boat.dev's OpenAPI")


def body_schema(op):
    return op["requestBody"]["content"]["application/json"]["schema"]


def response_schema(op, status):
    return op["responses"][str(status)]["content"]["application/json"]["schema"]


def sandbox(sid, state, **kw):
    return dict({"id": sid, "name": "bb", "state": state, "desktopAvailable": False, "snapshotAvailable": False}, **kw)


def err(status, code):
    return {"ok": False, "type": "sandbox.error", "status": status, "code": code, "message": code,
            "requestId": "req_1", "error": {"code": code, "message": code, "status": status}}


def stop_op(status):
    return {"id": "stop_3f9a1c2e7b4d5a60", "status": status, "requestedAt": "2026-11-09T00:00:00Z",
            "lastAttemptAt": None, "error": None, "endedAt": None}


def snap(status):
    return {"name": "bb-snap", "status": status, "sourceSandboxId": SID, "createdAt": "2026-11-09T00:00:00Z"}


def deleting(kind, target):
    return {"ok": True, "type": kind, "operation": {
        "id": "bdop_" + "0" * 32, "kind": "sandbox", "targetId": target, "reason": "explicit", "status": "pending",
        "attemptCount": 0, "requestedAt": "2026-11-09T00:00:00Z", "completedAt": None}}


def command(rc, out=""):
    return {"ok": True, "type": "command.finished", "success": rc == 0, "exitCode": rc, "stdout": out,
            "stderr": "", "timedOut": False}


class Server:
    """Answers each request from a queue of (operation, status, body) fixtures,
    after checking the request against the spec. Records what was sent."""

    def __init__(self, fixtures):
        self.fixtures, self.sent = list(fixtures), []

    def __call__(self, method, url, key, body=None, raw=None, timeout=660, ctype="application/json", headers=None):
        name, op, tmpl, path = find_op(method, url)
        self.sent.append((name, body, headers or {}))
        assert key == "test-key" and raw is None, "JSON bodies only"
        for p in op.get("parameters", []):
            if p.get("in") == "header" and p.get("required"):
                assert p["name"] in (headers or {}), f"{name}: header {p['name']} is required"
                if p["name"] == "X-Ascii-Confirm-Delete":
                    assert headers[p["name"]] == path.rsplit("/", 1)[1], f"{name}: confirm must equal the id"
        if body is not None:
            s = body_schema(op)
            unknown = set(body) - known_fields(s)
            assert not unknown, f"{name}: fields not in the spec: {unknown}"
            assert not validate(body, s), f"{name}: {validate(body, s)}"
        elif op.get("requestBody", {}).get("required"):
            raise AssertionError(f"{name}: body required")
        want, status, resp = self.fixtures.pop(0)
        assert want == name, f"expected a call to {want}, driver called {name}"
        assert str(status) in op["responses"], f"{name}: {status} not a documented response"
        problems = validate(resp, response_schema(op, status))
        assert not problems, f"fixture for {name} {status} breaks the spec: {problems}"
        if status >= 400:
            raise drivers.ApiError(status, json.dumps(resp), f"{method} {url}")
        return json.dumps(resp)


class BoatDevAgainstSpec(unittest.TestCase):
    def run_with(self, fixtures, fn):
        srv = Server(fixtures)
        old_http, old_poll = drivers._http, drivers.POLL_S
        drivers._http, drivers.POLL_S = srv, 0
        try:
            out = fn(drivers.BoatDev(key="test-key"))
        finally:
            drivers._http, drivers.POLL_S = old_http, old_poll
        self.assertEqual(srv.fixtures, [], "fixtures left over: the driver made fewer calls")
        return out, srv.sent

    def test_spec_subset_is_the_published_one(self):
        self.assertEqual(SPEC["source"], "https://docs.boat.dev/openapi/boat-v1.yaml")
        self.assertEqual(BASE, "https://boat.dev/api/v1")
        self.assertEqual(drivers.BoatDev(key="k").url, BASE)
        self.assertRegex(SPEC["sha256"], "^[0-9a-f]{64}$")

    def test_create_exec_ready_and_exec(self):
        def flow(d):
            sid = d.create("bb-abc-0-a")
            starting = d.run_true(sid)
            ready = d.run_true(sid)
            return sid, starting, ready, d.exec(sid, "echo 'hi'", 900)
        created = {"ok": True, "type": "sandbox.created", "status": "provisioning", "ttlSeconds": 7200,
                   "sandbox": sandbox(SID, "provisioning")}
        (sid, starting, ready, ex), sent = self.run_with([
            ("POST /sandboxes", 202, created),
            ("POST /sandboxes/{sandboxId}/commands", 409, err(409, "boat_starting")),
            ("POST /sandboxes/{sandboxId}/commands", 200, command(0)),
            ("POST /sandboxes/{sandboxId}/commands", 200, command(3, "hi\n")),
        ], flow)
        self.assertEqual((sid, starting, ready, ex), (SID, None, 0, (3, "hi\n", "")))
        self.assertEqual(sent[0][1]["type"], "default")
        self.assertEqual(sent[3][1]["timeoutSeconds"], 600)  # the spec caps commands at 600 s
        self.assertEqual(sent[3][1]["command"], "bash -lc 'echo '\"'\"'hi'\"'\"''")

    def test_put_file_is_base64_json(self):
        written = {"ok": True, "type": "file.written", "success": True, "path": "/tmp/x", "encoding": "base64", "size": 3}
        _, sent = self.run_with([("PUT /sandboxes/{sandboxId}/files", 200, written)],
                                lambda d: d.put_file(SID, "/tmp/x", b"\x00ab"))
        self.assertEqual(base64.b64decode(sent[0][1]["content"]), b"\x00ab")

    def test_fork_reads_the_new_id_even_without_a_sandbox(self):
        forking = {"ok": True, "type": "sandbox.forking", "id": FORK_ID, "status": "forking", "sandbox": None}
        fid, sent = self.run_with([("POST /sandboxes/{sandboxId}/fork", 202, forking)], lambda d: d.fork(SID, "bb-f"))
        self.assertEqual(fid, FORK_ID)
        self.assertNotIn("name", sent[0][1])

    def test_stop_polls_until_archived_then_resume(self):
        stopping = {"ok": True, "type": "sandbox.stopping", "id": SID, "status": "archiving",
                    "sandbox": sandbox(SID, "archiving"), "operation": stop_op("pending")}
        info = lambda st, op: {"ok": True, "type": "sandbox.info", "sandbox": sandbox(SID, st, stop=op)}  # noqa: E731
        resuming = {"ok": True, "type": "sandbox.resuming", "id": SID, "status": "resuming",
                    "sandbox": sandbox(SID, "provisioning")}
        self.run_with([
            ("POST /sandboxes/{sandboxId}/stop", 202, stopping),
            ("GET /sandboxes/{sandboxId}", 200, info("archiving", stop_op("pending"))),
            ("GET /sandboxes/{sandboxId}", 200, info("archived", stop_op("completed"))),
            ("POST /sandboxes/{sandboxId}/resume", 202, resuming),
        ], lambda d: (d.stop(SID), d.resume(SID)))

    def test_saved_snapshot_round_trip_and_cleanup(self):
        saving = {"ok": True, "type": "snapshot.saved.saving", "status": "saving", "snapshot": snap("saving")}
        info = lambda st: {"ok": True, "type": "snapshot.saved.info", "snapshot": snap(st)}  # noqa: E731
        created = {"ok": True, "type": "sandbox.created", "status": "provisioning", "ttlSeconds": 7200,
                   "sandbox": sandbox(NEW_ID, "provisioning")}

        def flow(d):
            ref = d.snapshot(SID, "bb-snap")
            new = d.from_snapshot(ref, "bb-s")
            d.delete(new)
            d.delete_snapshot(ref)
            return ref, new
        (ref, new), sent = self.run_with([
            ("POST /saved-snapshots", 202, saving),
            ("GET /saved-snapshots/{name}", 200, info("saving")),
            ("GET /saved-snapshots/{name}", 200, info("ready")),
            ("POST /sandboxes", 202, created),
            ("DELETE /sandboxes/{sandboxId}", 202, deleting("sandbox.deleting", NEW_ID)),
            ("DELETE /saved-snapshots/{name}", 200, {"ok": True, "type": "snapshot.saved.deleted", "name": "bb-snap", "status": "deleted"}),
        ], flow)
        self.assertEqual((ref, new), ("bb-snap", NEW_ID))
        self.assertEqual(sent[3][1]["from"], "bb-snap")
        self.assertEqual(sent[4][2], {"X-Ascii-Confirm-Delete": NEW_ID})

    def test_failed_snapshot_raises(self):
        saving = {"ok": True, "type": "snapshot.saved.saving", "status": "saving", "snapshot": snap("saving")}
        failed = {"ok": True, "type": "snapshot.saved.info", "snapshot": dict(snap("failed"), error="disk")}
        with self.assertRaises(RuntimeError):
            self.run_with([("POST /saved-snapshots", 202, saving), ("GET /saved-snapshots/{name}", 200, failed)],
                          lambda d: d.snapshot(SID, "bb-snap"))

    def test_bench_names_fit_the_spec(self):
        create = body_schema(SPEC["operations"]["POST /sandboxes"])
        snap_s = body_schema(SPEC["operations"]["POST /saved-snapshots"])
        prefix = "bb-abc123-4"  # boatbench: bb-<run_id hex 6>-<repeat>
        self.assertFalse(validate({"name": prefix + "-c9", "type": "default"}, create))
        self.assertFalse(validate({"sandboxId": SID, "name": prefix + "-snap"}, snap_s))


if __name__ == "__main__":
    unittest.main()
