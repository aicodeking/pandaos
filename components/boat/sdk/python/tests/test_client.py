import base64
import hashlib
import hmac
import io
import json
import os
import shutil
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from boat import Boat, BoatError, VM as VMRecord, verify_webhook  # noqa: E402

TOKEN = "boat_test"
VM = {"id": "vm1", "name": "dev", "size": "small", "state": "running", "image": "ubuntu-24.04", "team": "t1"}


class Mock(BaseHTTPRequestHandler):
    log = []
    files = {}

    def log_message(self, *a):
        pass

    def _send(self, code, obj=None, raw=None, ctype="application/json"):
        body = raw if raw is not None else json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _handle(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        Mock.log.append((self.command, self.path, body))
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
            return self._send(201, {"vms": [dict(VM, id="vm%d" % i, name="%s-%d" % (req["name"], i))
                                            for i in range(1, req["count"] + 1)]})
        if (m, p) == ("POST", "/v1/vms"):
            return self._send(201, dict(VM, **json.loads(body)))
        if (m, p) == ("GET", "/v1/vms"):
            return self._send(200, {"vms": [VM]})
        if (m, p) == ("POST", "/v1/vms/vm1/fork"):
            return self._send(201, dict(VM, id="vm2", name=json.loads(body).get("name", "")))
        if (m, p) == ("POST", "/v1/vms/vm1/stop"):
            return self._send(200, dict(VM, state="stopped"))
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
                Mock.files[p] = body
                return self._send(200, {"ok": True})
            return self._send(200, raw=Mock.files.get(p, b""), ctype="application/octet-stream")
        if (m, p) == ("POST", "/v1/vms/vm1/ssh"):
            return self._send(200, {"host": "10.0.0.5", "port": 22, "user": "ubuntu"})
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

    do_GET = do_POST = do_PUT = do_DELETE = _handle


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), Mock)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = "http://127.0.0.1:%d" % cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        Mock.log.clear()
        self.b = Boat(self.url, TOKEN)

    def test_open_timeout_selection(self):
        self.b.timeout = 1.5
        for kwargs, expected in (({}, 1.5), ({"timeout": 0.75}, 0.75),
                                 ({"timeout": 0}, 0), ({"timeout": None}, None)):
            with self.subTest(kwargs=kwargs), patch("boat.client.urllib.request.urlopen",
                                                   return_value=io.BytesIO()) as transport:
                with self.b._open("GET", "/v1/me", **kwargs):
                    pass
                self.assertEqual(transport.call_args.kwargs["timeout"], expected)

    def test_streaming_paths_disable_transport_timeout(self):
        calls = (
            ("exec", lambda: list(self.b.exec("vm1", ["true"]))),
            ("prompt", lambda: list(self.b.prompt("vm1", "go"))),
            ("follow_command", lambda: list(self.b.follow_command("vm1", "c_1"))),
            ("follow_prompt", lambda: list(self.b.follow_prompt("vm1", "prm_1"))),
            ("download_snapshot", lambda: self.b.download_snapshot("vm1", "s1", io.BytesIO())),
            ("import_snapshot", lambda: self.b.import_snapshot(b"ARCHIVE")),
        )
        for name, call in calls:
            with self.subTest(name=name), patch("boat.client.urllib.request.urlopen",
                                               return_value=io.BytesIO(b'{"exit":0}\n')) as transport:
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


if __name__ == "__main__":
    unittest.main()
