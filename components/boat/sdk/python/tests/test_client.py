import base64
import datetime
import errno
import hashlib
import hmac
import http.client
import io
import json
import os
import shutil
import socket
import sys
import tarfile
import tempfile
import threading
import time
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from boat import Boat, BoatError, BoatTimeoutError, VM as VMRecord, VMHandle, verify_webhook  # noqa: E402
from boat.client import ExecEvent, _iter_lines  # noqa: E402

TOKEN = "boat_test"
VM = {"id": "vm1", "name": "dev", "size": "small", "state": "running", "image": "ubuntu-24.04", "team": "t1"}
PATCH_KEYS = frozenset({"name", "ttl_s", "extend_s", "labels", "idle_stop_s"})


class _DaemonHTTPServer(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        exc = sys.exc_info()[1]
        if isinstance(exc, (ConnectionResetError, BrokenPipeError)):
            return
        super().handle_error(request, client_address)


class _FakeTransportResp:
    def __init__(self, raw=b"", status=200):
        self.status = status
        self.reason = "OK"
        self.headers = {}
        self._buf = io.BytesIO(raw)
        self.fp = None
        self._closed = False

    def read(self, amt=None):
        return self._buf.read() if amt is None else self._buf.read(amt)

    def readline(self, limit=-1):
        return self._buf.readline() if limit < 0 else self._buf.readline(limit)

    def isclosed(self):
        return self._closed

    def close(self):
        self._closed = True

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()

    def __iter__(self):
        return self

    def __next__(self):
        line = self.readline()
        if not line:
            raise StopIteration
        return line


class Mock(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    log = []
    peers = []
    files = {}
    vm_setup = None
    exec_hang = None
    push_hang = None
    idem = []

    def log_message(self, *a):
        pass

    def _read_body(self):
        if "chunked" in (self.headers.get("Transfer-Encoding") or "").lower():
            chunks = []
            while True:
                line = self.rfile.readline()
                if not line:
                    break
                size = int(line.split(b";", 1)[0].strip(), 16)
                if size == 0:
                    while self.rfile.readline() not in (b"", b"\r\n", b"\n"):
                        pass
                    break
                chunks.append(self.rfile.read(size))
                self.rfile.read(2)
            return b"".join(chunks)
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n > 0 else b""

    def _send(self, code, obj=None, raw=None, ctype="application/json"):
        body = raw if raw is not None else json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _handle(self):
        body = self._read_body()
        Mock.log.append((self.command, self.path, body))
        Mock.peers.append(self.client_address)
        Mock.idem.append((self.command, self.path, self.headers.get("Idempotency-Key")))
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            return self._send(401, {"error": {"code": "unauthorized", "message": "bad key", "request_id": "req_1"}})
        p, m = self.path, self.command
        if p.startswith("/v1/vms/silent/"):
            raw = b'{"stream":"stdout","data_b64":"bGF0ZQ=="}\n{"exit":0}\n'
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            time.sleep(0.6)
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                pass  # The timeout regression closes the connection before the event.
            return
        if (m, p) == ("POST", "/v1/vms") and json.loads(body).get("count", 1) > 1:
            req = json.loads(body)
            vms = [dict(VM, id="vm%d" % i, name="%s-%d" % (req["name"], i)) for i in range(1, req["count"] + 1)]
            if Mock.vm_setup is not None:
                vms[-1].update(Mock.vm_setup["response"])
            return self._send(201, {"vms": vms})
        if m == "POST" and p in ("/v1/vms/vmenv/start", "/v1/vms/vmenv/fork", "/v1/vms/vmenv/clone"):
            return self._send(200, dict(VM, id="vmenv2", env_error="push failed", agent_ready=False))
        if (m, p) == ("POST", "/v1/vms/vm502/start"):
            return self._send(502, {"error": {"code": "env_update_failed", "message": "env push failed"}})
        if (m, p) == ("POST", "/v1/vms/vm502d/start"):
            return self._send(502, {"error": {"code": "env_start_failed", "message": "batch failed closed"},
                                    "vm": dict(VM, id="vm502d", state="stopped"),
                                    "vms": [dict(VM, id="vm502d", state="stopped"), dict(VM, id="vm502e")]})
        if (m, p) == ("POST", "/v1/vms/vm502c/start"):
            return self._send(502, {"error": {"code": "env_start_failed", "message": "no_env start failed"}, "vm": dict(VM, id="vm502c", state="stopped")})
        if (m, p) == ("POST", "/v1/vms/vm502b/start"):
            return self._send(502, {"error": {"code": "env_update_failed", "message": "env push failed"}, "vm": dict(VM, id="vm502b")})
        if (m, p) == ("POST", "/v1/vms"):
            vm = dict(VM, **json.loads(body))
            if Mock.vm_setup is not None:
                vm.update(Mock.vm_setup["response"])
            return self._send(201, vm)
        if m == "GET" and p.startswith("/v1/vms/vm1/commands/setup1/logs?"):
            setup = Mock.vm_setup
            if "sequences" in setup:
                seq = setup["sequences"]
                events = seq.pop(0) if len(seq) > 1 else seq[0]
                return self._send(200, raw=b"".join(json.dumps(e).encode() + b"\n" for e in events),
                                  ctype="application/x-ndjson")
            if "stream_hang" in setup:
                self.send_response(200)
                self.send_header("Content-Type", "application/x-ndjson")
                self.send_header("Connection", "close")
                self.end_headers()
                try:
                    for e in setup.get("events", []):
                        self.wfile.write(json.dumps(e).encode() + b"\n")
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    return
                if "started" in setup:
                    setup["started"].set()
                setup["stream_hang"].wait(3)
                return
            if "started" in setup:
                setup["started"].set()
                setup["release"].wait(3)
            if "delay_s" in setup:
                time.sleep(setup["delay_s"])
            if "error" in setup:
                return self._send(504, {"error": setup["error"]})
            return self._send(200, raw=b"".join(json.dumps(e).encode() + b"\n" for e in setup["events"]),
                              ctype="application/x-ndjson")
        if (m, p) == ("GET", "/v1/vms"):
            return self._send(200, {"vms": [VM]})
        if (m, p) == ("GET", "/v1/vms/vm1"):
            return self._send(200, VM)
        if m == "PATCH" and p.startswith("/v1/vms/"):
            try:
                patch_obj = json.loads(body)
            except ValueError:
                return self._send(400, {"error": {"code": "invalid_json", "message": "malformed json"}})
            if not isinstance(patch_obj, dict) or (set(patch_obj.keys()) - PATCH_KEYS):
                return self._send(400, {"error": {"code": "invalid_json", "message": "unknown field"}})
            if not patch_obj:
                return self._send(400, {"error": {"code": "invalid_request", "message": "empty patch"}})
            if p != "/v1/vms/vm1":
                return self._send(404, {"error": {"code": "not_found", "message": "vm not found", "request_id": "req_2"}})
            return self._send(200, dict(VM, **patch_obj))
        if (m, p) == ("POST", "/v1/vms/vm1/start"):
            extra = json.loads(body) if body else {}
            return self._send(200, dict(VM, state="running", **extra))
        if m == "GET" and p.split("?")[0] == "/v1/usage":
            return self._send(200, {"usage": []})
        if m == "GET" and p.split("?")[0] == "/v1/vms/vm1/usage":
            return self._send(200, {"vm_id": "vm1", "seconds": 42, "by_size": {"default": 42}})
        if (m, p) == ("POST", "/v1/vms/vm1/fork"):
            req = json.loads(body) if body else {}
            return self._send(201, dict(VM, id="vm2", name=req.get("name", ""), labels=req.get("labels") or {}))
        if (m, p) == ("POST", "/v1/vms/vm1/clone"):
            req = json.loads(body) if body else {}
            return self._send(201, dict(VM, id="vm3", name=req.get("name", ""), labels=req.get("labels") or {}))
        if (m, p) == ("POST", "/v1/vms/vm1/stop"):
            return self._send(200, dict(VM, state="stopped"))
        if m == "POST" and p == "/v1/vms/vm1/exec" and Mock.exec_hang is not None and "tar xzf" not in body.decode():
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Connection", "close")
            self.end_headers()
            Mock.exec_hang.wait(3)
            return
        if m == "POST" and p == "/v1/vms/vm1/exec" and "tar xzf" in body.decode():
            cmd = json.loads(body)["cmd"]
            out = ("/home/dev/" + cmd[3] + "\n").encode()
            lines = [{"stream": "stdout", "data_b64": base64.b64encode(out).decode()}, {"exit": 0}]
            return self._send(200, raw=b"".join(json.dumps(x).encode() + b"\n" for x in lines),
                              ctype="application/x-ndjson")
        if m == "POST" and p == "/v1/vms/vm1/exec" and json.loads(body).get("timeout_s") == 5:
            lines = [{"exit": 143, "signal": "SIGTERM", "timed_out": True}]
            return self._send(200, raw=b"".join(json.dumps(x).encode() + b"\n" for x in lines),
                              ctype="application/x-ndjson")
        if m == "POST" and p in ("/v1/vms/vm1/exec", "/v1/vms/vm1/prompt"):
            lines = [{"stream": "stdout", "data_b64": base64.b64encode(b"hello ").decode()},
                     {"stream": "stdout", "data_b64": base64.b64encode(b"world\n").decode()},
                     {"stream": "stderr", "data_b64": base64.b64encode(b"oops\n").decode()},
                     {"exit": 0}]
            return self._send(200, raw=b"".join(json.dumps(x).encode() + b"\n" for x in lines),
                              ctype="application/x-ndjson")
        if p.startswith("/v1/vms/vm1/files?path="):
            if m == "PUT":
                if Mock.push_hang is not None:
                    Mock.push_hang.wait(3)
                Mock.files[p] = body
                return self._send(200, {"ok": True})
            return self._send(200, raw=Mock.files.get(p, b""), ctype="application/octet-stream")
        if (m, p) == ("POST", "/v1/vms/vm1/ssh"):
            return self._send(200, {"host": "10.0.0.5", "port": 22, "user": "ubuntu"})
        if (m, p) == ("GET", "/v1/vms/vm1/ssh"):
            return self._send(200, {"keys": [{"fingerprint": "SHA256:abc+def/", "public_key": "ssh-ed25519 AAAA"}]})
        if m == "DELETE" and p.startswith("/v1/vms/") and "/ssh/" in p:
            fp_enc = p.split("/ssh/", 1)[1]
            if not fp_enc or "/" in fp_enc:
                return self._send(404, {"error": {"code": "not_found", "message": "not found"}})
            return self._send(200, {"ok": True, "fingerprint": urllib.parse.unquote(fp_enc)})
        if m == "POST" and p.startswith("/v1/vms/") and p.endswith("/port-tokens/revoke"):
            mid = p[len("/v1/vms/"):-len("/port-tokens/revoke")]
            if not mid or "/" in mid:
                return self._send(404, {"error": {"code": "not_found", "message": "not found"}})
            return self._send(200, {"ok": True, "generation": 2})
        if (m, p) == ("PUT", "/v1/teams/t1/self-token"):
            return self._send(200, json.loads(body))
        if (m, p) == ("PUT", "/v1/teams/t1/share-allowlist"):
            return self._send(200, json.loads(body))
        if (m, p) == ("GET", "/v1/teams/t1"):
            return self._send(200, {"id": "t1", "name": "acme", "role": "owner", "members": [{"user_id": "u1", "role": "owner"}], "created": "2026-01-01T00:00:00Z"})
        if m == "DELETE" and p.startswith("/v1/teams/") and "/members/" in p:
            return self._send(200, {"ok": True})
        if (m, p) == ("POST", "/v1/keys"):
            req = json.loads(body) if body else {}
            out = {"id": "k2", "user_id": "u1", "team_id": req.get("team_id", "t1"), "name": req.get("name", "key"),
                   "scopes": req.get("scopes") or [], "created": "2026-01-01T00:00:00Z", "token": "boat_new_tok"}
            if req.get("expires_in_s"):
                out["expires"] = "2026-01-02T00:00:00Z"
            return self._send(201, out)
        if m == "GET" and (p == "/v1/events" or p.startswith("/v1/events?")):
            sse = (
                ": keepalive\n\n"
                'id: 11\nevent: vm.started\ndata: {"seq":11,"type":"vm.started","vm_id":"vm1"}\n\n'
                'id: 12\nevent: vm.stopped\ndata: {"seq":12,"type":"vm.stopped","vm_id":"vm1"}\n\n'
            ).encode("utf-8")
            return self._send(200, raw=sse, ctype="text/event-stream")
        if (m, p) == ("PUT", "/v1/secrets/ANTHROPIC_API_KEY"):
            return self._send(200, {"name": "ANTHROPIC_API_KEY"})
        if (m, p) == ("POST", "/v1/vms/vm1/ports"):
            return self._send(201, dict(json.loads(body), guest_port=8080))
        if (m, p) == ("GET", "/v1/vms/vm1/ports/8080/url?ttl_s=600"):
            return self._send(200, {"url": "https://8080-vm1.vm.example/?boat_exp=1&boat_sig=ab", "token": "1.ab",
                                    "auth": "private", "guest_port": 8080})
        if (m, p) == ("POST", "/v1/vms/vm1/commands"):
            return self._send(201, {"id": "c_1", "cmd": json.loads(body)["cmd"], "state": "running"})
        if p.startswith("/v1/vms/vm1/commands/c_1/logs?follow=1"):
            lines = [{"stream": "stdout", "data_b64": base64.b64encode(b"out").decode(), "next_offset": 3}, {"exit": 4}]
            return self._send(200, raw=b"".join(json.dumps(x).encode() + b"\n" for x in lines), ctype="application/x-ndjson")
        if (m, p) == ("GET", "/v1/vms/vm1/commands/c_1"):
            return self._send(200, {"id": "c_1", "cmd": ["x"], "state": "exited", "exit": 4})
        if (m, p) == ("POST", "/v1/vms/vm1/prompts"):
            return self._send(202, {"id": "prm_1", "conversation_id": "conv_1", "agent": "claude", "cmd_id": "c_2", "state": "running"})
        if (m, p) == ("POST", "/v1/vms/vm1/prompts/prm_1/steer"):
            return self._send(202, {"id": "prm_2", "conversation_id": "conv_1", "state": "running"})
        if p.startswith("/v1/vms/vm1/files/search?"):
            return self._send(200, {"matches": [{"path": "/a"}], "truncated": False})
        if (m, p) == ("POST", "/v1/environments"):
            return self._send(201, dict(json.loads(body), id="env_1", version=1))
        if (m, p) == ("PUT", "/v1/environments/base"):
            return self._send(200, dict(json.loads(body), id="env_1", name="base", version=2))
        if m == "POST" and p.startswith("/v1/environments/") and p.endswith("/upgrade"):
            ref = urllib.parse.unquote(p[len("/v1/environments/"):-len("/upgrade")])
            payload = json.loads(body) if body else {}
            ids = payload.get("agentIds")
            upgraded = len(ids) if isinstance(ids, list) else 3
            live = min(upgraded, 2)
            stopped = max(0, upgraded - live)
            return self._send(200, {
                "environment": ref, "version": 2,
                "upgraded": upgraded, "upgraded_live": live, "upgraded_stopped": stopped, "failed": 0,
            })
        if m == "GET" and (p == "/v1/snapshots" or p.startswith("/v1/snapshots?")):
            qs = urllib.parse.parse_qs(urllib.parse.urlsplit(p).query)
            vis = qs.get("visibility", ["default"])[0]
            return self._send(200, {"snapshots": [{"id": "s1", "visibility": vis}]})
        if (m, p) == ("POST", "/v1/keys/k1/rotate"):
            return self._send(200, {"id": "k1", "token": "boat_new", "prev_until": "x"})
        if (m, p) == ("POST", "/v1/webhooks/w1/rotate"):
            return self._send(200, {"id": "w1", "secret": "whsec_new"})
        if (m, p) == ("PUT", "/v1/ai/credentials/anthropic"):
            return self._send(200, {"provider": "anthropic", "last4": "1234", "updated": "x"})
        if (m, p) == ("POST", "/v1/vms/vm1/resize"):
            return self._send(200, dict(VM, size=json.loads(body)["size"]))
        if (m, p) == ("POST", "/v1/vms/vm1/snapshots/s1/shares"):
            return self._send(201, {"id": "shr1", "snapshot_id": "s1", "uses": 0, "code": "bsh_x"})
        if (m, p) == ("GET", "/v1/vms/vm1/snapshots/s1/download"):
            return self._send(200, raw=b"ARCHIVE", ctype="application/zstd")
        if (m, p) == ("PUT", "/v1/telemetry"):
            return self._send(200, {"configured": True, "endpoint": json.loads(body)["endpoint"]})
        if (m, p) == ("DELETE", "/v1/vms/vm1"):
            return self._send(200, {"ok": True})
        return self._send(404, {"error": {"code": "not_found", "message": "vm not found", "request_id": "req_2"}})

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _handle


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = _DaemonHTTPServer(("127.0.0.1", 0), Mock)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = "http://127.0.0.1:%d" % cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()

    def setUp(self):
        Mock.log.clear()
        Mock.peers.clear()
        Mock.idem.clear()
        Mock.vm_setup = None
        Mock.exec_hang = None
        Mock.push_hang = None
        self.b = Boat(self.url, TOKEN)
        self.addCleanup(self.b.close)

    def test_open_timeout_selection(self):
        self.b.timeout = 1.5
        for kwargs, expected in (({}, 1.5), ({"timeout": 0.75}, 0.75),
                                 ({"timeout": 0}, 0), ({"timeout": None}, None)):
            with self.subTest(kwargs=kwargs), patch.object(
                self.b._pool, "request", return_value=_FakeTransportResp()
            ) as transport:
                with self.b._open("GET", "/v1/me", **kwargs):
                    pass
                self.assertEqual(transport.call_args.kwargs["timeout"], expected)

    def test_streaming_paths_disable_transport_timeout(self):
        calls = (
            ("exec", lambda: list(self.b.exec("vm1", ["true"]))),
            ("prompt", lambda: list(self.b.prompt("vm1", "go"))),
            ("follow_command", lambda: list(self.b.follow_command("vm1", "c_1"))),
            ("follow_prompt", lambda: list(self.b.follow_prompt("vm1", "prm_1"))),
            ("iter_events", lambda: list(self.b.iter_events())),
            ("download_snapshot", lambda: self.b.download_snapshot("vm1", "s1", io.BytesIO())),
            ("import_snapshot", lambda: self.b.import_snapshot(b"ARCHIVE")),
        )
        for name, call in calls:
            raw = b'data: {"seq":1,"type":"vm.started"}\n\n' if name == "iter_events" else b'{"exit":0}\n'
            with self.subTest(name=name), patch.object(
                self.b._pool, "request", return_value=_FakeTransportResp(raw)
            ) as transport:
                call()
                self.assertIsNone(transport.call_args.kwargs["timeout"])

    def test_silent_streams_outlast_client_timeout(self):
        self.b.timeout = 0.2
        streams = (
            ("exec", self.b.exec("silent", ["true"])),
            ("prompt", self.b.prompt("silent", "go")),
            ("follow_command", self.b.follow_command("silent", "c_1")),
            ("follow_prompt", self.b.follow_prompt("silent", "prm_1")),
        )
        for name, stream in streams:
            with self.subTest(name=name):
                events = list(stream)
                self.assertEqual(events[0].data, b"late")
                self.assertEqual(events[-1].exit_code, 0)

    def test_create_prompt_fork_stop(self):
        vm = self.b.create_vm(name="dev", size="small", ttl_s=3600)
        self.assertEqual(vm.id, "vm1")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "dev", "size": "small", "ttl_s": 3600})
        out = b"".join(e.data for e in self.b.prompt(vm.id, "write tests", agent="codex") if e.stream == "stdout")
        self.assertEqual(out, b"hello world\n")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"prompt": "write tests", "agent": "codex"})
        fork = self.b.fork(vm.id, name="dev-2")
        self.assertEqual((fork.id, fork.name), ("vm2", "dev-2"))
        self.assertEqual(self.b.stop(vm.id).state, "stopped")

    def test_create_ttl_zero_sent_and_omitted_not(self):
        self.b.create_vm(name="dev", ttl_s=0)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "dev", "ttl_s": 0})
        self.b.create_vm(name="dev")
        self.assertNotIn("ttl_s", json.loads(Mock.log[-1][2]))

    def test_create_raises_on_env_error(self):
        Mock.vm_setup = {"response": {"env_error": "push failed", "agent_ready": False}}
        with self.assertRaises(BoatError) as cm:
            self.b.create_vm(name="dev")
        self.assertEqual(cm.exception.code, "env_failed")
        self.assertIn("push failed", cm.exception.message)
        self.assertIn("vm1", cm.exception.message)
        self.assertEqual(self.b.create_vm(name="dev", allow_env_error=True).raw["env_error"], "push failed")
        with self.assertRaises(BoatError) as cm:
            self.b.create_vms(2, name="w")
        self.assertEqual(cm.exception.code, "env_failed")
        self.assertIn("vm2", cm.exception.message)
        self.assertNotIn("vm1:", cm.exception.message)
        self.assertEqual(len(self.b.create_vms(2, name="w", allow_env_error=True)), 2)
        Mock.vm_setup = None
        self.assertEqual(self.b.create_vm(name="ok").id, "vm1")

    def test_fork_clone_start_raise_on_env_error(self):
        for call in (self.b.fork, self.b.clone_vm, self.b.start):
            with self.assertRaises(BoatError) as cm:
                call("vmenv")
            self.assertEqual(cm.exception.code, "env_failed")
            self.assertIn("push failed", cm.exception.message)
            self.assertIn("vmenv2", cm.exception.message)
            self.assertEqual(call("vmenv", allow_env_error=True).raw["env_error"], "push failed")

    def test_start_env_update_failed_surfaces(self):
        with self.assertRaises(BoatError) as cm:
            self.b.start("vm502")
        self.assertEqual((cm.exception.status, cm.exception.code), (502, "env_update_failed"))

    def test_start_env_start_failed_raises_with_stopped_vm(self):
        for kw in ({}, {"allow_env_error": True}):
            with self.assertRaises(BoatError) as cm:
                self.b.start("vm502c", **kw)
            self.assertEqual((cm.exception.status, cm.exception.code), (502, "env_start_failed"))
            self.assertEqual(cm.exception.vm["state"], "stopped")

    def test_error_exposes_vms_next_to_vm(self):
        with self.assertRaises(BoatError) as cm:
            self.b.start("vm502d")
        self.assertEqual(cm.exception.code, "env_start_failed")
        self.assertEqual(cm.exception.vm["id"], "vm502d")
        self.assertEqual([v["id"] for v in cm.exception.vms], ["vm502d", "vm502e"])
        self.assertEqual(cm.exception.vms[0]["state"], "stopped")
        with self.assertRaises(BoatError) as cm:
            self.b.start("vm502c")
        self.assertIsNone(cm.exception.vms)

    def test_start_env_update_failed_with_vm_body(self):
        with self.assertRaises(BoatError) as cm:
            self.b.start("vm502b")
        self.assertEqual(cm.exception.code, "env_update_failed")
        self.assertEqual(cm.exception.vm["id"], "vm502b")
        vm = self.b.start("vm502b", allow_env_error=True)
        self.assertEqual((vm.id, vm.raw["env_error"]), ("vm502b", "env push failed"))
        with self.assertRaises(BoatError):  # no VM body: still raises even when allowed
            self.b.start("vm502", allow_env_error=True)

    def test_force_stop_fail_fast_and_batch(self):
        self.b.stop("vm1", force=True)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"force": True})
        self.b.stop("vm1")
        self.assertEqual(Mock.log[-1][2], b"")
        self.b.fork("vm1", fail_fast=True)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"fail_fast": True})
        self.b.create_vm(fail_fast=True)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"fail_fast": True})
        vms = self.b.create_vms(3, name="w", secrets=[])
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "w", "secrets": [], "count": 3})
        self.assertEqual([v.name for v in vms], ["w-1", "w-2", "w-3"])

    def test_exec_streaming_and_run(self):
        events = list(self.b.exec("vm1", ["ls", "-la"]))
        self.assertEqual(events[-1].exit_code, 0)
        self.assertEqual(len(events), 4)
        res = self.b.run("vm1", ["ls"])
        self.assertEqual((res.stdout, res.stderr, res.exit_code), (b"hello world\n", b"oops\n", 0))

    def test_files_and_ssh(self):
        self.b.write_file("vm1", "/tmp/a b.txt", b"\x00data")
        self.assertEqual(self.b.read_file("vm1", "/tmp/a b.txt"), b"\x00data")
        self.assertIn("path=%2Ftmp%2Fa%20b.txt", Mock.log[-1][1])
        self.assertEqual(self.b.ssh("vm1", "ssh-ed25519 AAAA x").port, 22)

    def test_errors(self):
        with self.assertRaises(BoatError) as cm:
            self.b.get_vm("nope")
        self.assertEqual((cm.exception.status, cm.exception.code, cm.exception.request_id), (404, "not_found", "req_2"))
        with self.assertRaises(BoatError) as cm:
            Boat(self.url, "wrong").list_vms()
        self.assertEqual(cm.exception.status, 401)

    def test_list_and_misc(self):
        self.assertEqual([v.name for v in self.b.list_vms()], ["dev"])
        self.b.set_secret("ANTHROPIC_API_KEY", "sk")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"value": "sk"})
        self.b.set_secret("ANTHROPIC_API_KEY", "sk", inject=True)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"value": "sk", "inject": True})
        self.b.delete("vm1")

    def test_ports_and_secret_selection(self):
        self.b.add_port("vm1", 8080)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"port": 8080, "auth": "private"})
        self.b.add_port("vm1", 8080, public=True)
        self.assertEqual(json.loads(Mock.log[-1][2])["auth"], "public")
        self.assertEqual(self.b.port_url("vm1", 8080, ttl_s=600)["token"], "1.ab")
        self.b.create_vm(name="x", secrets=[])
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "x", "secrets": []})
        self.b.create_vm(name="y", secrets=["A"])
        self.assertEqual(json.loads(Mock.log[-1][2])["secrets"], ["A"])

    def test_detached_commands(self):
        c = self.b.start_command("vm1", ["sleep", "9"], cwd="/tmp")
        self.assertEqual((c.id, c.state), ("c_1", "running"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"cmd": ["sleep", "9"], "cwd": "/tmp"})
        ev = list(self.b.follow_command("vm1", "c_1"))
        self.assertEqual((ev[0].data, ev[-1].exit_code), (b"out", 4))
        self.assertEqual(self.b.command_status("vm1", "c_1").exit, 4)

    def test_follow_absolute_resume_cursors(self):
        for follow, path in ((self.b.follow_command, "commands/c_1"),
                             (self.b.follow_prompt, "prompts/c_1")):
            with self.subTest(path=path):
                # Retention skipped bytes before this four-byte UTF-8 chunk.
                chunk = "尾\n".encode("utf-8")
                wire = [{"stream": "stdout", "data_b64": base64.b64encode(chunk).decode(),
                         "next_offset": 1004},
                        {"reconnect": True, "next_offset": 1004}]
                terminal = [{"stream": "stderr", "data_b64": "IQ==", "next_offset": 1005},
                            {"exit": 143, "signal": "SIGTERM", "next_offset": 1005}]
                responses = [_FakeTransportResp(b"".join(json.dumps(e).encode() + b"\n" for e in events))
                             for events in (wire, terminal)]
                with patch.object(self.b._pool, "request", side_effect=responses) as transport:
                    stream = follow("vm1", "c_1", offset=700)
                    event = next(stream)
                    self.assertEqual((event.data, event.next_offset), (chunk, 1004))
                    with self.assertRaises(BoatError) as cm:
                        next(stream)
                    error = cm.exception
                    self.assertEqual((error.status, error.code, error.request_id), (409, "reconnect", ""))
                    self.assertEqual(error.next_offset, 1004)
                    resumed = list(follow("vm1", "c_1", offset=error.next_offset))
                    self.assertEqual((resumed[0].data, resumed[0].next_offset), (b"!", 1005))
                    self.assertEqual((resumed[-1].exit_code, resumed[-1].error,
                                      resumed[-1].next_offset), (143, "SIGTERM", 1005))
                    self.assertEqual([call.args[0] for call in transport.call_args_list],
                                     [self.url + "/v1/vms/vm1/" + path + "/logs?follow=1&offset=" + str(n)
                                      for n in (700, 1004)])
                    self.assertTrue(all(call.kwargs["timeout"] is None
                                        for call in transport.call_args_list))

    def test_follow_missing_cursors_are_unknown(self):
        for follow in (self.b.follow_command, self.b.follow_prompt):
            with self.subTest(follow=follow.__name__):
                raw = b'{"stream":"stdout","data_b64":"eA=="}\n{"reconnect":true}\n'
                with patch.object(self.b._pool, "request", return_value=_FakeTransportResp(raw)):
                    stream = follow("vm1", "id", offset=700)
                    self.assertIsNone(next(stream).next_offset)
                    with self.assertRaises(BoatError) as cm:
                        next(stream)
                    self.assertIsNone(cm.exception.next_offset)

    def test_resume_cursor_constructor_compatibility(self):
        event = ExecEvent("stderr", b"x", 2, "SIGINT")
        self.assertEqual((event.stream, event.data, event.exit_code, event.error),
                         ("stderr", b"x", 2, "SIGINT"))
        self.assertIsNone(event.next_offset)
        error = BoatError(400, "bad_request", "bad", "req_1")
        self.assertEqual(str(error), "bad (HTTP 400, req_1)")
        self.assertIsNone(error.next_offset)

    def test_follow_zero_cursor_and_legacy_terminal(self):
        for follow in (self.b.follow_command, self.b.follow_prompt):
            with self.subTest(follow=follow.__name__):
                raw = b'{"stream":"stdout","next_offset":0}\n{"exit":0}\n'
                with patch.object(self.b._pool, "request", return_value=_FakeTransportResp(raw)):
                    events = list(follow("vm1", "id"))
                self.assertEqual(events[0].next_offset, 0)
                self.assertEqual(events[0].data, b"")
                self.assertEqual(events[-1].exit_code, 0)
                self.assertIsNone(events[-1].next_offset)

    def test_async_prompt_and_steer(self):
        run = self.b.start_prompt("vm1", "go", agent="claude", new=True)
        self.assertEqual((run.id, run.conversation_id), ("prm_1", "conv_1"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"prompt": "go", "agent": "claude", "new": True})
        self.assertEqual(self.b.steer_prompt("vm1", "prm_1", "stop").id, "prm_2")

    def test_rotation_env_ai_resize_share(self):
        self.assertEqual(self.b.rotate_key("k1", overlap_s=0)["token"], "boat_new")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"overlap_s": 0})
        self.assertEqual(self.b.rotate_webhook("w1")["secret"], "whsec_new")
        e = self.b.create_environment("base", setup_script="npm ci", secrets=[])
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "base", "setup_script": "npm ci", "secrets": []})
        self.assertEqual(e["version"], 1)
        self.assertEqual(self.b.set_ai_credential("anthropic", "sk-1234")["last4"], "1234")
        self.assertEqual(self.b.resize("vm1", size="large", discard_memory=True).size, "large")
        self.assertEqual(self.b.share_snapshot("vm1", "s1", ttl_s=60)["code"], "bsh_x")
        import io
        buf = io.BytesIO()
        self.assertEqual(self.b.download_snapshot("vm1", "s1", buf), 7)
        self.assertEqual(buf.getvalue(), b"ARCHIVE")
        self.assertEqual(self.b.search_files("vm1", "/", "a", content=True)[0]["path"], "/a")
        self.assertIn("content=1", Mock.log[-1][1])
        self.assertTrue(self.b.set_telemetry("https://o.example")["configured"])

    def test_verify_webhook_during_rotation(self):
        body = b"{}"
        new = hmac.new(b"new", b"1000." + body, hashlib.sha256).hexdigest()
        old = hmac.new(b"old", b"1000." + body, hashlib.sha256).hexdigest()
        hdr = "t=1000,v1=%s,v1=%s" % (new, old)
        self.assertTrue(verify_webhook("new", hdr, body, now=1000))
        self.assertTrue(verify_webhook("old", hdr, body, now=1000))
        self.assertFalse(verify_webhook("other", hdr, body, now=1000))

    def _tree(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d)
        root = os.path.join(d, "proj")
        os.makedirs(os.path.join(root, ".git"))
        for rel, mode in (("run.sh", 0o755), ("a/b.txt", 0o644), (".git/config", 0o644)):
            os.makedirs(os.path.dirname(os.path.join(root, rel)), exist_ok=True)
            with open(os.path.join(root, rel), "w") as f:
                f.write("x")
            os.chmod(os.path.join(root, rel), mode)
        os.symlink("run.sh", os.path.join(root, "link"))
        return root

    def test_push_packs_and_unpacks(self):
        root = self._tree()  # .git has no HEAD, so git refuses it: a plain directory walk
        got = self.b.push("vm1", root)
        self.assertEqual(got, {"path": "/home/dev/proj", "files": 3})
        put = [b for m, p, b in Mock.log if m == "PUT" and "boat-push" in p][-1]
        with tarfile.open(fileobj=io.BytesIO(put), mode="r:gz") as tar:
            self.assertEqual(sorted(tar.getnames()), ["a/b.txt", "link", "run.sh"])
            self.assertTrue(tar.getmember("run.sh").mode & 0o100)
            self.assertTrue(tar.getmember("link").issym())
        with self.assertRaises(ValueError):
            self.b.push("vm1", "/no/such/dir")

    def _run_vm1(self):
        def create(**kw):
            create.kw = kw
            return VMRecord.from_json({"id": "vm1", "name": "", "size": "large", "state": "running", "image": ""})
        self.b.create_vm = create
        return create

    def test_run_job_deletes_and_returns_exit(self):
        create = self._run_vm1()
        before = len(Mock.log)
        res = self.b.run_job(["true"], size="large")
        self.assertEqual((res.exit_code, res.size, res.vm_id, res.deleted), (0, "large", "vm1", True))
        self.assertEqual(create.kw["idle_stop_s"], 900)
        self.assertIn(("DELETE", "/v1/vms/vm1"), [(m, p) for m, p, _ in Mock.log[before:]])

    def test_run_job_waits_for_setup_before_push_and_exec(self):
        started, release = threading.Event(), threading.Event()
        Mock.vm_setup = {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": 0}],
                         "started": started, "release": release}
        root = self._tree()
        results, errors = [], []

        def run():
            try:
                results.append(self.b.run_job(["make"], push=root, environment="base"))
            except Exception as exc:
                errors.append(exc)

        job = threading.Thread(target=run)
        job.start()
        try:
            self.assertTrue(started.wait(1), "job skipped the pending setup command")
            self.assertEqual([(m, p) for m, p, _ in Mock.log], [
                ("POST", "/v1/vms"),
                ("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0"),
            ])
            self.assertTrue(job.is_alive())
        finally:
            release.set()
            job.join(3)
        self.assertFalse(job.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual((results[0].exit_code, results[0].deleted), (0, True))
        self.assertEqual([(m, p.split("?")[0]) for m, p, _ in Mock.log], [
            ("POST", "/v1/vms"), ("GET", "/v1/vms/vm1/commands/setup1/logs"),
            ("PUT", next(p.split("?")[0] for m, p, _ in Mock.log if m == "PUT")),
            ("POST", "/v1/vms/vm1/exec"), ("POST", "/v1/vms/vm1/exec"),
            ("DELETE", "/v1/vms/vm1"),
        ])
        self.assertEqual(results[0].stdout, b"hello world\n")

        # Successful setup with keep=True preserves the VM.
        Mock.log.clear()
        Mock.vm_setup = {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": 0}]}
        kept = self.b.run_job(["make"], keep=True, environment="base")
        self.assertEqual((kept.exit_code, kept.deleted), (0, False))
        self.assertEqual([(m, p) for m, p, _ in Mock.log], [
            ("POST", "/v1/vms"),
            ("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0"),
            ("POST", "/v1/vms/vm1/exec"),
        ])

        # Omitted or empty setup_cmd_id / setup_error skips follow_command and runs normally.
        for response in ({}, {"setup_cmd_id": "", "setup_error": ""}):
            with self.subTest(response=response):
                Mock.log.clear()
                Mock.vm_setup = {"response": response}
                res = self.b.run_job(["make"])
                self.assertEqual((res.exit_code, res.deleted), (0, True))
                self.assertEqual([(m, p) for m, p, _ in Mock.log], [
                    ("POST", "/v1/vms"),
                    ("POST", "/v1/vms/vm1/exec"),
                    ("DELETE", "/v1/vms/vm1"),
                ])

    def test_run_job_setup_failures_abort_and_respect_keep(self):
        cases = (
            ("start", {"response": {"setup_error": "could not start the setup script"}},
             "setup_failed", "could not start"),
            ("env_error", {"response": {"env_error": "push failed", "agent_ready": False}},
             "env_failed", "push failed"),
            ("start_with_cmd", {"response": {"setup_error": "could not start", "setup_cmd_id": "setup1"}},
             "setup_failed", "could not start"),
            ("exit", {"response": {"setup_cmd_id": "setup1"}, "events": [
                {"stream": "stderr", "data_b64": base64.b64encode(b"missing dependency\n").decode()},
                {"exit": 7}]}, "setup_failed", "missing dependency"),
            ("exit_stdout", {"response": {"setup_cmd_id": "setup1"}, "events": [
                {"stream": "stdout", "data_b64": base64.b64encode(b"apt lock busy\n").decode()},
                {"exit": 100}]}, "setup_failed", "apt lock busy"),
            ("timeout", {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": 124}]},
             "setup_failed", "124"),
            ("signal", {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": None, "signal": "SIGTERM"}]},
             "setup_failed", "SIGTERM"),
            ("signal_with_exit", {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": 0, "signal": "SIGKILL"}]},
             "setup_failed", "SIGKILL"),
            ("no_exit", {"response": {"setup_cmd_id": "setup1"}, "events": []},
             "setup_failed", "without an exit code"),
            ("partial_no_exit", {"response": {"setup_cmd_id": "setup1"}, "events": [
                {"stream": "stdout", "data_b64": base64.b64encode(b"step 1\n").decode()}]},
             "setup_failed", "without an exit code"),
            ("upstream_timeout", {"response": {"setup_cmd_id": "setup1"},
              "error": {"code": "upstream_error", "message": "setup timed out", "request_id": "req_setup"}},
             "upstream_error", "setup timed out"),
        )
        root = self._tree()
        for name, setup, code, message in cases:
            for keep in (False, True):
                with self.subTest(name=name, keep=keep):
                    Mock.log.clear()
                    Mock.vm_setup = setup
                    with self.assertRaises(BoatError) as caught:
                        self.b.run_job(["make"], push=root, keep=keep, environment="base")
                    self.assertEqual(caught.exception.code, code)
                    self.assertIn(message, caught.exception.message)
                    paths = [(m, p) for m, p, _ in Mock.log]
                    expected = [("POST", "/v1/vms")]
                    if "setup_cmd_id" in setup["response"] and not setup["response"].get("setup_error"):
                        expected.append(("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0"))
                    if not keep:
                        expected.append(("DELETE", "/v1/vms/vm1"))
                    self.assertEqual(paths, expected)

    def test_iter_lines_autoclosed_http_response_no_ebadf(self):
        payload = b'{"stream":"stdout","data_b64":"aGVsbG8="}\n{"exit":0}\n'
        raw_http = (
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: application/x-ndjson\r\n"
            b"Content-Length: " + str(len(payload)).encode("ascii") + b"\r\n"
            b"\r\n" + payload
        )
        client_sock, server_sock = socket.socketpair()
        try:
            client_sock.settimeout(5.0)
            server_sock.sendall(raw_http)
            server_sock.close()
            resp = http.client.HTTPResponse(client_sock)
            resp.begin()
            with resp:
                lines = list(_iter_lines(resp, time.monotonic() + 5.0))
            self.assertEqual(lines, [
                b'{"stream":"stdout","data_b64":"aGVsbG8="}\n',
                b'{"exit":0}\n',
            ])
        finally:
            try:
                client_sock.close()
            except OSError:
                pass
            try:
                server_sock.close()
            except OSError:
                pass

        # Also simulate an explicit socket object that raises EBADF once the final line is read.
        class ClosingSock:
            def __init__(self):
                self.closed = False
                self.timeout = 2.0

            def fileno(self):
                if self.closed:
                    raise OSError(errno.EBADF, "Bad file descriptor")
                return 10

            def gettimeout(self):
                if self.closed:
                    raise OSError(errno.EBADF, "Bad file descriptor")
                return self.timeout

            def settimeout(self, val):
                if self.closed:
                    raise OSError(errno.EBADF, "Bad file descriptor")
                self.timeout = val

        class AutoCloseResp:
            def __init__(self):
                self._sock = ClosingSock()
                self._lines = [b'{"exit":0}\n']

            def isclosed(self):
                return False  # force settimeout path to hit EBADF guard

            def readline(self):
                if self._lines:
                    out = self._lines.pop(0)
                    self._sock.closed = True
                    return out
                return b""

        fake_resp = AutoCloseResp()
        self.assertEqual(list(_iter_lines(fake_resp, time.monotonic() + 5.0)), [b'{"exit":0}\n'])

    def test_run_job_nonpositive_timeout_rejected_immediately(self):
        for bad in (0, 0.0, -1, -0.5):
            with self.subTest(timeout_s=bad):
                Mock.log.clear()
                with self.assertRaises(TimeoutError) as caught:
                    self.b.run_job(["make"], timeout_s=bad)
                self.assertIsInstance(caught.exception, BoatError)
                self.assertIsInstance(caught.exception, BoatTimeoutError)
                self.assertEqual(Mock.log, [])

    def test_run_job_setup_reconnect_resumes_and_honors_deadline(self):
        Mock.log.clear()
        Mock.vm_setup = {
            "response": {"setup_cmd_id": "setup1"},
            "sequences": [
                [
                    {"stream": "stdout", "data_b64": base64.b64encode(b"abc").decode(), "next_offset": 3},
                    {"reconnect": True, "next_offset": 3},
                ],
                [
                    {"reconnect": True},  # no next_offset: keep offset=3
                ],
                [
                    {"stream": "stdout", "data_b64": base64.b64encode(b"def\n").decode(), "next_offset": 9},
                    {"reconnect": True, "next_offset": 9},
                ],
                [
                    {"exit": 0},
                ],
            ],
        }
        res = self.b.run_job(["make"], timeout_s=10, environment="base")
        self.assertEqual((res.exit_code, res.deleted), (0, True))
        follow_paths = [p for m, p, _ in Mock.log if m == "GET" and "/commands/setup1/logs" in p]
        self.assertEqual(follow_paths, [
            "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0",
            "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=3",
            "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=3",
            "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=9",
        ])

        # Repeated reconnects expire cleanly when timeout_s runs out.
        for keep in (False, True):
            with self.subTest(keep=keep):
                Mock.log.clear()
                Mock.vm_setup = {
                    "response": {"setup_cmd_id": "setup1"},
                    "sequences": [
                        [
                            {"stream": "stderr", "data_b64": base64.b64encode(b"still booting\n").decode(), "next_offset": 14},
                            {"reconnect": True, "next_offset": 14},
                        ],
                        [{"reconnect": True, "next_offset": 14}],
                    ],
                }
                with self.assertRaises(BoatError) as caught:
                    self.b.run_job(["make"], timeout_s=0.12, keep=keep, environment="base")
                self.assertEqual(caught.exception.code, "setup_failed")
                self.assertIn("timed out", caught.exception.message)
                self.assertIn("still booting", caught.exception.message)
                paths = [(m, p) for m, p, _ in Mock.log]
                self.assertEqual(paths[0], ("POST", "/v1/vms"))
                self.assertIn(("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0"), paths)
                self.assertIn(("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=14"), paths)
                if keep:
                    self.assertNotIn(("DELETE", "/v1/vms/vm1"), paths)
                else:
                    self.assertEqual(paths[-1], ("DELETE", "/v1/vms/vm1"))

    def test_run_job_end_to_end_deadline_across_push_and_exec(self):
        root = self._tree()
        # Hung push obeys remaining run_job deadline -> exit_code 124 and cleans up VM.
        hang_push = threading.Event()
        Mock.push_hang = hang_push
        try:
            res = self.b.run_job(["make"], push=root, timeout_s=0.15)
        finally:
            hang_push.set()
            Mock.push_hang = None
        self.assertEqual((res.exit_code, res.deleted), (124, True))
        self.assertEqual(Mock.log[-1][:2], ("DELETE", "/v1/vms/vm1"))

        # Hung exec stream obeys remaining client socket deadline -> exit_code 124.
        Mock.log.clear()
        hang_exec = threading.Event()
        Mock.exec_hang = hang_exec
        try:
            res = self.b.run_job(["sleep", "99"], push=root, timeout_s=0.25)
        finally:
            hang_exec.set()
            Mock.exec_hang = None
        self.assertEqual((res.exit_code, res.deleted), (124, True))
        exec_bodies = [json.loads(b) for m, p, b in Mock.log if (m, p) == ("POST", "/v1/vms/vm1/exec") and "tar xzf" not in b.decode()]
        self.assertEqual(len(exec_bodies), 1)
        self.assertEqual(exec_bodies[0]["timeout_s"], 1)

    def test_sdk_parity_update_fork_start_env_exec_stdin_tty(self):
        self.assertIs(VMHandle, VMRecord)
        vm = self.b.get_vm("vm1")
        updated = vm.update(
            name="renamed",
            ttl_s=3600,
            extend_s=300,
            labels={"env": "prod"},
            idle_stop_s=120,
        )
        self.assertEqual(updated.name, "renamed")
        self.assertEqual(vm.name, "renamed")
        self.assertEqual(vm.labels, {"env": "prod"})
        patch_method, patch_path, patch_raw = Mock.log[-1]
        self.assertEqual((patch_method, patch_path), ("PATCH", "/v1/vms/vm1"))
        self.assertEqual(json.loads(patch_raw), {
            "name": "renamed",
            "ttl_s": 3600,
            "extend_s": 300,
            "labels": {"env": "prod"},
            "idle_stop_s": 120,
        })

        # Client-level update_vm also works with exact server fields and rejects empty or removed kwargs.
        u2 = self.b.update_vm("vm1", name="v2", idle_stop_s=-1)
        self.assertEqual(u2.name, "v2")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "v2", "idle_stop_s": -1})
        n = len(Mock.log)
        with self.assertRaisesRegex(ValueError, "host-managed"):
            self.b.update_vm("vm1", resume_on_boot=True)
        with self.assertRaisesRegex(ValueError, "host-managed"):
            vm.update(resume_on_boot=False)
        self.assertEqual(len(Mock.log), n)
        with self.assertRaises(ValueError):
            self.b.update_vm("vm1")
        with self.assertRaises(ValueError):
            vm.update()
        for bad_kw in ({"idle_timeout_s": 10}, {"hard_ttl_s": 10}, {"keep_warm": True}):
            with self.assertRaises(TypeError):
                self.b.update_vm("vm1", **bad_kw)
            with self.assertRaises(TypeError):
                vm.update(**bad_kw)
        with self.assertRaises(BoatError) as bad_patch:
            self.b._call("PATCH", "/v1/vms/vm1", {"idle_timeout_s": 10})
        self.assertEqual((bad_patch.exception.status, bad_patch.exception.code), (400, "invalid_json"))

        # create_vm / create_vms accept self_api.
        self.b.create_vm(name="isolated", self_api=False)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "isolated", "self_api": False})
        self.b.create_vms(2, name="pool", self_api=True)
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "pool", "self_api": True, "count": 2})

        # fork, clone, and start accept optional env, no_env, ttl_s, and labels on both VM and Boat.
        child = vm.fork(name="child-1", fail_fast=True, env={"FOO": "bar"},
                        labels={"tier": "ci"}, no_env=True, ttl_s=900)
        self.assertEqual(child.id, "vm2")
        self.assertEqual(child.labels, {"tier": "ci"})
        self.assertEqual(json.loads(Mock.log[-1][2]), {
            "name": "child-1", "fail_fast": True, "env": {"FOO": "bar"},
            "labels": {"tier": "ci"}, "no_env": True, "ttl_s": 900,
        })

        cloned = vm.clone(name="clone-1", fail_fast=True, env={"C": "1"},
                          labels={"role": "worker"}, no_env=False, ttl_s=1200)
        self.assertEqual((cloned.id, cloned.name, cloned.labels), ("vm3", "clone-1", {"role": "worker"}))
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/vms/vm1/clone"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {
            "name": "clone-1", "fail_fast": True, "env": {"C": "1"},
            "labels": {"role": "worker"}, "no_env": False, "ttl_s": 1200,
        })

        vm.start(fail_fast=True, env={"BOOT": "1"}, no_env=True, ttl_s=600)
        self.assertEqual(json.loads(Mock.log[-1][2]), {
            "fail_fast": True, "env": {"BOOT": "1"}, "no_env": True, "ttl_s": 600,
        })

        # environment is sent on start/fork/clone; combined with no_env it raises locally.
        n = len(Mock.log)
        vm.start(environment="staging")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"environment": "staging"})
        vm.fork(environment="base")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"environment": "base"})
        vm.clone(environment="staging")
        self.assertEqual(json.loads(Mock.log[-1][2]), {"environment": "staging"})
        n = len(Mock.log)
        for call in (self.b.start, self.b.fork, self.b.clone_vm):
            with self.assertRaises(ValueError):
                call("vm1", environment="staging", no_env=True)
        self.assertEqual(len(Mock.log), n)

        # Bare start sends empty body as before.
        self.b.start("vm1")
        self.assertEqual(Mock.log[-1][2], b"")

        # exec and run accept stdin (str or bytes -> stdin_b64) and tty=True.
        r1 = vm.run(["cat"], stdin="hello stdin\n", tty=True)
        self.assertEqual(r1.exit_code, 0)
        self.assertEqual(json.loads(Mock.log[-1][2]), {
            "cmd": ["cat"],
            "stdin_b64": base64.b64encode(b"hello stdin\n").decode("ascii"),
            "tty": True,
        })

        r2 = self.b.run("vm1", ["cat"], stdin=b"\x00\xff", tty=False)
        self.assertEqual(r2.exit_code, 0)
        self.assertEqual(json.loads(Mock.log[-1][2]), {
            "cmd": ["cat"],
            "stdin_b64": base64.b64encode(b"\x00\xff").decode("ascii"),
        })

    def test_run_job_setup_respects_timeout(self):
        root = self._tree()
        for keep in (False, True):
            with self.subTest(keep=keep):
                Mock.log.clear()
                hang = threading.Event()
                Mock.vm_setup = {
                    "response": {"setup_cmd_id": "setup1"},
                    "events": [{"stream": "stdout", "data_b64": base64.b64encode(b"installing...\n").decode()}],
                    "stream_hang": hang,
                }
                try:
                    with self.assertRaises(BoatError) as caught:
                        self.b.run_job(["make"], push=root, timeout_s=0.15, keep=keep, environment="base")
                finally:
                    hang.set()
                self.assertEqual(caught.exception.code, "setup_failed")
                self.assertIn("timed out", caught.exception.message)
                self.assertIn("installing...", caught.exception.message)
                expected = [
                    ("POST", "/v1/vms"),
                    ("GET", "/v1/vms/vm1/commands/setup1/logs?follow=1&offset=0"),
                ]
                if not keep:
                    expected.append(("DELETE", "/v1/vms/vm1"))
                self.assertEqual([(m, p) for m, p, _ in Mock.log], expected)

        # Without timeout_s, silent setup streams outlast client's short request timeout.
        Mock.log.clear()
        self.b.timeout = 0.1
        Mock.vm_setup = {"response": {"setup_cmd_id": "setup1"}, "delay_s": 0.25, "events": [{"exit": 0}]}
        res = self.b.run_job(["make"], environment="base")
        self.assertEqual((res.exit_code, res.deleted), (0, True))

        # With timeout_s, remaining budget after setup is passed to exec.
        Mock.log.clear()
        self.b.timeout = 60.0
        now = [100.0]
        orig_follow = self.b.follow_command

        def advancing_follow(vm_id, cmd_id, offset=0, timeout=None):
            self.assertAlmostEqual(timeout, 10.0, places=3)
            for ev in orig_follow(vm_id, cmd_id, offset=offset, timeout=timeout):
                yield ev
            now[0] += 2.4  # 7.6s remaining -> ceil(7.6) == 8

        Mock.vm_setup = {"response": {"setup_cmd_id": "setup1"}, "events": [{"exit": 0}]}
        with patch("boat.client.time.monotonic", side_effect=lambda: now[0]), \
             patch.object(self.b, "follow_command", side_effect=advancing_follow):
            res = self.b.run_job(["make"], timeout_s=10, environment="base")
        self.assertEqual(res.exit_code, 0)
        exec_body = [json.loads(b) for m, p, b in Mock.log if (m, p) == ("POST", "/v1/vms/vm1/exec")][-1]
        self.assertEqual(exec_body["timeout_s"], 8)

    def test_run_job_timeout_is_124_and_keep_keeps(self):
        self._run_vm1()
        before = len(Mock.log)
        res = self.b.run_job(["sleep", "9"], timeout_s=5, keep=True)
        self.assertEqual((res.exit_code, res.deleted), (124, False))
        self.assertNotIn(("DELETE", "/v1/vms/vm1"), [(m, p) for m, p, _ in Mock.log[before:]])

    def test_run_job_push_runs_there(self):
        self._run_vm1()
        self.b.run_job(["make"], push=self._tree())
        last = [json.loads(b) for m, p, b in Mock.log if p == "/v1/vms/vm1/exec"][-1]
        self.assertEqual(last["cwd"], "/home/dev/proj")
        self.b.run_job(["make"], cwd="sub")
        last = [json.loads(b) for m, p, b in Mock.log if p == "/v1/vms/vm1/exec"][-1]
        self.assertEqual(last["cmd"][3:], ["sub", "make"])
        with self.assertRaises(ValueError):
            self.b.run_job(["make"], push="/no/such/dir")

    def test_run_job_push_with_cwd_still_uploads(self):
        self._run_vm1()
        self.b.run_job(["make"], push=self._tree(), cwd="/work")
        paths = [p for m, p, b in Mock.log]
        self.assertTrue(any(p == "/v1/vms/vm1/exec" and b"tar" in b for m, p, b in Mock.log), paths)
        execs = [json.loads(b) for m, p, b in Mock.log if p == "/v1/vms/vm1/exec"]
        self.assertGreaterEqual(len(execs), 2)  # push (unpack) then the command
        self.assertEqual(execs[-1]["cwd"], "/work")
        self.assertEqual(execs[-1]["cmd"], ["make"])

    def test_idempotency_key_on_create_and_fork(self):
        self.b.create_vm(name="dev", idempotency_key="k-create")
        self.assertEqual(Mock.idem[-1], ("POST", "/v1/vms", "k-create"))
        self.assertNotIn("idempotency_key", json.loads(Mock.log[-1][2]))
        self.b.create_vms(2, name="many", idempotency_key="k-many")
        self.assertEqual(Mock.idem[-1], ("POST", "/v1/vms", "k-many"))
        self.b.fork("vm1", idempotency_key="k-fork")
        self.assertEqual(Mock.idem[-1], ("POST", "/v1/vms/vm1/fork", "k-fork"))
        self.b.create_vm(name="plain")
        self.assertIsNone(Mock.idem[-1][2])
        with self.assertRaises(ValueError):
            self.b.create_vm(idempotency_key="")
        with self.assertRaises(ValueError):
            self.b.fork("vm1", idempotency_key="x" * 256)

    def test_usage_passes_from_and_to(self):
        self.b.usage(frm="2026-01-01", to="2026-02-01")
        q = urllib.parse.parse_qs(urllib.parse.urlparse(Mock.log[-1][1]).query)
        self.assertEqual(q, {"from": ["2026-01-01"], "to": ["2026-02-01"]})
        self.b.usage(to="2026-02-01")
        self.assertEqual(Mock.log[-1][1], "/v1/usage?to=2026-02-01")

    def test_usage_accepts_date_objects(self):
        import datetime as dt
        self.b.usage(frm=dt.date(2026, 1, 1),
                     to=dt.datetime(2026, 2, 1, 3, 0, tzinfo=dt.timezone(dt.timedelta(hours=2))))
        q = urllib.parse.parse_qs(urllib.parse.urlparse(Mock.log[-1][1]).query)
        self.assertEqual(q, {"from": ["2026-01-01"], "to": ["2026-02-01T01:00:00Z"]})
        self.b.usage(frm=dt.datetime(2026, 1, 1, 5, 6, 7))
        q = urllib.parse.parse_qs(urllib.parse.urlparse(Mock.log[-1][1]).query)
        self.assertEqual(q, {"from": ["2026-01-01T05:06:07Z"]})

    def test_host_port(self):
        got = self.b.host_port("vm1", 8080, public=True, ttl_s=600)
        self.assertIn("8080-vm1", got["url"])
        posted = [json.loads(b) for m, p, b in Mock.log if (m, p) == ("POST", "/v1/vms/vm1/ports")][-1]
        self.assertEqual(posted["auth"], "public")

    def test_verify_webhook(self):
        body = b'{"type":"vm.stopped"}'
        sig = hmac.new(b"whsec_x", b"1000." + body, hashlib.sha256).hexdigest()
        hdr = "t=1000,v1=" + sig
        self.assertTrue(verify_webhook("whsec_x", hdr, body, now=1001))
        self.assertFalse(verify_webhook("whsec_x", hdr, body + b" ", now=1001))
        self.assertFalse(verify_webhook("whsec_x", hdr, body, now=5000))
        self.assertFalse(verify_webhook("whsec_x", "garbage", body))

    def test_keep_alive_connection_pooling_and_stale_retry(self):
        with Boat(self.url, TOKEN) as client:
            Mock.peers.clear()
            self.assertEqual(client.list_vms()[0].id, "vm1")
            self.assertEqual(client.get_vm("vm1").name, "dev")
            self.assertEqual(client.update_vm("vm1", ttl_s=120).id, "vm1")
            client.write_file("vm1", "/tmp/pool.txt", b"pooled")
            self.assertEqual(client.read_file("vm1", "/tmp/pool.txt"), b"pooled")
            self.assertEqual(len(set(Mock.peers)), 1, "expected all 5 sequential requests to reuse 1 keep-alive socket")
            self.assertEqual(sum(len(v) for v in client._pool._idle.values()), 1)
        self.assertEqual(sum(len(v) for v in client._pool._idle.values()), 0)

        # Simulate a server that drops an idle keep-alive connection mid-reuse (RemoteDisconnected).
        accept_count = [0]

        class DropSecondOnFirstSocket(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def handle_one_request(self):
                if getattr(self, "_served_one", False):
                    # Drop the reused socket immediately as the second request arrives -> RemoteDisconnected.
                    self.close_connection = True
                    try:
                        self.connection.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    self.connection.close()
                    return
                self._served_one = True
                super().handle_one_request()

            def do_GET(self):
                accept_count[0] += 1
                raw = json.dumps({"vms": [VM]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        drop_srv = _DaemonHTTPServer(("127.0.0.1", 0), DropSecondOnFirstSocket)
        threading.Thread(target=drop_srv.serve_forever, daemon=True).start()
        self.addCleanup(lambda: (drop_srv.shutdown(), drop_srv.server_close()))
        drop_url = "http://127.0.0.1:%d" % drop_srv.server_address[1]
        with Boat(drop_url, TOKEN) as retry_client:
            self.assertEqual(retry_client.list_vms()[0].id, "vm1")
            self.assertEqual(retry_client.list_vms()[0].id, "vm1")
        self.assertEqual(accept_count[0], 2)

    def test_run_jobs_parallel_order_concurrency_and_cleanup(self):
        lock = threading.Lock()
        created_ids = []
        deleted_ids = []
        in_flight = [0]
        peak_in_flight = [0]
        seq = [0]
        fail_vm = [None]

        class JobServer(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send_json(self, code, obj):
                raw = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n)) if n else {}
                if self.path == "/v1/vms":
                    with lock:
                        seq[0] += 1
                        vid = "jobvm-%d" % seq[0]
                        created_ids.append(vid)
                        in_flight[0] += 1
                        if in_flight[0] > peak_in_flight[0]:
                            peak_in_flight[0] = in_flight[0]
                    return self._send_json(201, dict(VM, id=vid, size=body.get("size") or "small"))
                if self.path.endswith("/exec"):
                    vid = self.path.split("/")[3]
                    cmd = body["cmd"]
                    if fail_vm[0] and cmd[0] == "boom":
                        return self._send_json(500, {"error": {"code": "exec_boom", "message": "boom"}})
                    delay = float(cmd[1])
                    if body.get("timeout_s") == 1 and delay > 0.5:
                        time.sleep(0.3)
                    else:
                        time.sleep(delay)
                    out = cmd[2].encode("utf-8")
                    lines = (
                        json.dumps({"stream": "stdout", "data_b64": base64.b64encode(out).decode()}).encode() + b"\n"
                        + json.dumps({"exit": int(cmd[3])}).encode() + b"\n"
                    )
                    self.send_response(200)
                    self.send_header("Content-Type", "application/x-ndjson")
                    self.send_header("Content-Length", str(len(lines)))
                    self.end_headers()
                    self.wfile.write(lines)
                    return
                self._send_json(404, {"error": {"code": "not_found", "message": "nope"}})

            def do_DELETE(self):
                vid = self.path.rsplit("/", 1)[-1]
                with lock:
                    deleted_ids.append(vid)
                    in_flight[0] -= 1
                self._send_json(200, {"ok": True})

        job_srv = _DaemonHTTPServer(("127.0.0.1", 0), JobServer)
        threading.Thread(target=job_srv.serve_forever, daemon=True).start()
        self.addCleanup(lambda: (job_srv.shutdown(), job_srv.server_close()))
        with Boat("http://127.0.0.1:%d" % job_srv.server_address[1], TOKEN) as jb:
            # 1. Input order preserved even when job 0 is slowest; concurrency bounded to 2; timeout gives 124 + cleanup.
            specs = [
                {"cmd": ["work", "0.10", "first", "0"], "size": "large"},
                ["work", "0.01", "second", "3"],
                {"cmd": ["work", "2.0", "slow", "0"], "timeout_s": 0.12},
                {"cmd": ["work", "0.02", "fourth", "0"]},
            ]
            results = jb.run_jobs(specs, concurrency=2)
            self.assertEqual([r.stdout for r in results], [b"first", b"second", b"", b"fourth"])
            self.assertEqual([r.exit_code for r in results], [0, 3, 124, 0])
            self.assertEqual(results[0].size, "large")
            self.assertTrue(all(r.deleted for r in results))
            self.assertLessEqual(peak_in_flight[0], 2)
            self.assertEqual(sorted(created_ids), sorted(deleted_ids))

            # 2. Error in one job still cleans up every started VM before raising.
            created_ids.clear()
            deleted_ids.clear()
            fail_vm[0] = True
            with self.assertRaises(BoatError) as cm:
                jb.run_jobs([
                    {"cmd": ["work", "0.08", "peer", "0"]},
                    {"cmd": ["boom"]},
                    {"cmd": ["work", "0.05", "queued", "0"]},
                ], concurrency=2)
            self.assertEqual(cm.exception.code, "exec_boom")
            self.assertEqual(sorted(created_ids), sorted(deleted_ids))

            # 3. Validation errors fail before creating VMs.
            created_ids.clear()
            with self.assertRaises(ValueError):
                jb.run_jobs([["true"]], concurrency=0)
            with self.assertRaises(BoatTimeoutError):
                jb.run_jobs([{"cmd": ["true"], "timeout_s": 0}])
            self.assertEqual(created_ids, [])

    def test_ssh_port_tokens_team_policies_and_sse_events(self):
        vm = self.b.get_vm("vm1")
        info = vm.grant_ssh("ssh-ed25519 AAAA test")
        self.assertEqual((info.host, info.port), ("10.0.0.5", 22))
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/vms/vm1/ssh"))

        keys = self.b.ssh_keys("vm1")
        self.assertEqual(keys[0]["fingerprint"], "SHA256:abc+def/")

        rev = vm.revoke_ssh("SHA256:abc+def/=")
        self.assertTrue(rev["ok"])
        self.assertEqual(rev["fingerprint"], "SHA256:abc+def/=")
        self.assertEqual(Mock.log[-1][:2], ("DELETE", "/v1/vms/vm1/ssh/SHA256%3Aabc%2Bdef%2F%3D"))

        rev_client = self.b.revoke_ssh("vm/1", "SHA256:xy/z+")
        self.assertTrue(rev_client["ok"])
        self.assertEqual(Mock.log[-1][:2], ("DELETE", "/v1/vms/vm%2F1/ssh/SHA256%3Axy%2Fz%2B"))

        self.assertEqual(vm.revoke_port_tokens()["generation"], 2)
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/vms/vm1/port-tokens/revoke"))
        self.assertEqual(self.b.revoke_port_tokens("vm/1")["generation"], 2)
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/vms/vm%2F1/port-tokens/revoke"))

        before_err_count = len(Mock.log)
        for bad_id in ("", "   ", ".", "..", None):
            with self.assertRaises(ValueError):
                self.b.revoke_port_tokens(bad_id)  # type: ignore[arg-type]
            with self.assertRaises(ValueError):
                self.b.revoke_ssh(bad_id, "SHA256:abc")  # type: ignore[arg-type]
        for bad_fp in ("", "   ", ".", "..", None):
            with self.assertRaises(ValueError):
                self.b.revoke_ssh("vm1", bad_fp)  # type: ignore[arg-type]
            with self.assertRaises(ValueError):
                vm.revoke_ssh(bad_fp)  # type: ignore[arg-type]
        with self.assertRaises(TypeError):
            self.b.revoke_port_tokens()  # type: ignore[call-arg]
        with self.assertRaises(TypeError):
            self.b.revoke_ssh("vm1")  # type: ignore[call-arg]
        with self.assertRaises(TypeError):
            vm.revoke_ssh()  # type: ignore[call-arg]
        self.assertEqual(len(Mock.log), before_err_count)

        pol = self.b.set_self_token_policy("t1", public_ports=True, ssh=True)
        self.assertEqual(pol, {"public_ports": True, "ssh": True})
        self.assertEqual(Mock.log[-1][:2], ("PUT", "/v1/teams/t1/self-token"))

        allow = self.b.set_share_allowlist("t1", ["team_a", "team_b"])
        self.assertEqual(allow, {"teams": ["team_a", "team_b"]})
        self.assertEqual(Mock.log[-1][:2], ("PUT", "/v1/teams/t1/share-allowlist"))

        evs = list(self.b.iter_events(after=10))
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/events?after=10"))
        self.assertEqual([(e["seq"], e["type"], e["vm_id"]) for e in evs], [
            (11, "vm.started", "vm1"),
            (12, "vm.stopped", "vm1"),
        ])

    def test_update_environment_reset_secrets_and_team_snapshots_visibility(self):
        updated = self.b.update_environment("base", reset_secrets=True, setup_script="echo ok")
        self.assertEqual(updated["name"], "base")
        self.assertEqual(Mock.log[-1][:2], ("PUT", "/v1/environments/base"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"reset_secrets": True, "setup_script": "echo ok"})

        def_snaps = self.b.team_snapshots()
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/snapshots"))
        self.assertEqual(def_snaps[0]["visibility"], "default")

        shared_snaps = self.b.team_snapshots(visibility="shared")
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/snapshots?visibility=shared"))
        self.assertEqual(shared_snaps[0]["visibility"], "shared")

        all_snaps = self.b.team_snapshots(visibility="all")
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/snapshots?visibility=all"))
        self.assertEqual(all_snaps[0]["visibility"], "all")

    def test_upgrade_environment(self):
        res = self.b.upgrade_environment("base")
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/environments/base/upgrade"))
        self.assertEqual(Mock.log[-1][2], b"")
        self.assertEqual(res, {
            "environment": "base", "version": 2,
            "upgraded": 3, "upgraded_live": 2, "upgraded_stopped": 1, "failed": 0,
        })

        targeted = self.b.upgrade_environment("env/1", vm_ids=["vm1", "vm2"])
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/environments/env%2F1/upgrade"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"agentIds": ["vm1", "vm2"]})
        self.assertEqual(targeted["upgraded"], 2)

        by_agent = self.b.upgrade_environment("base", agent_ids=("bx_1",))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"agentIds": ["bx_1"]})
        self.assertEqual(by_agent["upgraded"], 1)

        empty = self.b.upgrade_environment("base", vm_ids=[])
        self.assertEqual(json.loads(Mock.log[-1][2]), {"agentIds": []})
        self.assertEqual(empty["upgraded"], 0)

        with self.assertRaises(ValueError):
            self.b.upgrade_environment("base", vm_ids=["vm1"], agent_ids=["vm2"])
        with self.assertRaises(TypeError):
            self.b.upgrade_environment("base", "vm1")

    def test_patch_vm_and_vm_usage(self):
        patched = self.b.patch_vm("vm1", name="patched", ttl_s=120)
        self.assertEqual(Mock.log[-1][:2], ("PATCH", "/v1/vms/vm1"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "patched", "ttl_s": 120})
        self.assertEqual(patched.name, "patched")

        u0 = self.b.vm_usage("vm1")
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/vms/vm1/usage"))
        self.assertEqual(u0["seconds"], 42)

        dt = datetime.datetime(2026, 2, 1, 0, 0, 0, tzinfo=datetime.timezone.utc)
        self.b.vm_usage("vm1", since=1700000000, until=dt)
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/vms/vm1/usage?since=1700000000&until=2026-02-01T00%3A00%3A00Z"))
        with self.assertRaises(TypeError):
            self.b.vm_usage("vm1", since=True)

    def test_run_raises_on_stream_errors_and_missing_exit(self):
        cases = [
            ({"error": "binary not found", "code": "spawn_failed"}, 400, "spawn_failed", "binary not found"),
            ({"error": "no such cwd", "code": "not_found"}, 400, "not_found", "no such cwd"),
            ({"error": "unknown user", "code": "invalid_user"}, 400, "invalid_user", "unknown user"),
            ({"error": "bad arg", "code": "invalid_argument"}, 400, "invalid_argument", "bad arg"),
            ({"error": "denied", "code": "permission_denied"}, 403, "permission_denied", "denied"),
            ({"error": "agent disconnected"}, 502, "exec_error", "agent disconnected"),
            (None, 502, "stream_ended", "without exit status"),
        ]
        state = {"frame": None}

        class ErrExecHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send(self, status, obj=None, raw=None, ctype="application/json"):
                data = raw if raw is not None else json.dumps(obj).encode()
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    self.rfile.read(n)
                if self.path == "/v1/vms":
                    return self._send(201, dict(VM, id="vmerr"))
                if self.path.endswith("/exec"):
                    lines = [{"stream": "stdout", "data_b64": base64.b64encode(b"partial").decode()}]
                    if state["frame"] is not None:
                        lines.append(state["frame"])
                    raw = b"".join(json.dumps(x).encode() + b"\n" for x in lines)
                    return self._send(200, raw=raw, ctype="application/x-ndjson")
                return self._send(404, {"error": {"code": "not_found", "message": "not found"}})

            def do_DELETE(self):
                state.setdefault("deleted", []).append(self.path)
                return self._send(200, {"ok": True})

        srv = _DaemonHTTPServer(("127.0.0.1", 0), ErrExecHandler)
        th = threading.Thread(target=srv.serve_forever, daemon=True)
        th.start()
        try:
            with Boat(f"http://127.0.0.1:{srv.server_port}", TOKEN) as cli:
                vm = VMRecord.from_json(dict(VM, id="vmerr"), client=cli)
                for frame, want_status, want_code, want_msg in cases:
                    state["frame"] = frame
                    for fn in (lambda: cli.run("vmerr", ["nope"]), lambda: vm.run(["nope"])):
                        with self.assertRaises(BoatError) as cm:
                            fn()
                        self.assertEqual(cm.exception.status, want_status)
                        self.assertEqual(cm.exception.code, want_code)
                        self.assertIn(want_msg, cm.exception.message)
                # run_job propagates BoatError and still deletes the VM.
                state["frame"] = {"error": "spawn failed", "code": "spawn_failed"}
                state["deleted"] = []
                with self.assertRaises(BoatError) as cm:
                    cli.run_job(["missing"])
                self.assertEqual((cm.exception.status, cm.exception.code), (400, "spawn_failed"))
                self.assertEqual(state["deleted"], ["/v1/vms/vmerr"])
        finally:
            srv.shutdown()
            srv.server_close()

    def test_run_job_sets_ttl_and_renews_with_patch_before_delete(self):
        events = []

        class HeartbeatHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send(self, status, obj=None, raw=None, ctype="application/json"):
                data = raw if raw is not None else json.dumps(obj).encode()
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n)) if n else {}
                events.append(("POST", self.path, body))
                if self.path == "/v1/vms":
                    return self._send(201, dict(VM, id="vmhb", **body))
                if self.path == "/v1/vms/vmhb/exec":
                    time.sleep(0.18)
                    raw = b'{"stream":"stdout","data_b64":"b2sK"}\n{"exit":0}\n'
                    return self._send(200, raw=raw, ctype="application/x-ndjson")
                return self._send(404, {"error": {"code": "not_found", "message": "not found"}})

            def do_PATCH(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n)) if n else {}
                events.append(("PATCH", self.path, body))
                return self._send(200, dict(VM, id="vmhb", **body))

            def do_DELETE(self):
                events.append(("DELETE", self.path, None))
                return self._send(200, {"ok": True})

        import boat.client as boat_client
        srv = _DaemonHTTPServer(("127.0.0.1", 0), HeartbeatHandler)
        th = threading.Thread(target=srv.serve_forever, daemon=True)
        th.start()
        try:
            with Boat(f"http://127.0.0.1:{srv.server_port}", TOKEN) as cli:
                with patch.object(boat_client, "_JOB_HEARTBEAT_S", 0.05):
                    res = cli.run_job(["sleep", "1"])
                self.assertEqual((res.exit_code, res.deleted), (0, True))
                create_call = next(e for e in events if e[0] == "POST" and e[1] == "/v1/vms")
                self.assertEqual(create_call[2]["ttl_s"], 900)
                self.assertEqual(create_call[2]["idle_stop_s"], 900)
                patches = [e for e in events if e[0] == "PATCH"]
                self.assertGreaterEqual(len(patches), 2)
                for _, path, body in patches:
                    self.assertEqual(path, "/v1/vms/vmhb")
                    self.assertEqual(body, {"ttl_s": 900})
                delete_idx = next(i for i, e in enumerate(events) if e[0] == "DELETE")
                self.assertTrue(all(i < delete_idx for i, e in enumerate(events) if e[0] == "PATCH"))

                # Explicit ttl_s=0 disables heartbeat PATCH calls.
                events.clear()
                with patch.object(boat_client, "_JOB_HEARTBEAT_S", 0.05):
                    cli.run_job(["true"], ttl_s=0)
                self.assertEqual([e for e in events if e[0] == "PATCH"], [])
        finally:
            srv.shutdown()
            srv.server_close()

    def test_create_key_expiry_team_methods_and_boat_api_url(self):
        k = self.b.create_key(name="ci", scopes=["vms:read"], expires_in_s=3600)
        self.assertEqual(Mock.log[-1][:2], ("POST", "/v1/keys"))
        self.assertEqual(json.loads(Mock.log[-1][2]), {"name": "ci", "scopes": ["vms:read"], "expires_in_s": 3600})
        self.assertEqual(k["expires"], "2026-01-02T00:00:00Z")

        team = self.b.get_team("t1")
        self.assertEqual(Mock.log[-1][:2], ("GET", "/v1/teams/t1"))
        self.assertEqual(team["id"], "t1")
        self.assertEqual(team["members"][0]["user_id"], "u1")

        removed = self.b.remove_member("t1", "u/2")
        self.assertEqual(Mock.log[-1][:2], ("DELETE", "/v1/teams/t1/members/u%2F2"))
        self.assertTrue(removed["ok"])

        with patch.dict(os.environ, {"BOAT_URL": "", "BOAT_API_URL": self.url + "/api/v1/", "BOAT_API_KEY": TOKEN}):
            with Boat() as compat_client:
                self.assertEqual(compat_client.url, self.url)
                self.assertEqual(compat_client.get_vm("vm1").id, "vm1")
        with Boat(self.url + "/api/v1", TOKEN) as stripped:
            self.assertEqual(stripped.url, self.url)
        with Boat(self.url + "/v1/", TOKEN) as stripped_v1:
            self.assertEqual(stripped_v1.url, self.url)


if __name__ == "__main__":
    unittest.main()
