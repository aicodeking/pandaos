import base64
import hashlib
import hmac
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from boat import Boat, BoatError, verify_webhook  # noqa: E402

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
        if (m, p) == ("POST", "/v1/vms"):
            return self._send(201, dict(VM, **json.loads(body)))
        if (m, p) == ("GET", "/v1/vms"):
            return self._send(200, {"vms": [VM]})
        if (m, p) == ("POST", "/v1/vms/vm1/fork"):
            return self._send(201, dict(VM, id="vm2", name=json.loads(body).get("name", "")))
        if (m, p) == ("POST", "/v1/vms/vm1/stop"):
            return self._send(200, dict(VM, state="stopped"))
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
