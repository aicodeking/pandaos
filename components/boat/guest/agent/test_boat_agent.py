"""Protocol tests for boat-agent over a socketpair (python3 -m unittest)."""
import base64
import importlib.util
import json
import os
import pwd
import socket
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("boat_agent", os.path.join(HERE, "boat-agent.py"))
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)


class Client:
    def __init__(self):
        a, b = socket.socketpair()
        threading.Thread(target=agent.handle_conn, args=(b,), daemon=True).start()
        self.sock, self.r = a, a.makefile("rb")

    def send(self, obj):
        self.sock.sendall((json.dumps(obj) + "\n").encode())

    def recv(self):
        return json.loads(self.r.readline())

    def call(self, obj):
        self.send(obj)
        return self.recv()

    def exec(self, **req):
        self.send(dict(op="exec", **req))
        out, err = b"", b""
        while True:
            m = self.recv()
            if "stream" in m:
                data = base64.b64decode(m["data_b64"])
                if m["stream"] == "stdout":
                    out += data
                else:
                    err += data
            else:
                return out, err, m

    def close(self):
        self.r.close()
        self.sock.close()


class AgentTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        agent.ENV_FILE = os.path.join(self.tmp, "env")
        agent.EXEC_USER = pwd.getpwuid(os.getuid()).pw_name  # no `dev` on test hosts
        self.c = Client()

    def tearDown(self):
        self.c.close()

    def test_health(self):
        m = self.c.call({"op": "health"})
        self.assertTrue(m["ok"])
        self.assertGreaterEqual(m["uptime"], 0)
        self.assertIn("idle_s", m)

    def test_errors_keep_connection(self):
        self.assertIn("error", self.c.call({"op": "nope"}))
        self.c.sock.sendall(b"not json\n")
        self.assertIn("error", self.c.recv())
        self.assertIn("error", self.c.call({"op": "exec", "argv": []}))
        self.assertTrue(self.c.call({"op": "health"})["ok"])

    def test_exec_streams_and_exit(self):
        out, err, end = self.c.exec(argv=["sh", "-c", "echo out; echo err >&2; exit 3"],
                                    timeout_s=10)
        self.assertEqual((out, err, end["exit"]), (b"out\n", b"err\n", 3))

    def test_exec_stdin_env_cwd(self):
        out, _, end = self.c.exec(argv=["sh", "-c", "cat; echo \"$FOO\"; pwd"],
                                  stdin_b64=base64.b64encode(b"in\n").decode(),
                                  env={"FOO": "bar baz"}, cwd=self.tmp, timeout_s=10)
        lines = out.decode().splitlines()
        self.assertEqual(lines[:2], ["in", "bar baz"])
        self.assertEqual(os.path.realpath(lines[2]), os.path.realpath(self.tmp))
        self.assertEqual(end["exit"], 0)

    def test_exec_large_output(self):
        out, _, end = self.c.exec(argv=["sh", "-c", "head -c 3000000 /dev/zero"], timeout_s=20)
        self.assertEqual((len(out), end["exit"]), (3000000, 0))

    def test_exec_timeout_kills_group(self):
        marker = os.path.join(self.tmp, "child-alive")
        t0 = time.monotonic()
        _, _, end = self.c.exec(
            argv=["sh", "-c", "(sleep 3; touch %s) & sleep 30" % marker], timeout_s=0.5)
        self.assertTrue(end.get("timed_out"))
        self.assertIn(end.get("signal"), ("SIGTERM", "SIGKILL"))
        self.assertLess(time.monotonic() - t0, 6)
        time.sleep(3.5)
        self.assertFalse(os.path.exists(marker), "background child survived the group kill")

    def test_exec_tty(self):
        out, _, end = self.c.exec(argv=["sh", "-c", "test -t 1 && echo tty"], tty=True,
                                  timeout_s=10)
        self.assertIn(b"tty", out)
        self.assertEqual(end["exit"], 0)

    def test_set_env_is_used_by_exec(self):
        m = self.c.call({"op": "set_env", "vars": {"A_KEY": "it's \"quoted\" $x"}})
        self.assertEqual(m["count"], 1)
        self.assertEqual(os.stat(agent.ENV_FILE).st_mode & 0o777, 0o600)
        out, _, _ = self.c.exec(argv=["sh", "-c", "printf %s \"$A_KEY\""], timeout_s=10)
        self.assertEqual(out, b"it's \"quoted\" $x")
        # the file is valid shell too (login shells source it)
        out, _, _ = self.c.exec(argv=["sh", "-c", ". %s; printf %%s \"$A_KEY\"" % agent.ENV_FILE],
                                env={"A_KEY": ""}, timeout_s=10)
        self.assertEqual(out, b"it's \"quoted\" $x")
        self.assertIn("error", self.c.call({"op": "set_env", "vars": {"BAD-KEY": "x"}}))

    def test_put_get_file_chunked(self):
        path = os.path.join(self.tmp, "sub", "dir", "f.bin")
        blob = os.urandom(2_500_000)
        parts = [blob[:1_000_000], blob[1_000_000:2_000_000], blob[2_000_000:]]
        off = 0
        for i, p in enumerate(parts):
            m = self.c.call({"op": "put_file", "path": path, "mode": "0750", "offset": off,
                             "last": i == len(parts) - 1,
                             "data_b64": base64.b64encode(p).decode()})
            self.assertTrue(m["ok"], m)
            off += len(p)
            self.assertEqual(os.path.exists(path), i == len(parts) - 1)
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o750)
        self.c.send({"op": "get_file", "path": path})
        got = b""
        while True:
            m = self.c.recv()
            if m.get("eof"):
                break
            self.assertEqual(m["offset"], len(got))
            got += base64.b64decode(m["data_b64"])
        self.assertEqual((got, m["size"], m["mode"]), (blob, len(blob), "750"))

    def test_put_file_rejects_gap_and_relative(self):
        path = os.path.join(self.tmp, "g")
        self.assertIn("error", self.c.call({"op": "put_file", "path": path, "offset": 10,
                                            "last": False, "data_b64": "eA=="}))
        self.assertIn("error", self.c.call({"op": "put_file", "path": "rel", "data_b64": ""}))
        self.assertIn("error", self.c.call({"op": "get_file", "path": path + "missing"}))

    def test_set_identity_validation(self):
        good = {"op": "set_identity", "hostname": "vm-1", "ipv4_cidr": "10.0.0.2/30",
                "gw": "10.0.0.1", "dns": ["1.1.1.1"], "mac": "06:00:0a:00:00:02"}
        for k, v in [("hostname", "bad_host!"), ("ipv4_cidr", "10.0.0.300/30"),
                     ("mac", "zz:00"), ("gw", "nope"), ("ipv6_cidr", "1::/zz")]:
            self.assertIn("error", self.c.call(dict(good, **{k: v})), k)
        self.assertEqual(agent.validate_identity(good)["dns"], ["1.1.1.1"])

    def test_reseed_gates_set_identity(self):
        fed, calls = [], []
        saved = agent.rnd_ioctl, agent.apply_network, agent.regenerate_secrets, agent.IDENTITY_FILE
        agent.rnd_ioctl = fed.append
        agent.apply_network = lambda ident: calls.append("net")
        agent.regenerate_secrets = lambda: calls.append("keys")
        agent.IDENTITY_FILE = os.path.join(self.tmp, "identity.json")
        agent._reseed.update(armed=False, generation=None)
        ident = {"op": "set_identity", "hostname": "vm-1", "ipv4_cidr": "10.0.0.2/30",
                 "gw": "10.0.0.1", "dns": ["1.1.1.1"]}
        try:
            self.assertIn("reseed required", self.c.call(ident)["error"])
            self.assertEqual(calls, [])
            self.assertIn("error", self.c.call({"op": "reseed", "entropy_b64": "eA=="}))  # too short
            m = self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x01" * 64).decode()})
            self.assertEqual((m, fed), ({"ok": True, "bytes": 64}, [b"\x01" * 64]))
            # a bad request must not use up the reseed
            self.assertIn("error", self.c.call(dict(ident, hostname="bad!")))
            self.assertEqual(self.c.call(ident), {"ok": True})
            self.assertEqual(calls, ["net", "keys"])
            self.assertIn("reseed required", self.c.call(ident)["error"])  # used up
            # generation must match when given
            self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x02" * 32).decode(),
                         "generation": 7})
            self.assertIn("generation", self.c.call(dict(ident, generation=6))["error"])
            self.assertEqual(self.c.call(dict(ident, generation=7)), {"ok": True})
        finally:
            agent.rnd_ioctl, agent.apply_network, agent.regenerate_secrets, agent.IDENTITY_FILE = saved

    def test_reseed_ioctl_error_is_reported(self):
        saved = agent.RANDOM_DEV
        agent.RANDOM_DEV = os.path.join(self.tmp, "not-a-random-device")
        open(agent.RANDOM_DEV, "w").close()
        try:
            m = self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x00" * 32).decode()})
            self.assertIn("reseed:", m["error"])  # ENOTTY on a regular file
        finally:
            agent.RANDOM_DEV = saved

    def test_sync(self):
        self.assertEqual(self.c.call({"op": "sync"}), {"ok": True})

    def test_desktop_bad_action(self):
        self.assertIn("error", self.c.call({"op": "desktop", "action": "dance"}))


class DesktopProxyTest(unittest.TestCase):
    def test_check_token(self):
        tok = "t" * 32
        ok, loc = agent.check_token(b"GET /?token=%s HTTP/1.1\r\n\r\n" % tok.encode(), tok)
        self.assertEqual((ok, loc), (True, "./"))
        ok, loc = agent.check_token(
            b"GET /p/index.html?a=1&token=%s HTTP/1.1\r\n\r\n" % tok.encode(), tok)
        self.assertEqual((ok, loc), (True, "index.html?a=1"))
        ok, loc = agent.check_token(
            b"GET /websockify HTTP/1.1\r\nCookie: x=1; boat_desktop=%s\r\n\r\n" % tok.encode(), tok)
        self.assertEqual((ok, loc), (True, None))
        self.assertFalse(agent.check_token(b"GET /?token=wrong HTTP/1.1\r\n\r\n", tok)[0])
        self.assertFalse(agent.check_token(b"GET / HTTP/1.1\r\n\r\n", None)[0])

    def test_proxy_end_to_end(self):
        backend = socket.socket()
        backend.bind(("127.0.0.1", 0))
        backend.listen(1)

        def serve():
            c, _ = backend.accept()
            with c:
                c.recv(65536)
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
        threading.Thread(target=serve, daemon=True).start()
        srv = agent.start_proxy(port=0, backend_addr=backend.getsockname(), host="127.0.0.1")
        port = srv.getsockname()[1]
        agent._desktop["token"] = tok = "k" * 32
        try:
            def get(req):
                s = socket.create_connection(("127.0.0.1", port), timeout=5)
                with s:
                    s.sendall(req)
                    data = b""
                    while True:
                        chunk = s.recv(65536)
                        if not chunk:
                            return data
                        data += chunk
            self.assertIn(b"403", get(b"GET / HTTP/1.1\r\n\r\n").split(b"\r\n")[0])
            r = get(b"GET /?token=%s HTTP/1.1\r\n\r\n" % tok.encode())
            self.assertIn(b"302", r.split(b"\r\n")[0])
            self.assertIn(b"Set-Cookie: boat_desktop=" + tok.encode(), r)
            r = get(b"GET / HTTP/1.1\r\nCookie: boat_desktop=%s\r\n\r\n" % tok.encode())
            self.assertTrue(r.endswith(b"hi"), r)
        finally:
            agent._desktop["token"] = None
            srv.close()


def fake_kasmvnc(listener, frames_per_s=50):
    """A KasmVNC stand-in: serves an html page and RFB (security None) over a websocket."""
    import hashlib
    import struct

    def ws_send(c, payload):
        n = len(payload)
        hdr = bytes([0x82]) + (bytes([n]) if n < 126 else bytes([126]) + struct.pack(">H", n)
                               if n < 65536 else bytes([127]) + struct.pack(">Q", n))
        c.sendall(hdr + payload)

    def ws_recv(c, buf):
        while True:
            if len(buf) >= 6:
                n = buf[1] & 0x7F
                if len(buf) >= 6 + n:
                    mask, data = buf[2:6], buf[6:6 + n]
                    del buf[:6 + n]
                    return bytes(b ^ mask[i % 4] for i, b in enumerate(data))
            chunk = c.recv(65536)
            if not chunk:
                raise EOFError
            buf += chunk

    def serve(c):
        with c:
            head = b""
            while b"\r\n\r\n" not in head:
                head += c.recv(4096)
            if b"Upgrade: websocket" not in head:
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 13\r\n\r\n<html></html>")
                return
            key = [l.split(b":", 1)[1].strip() for l in head.split(b"\r\n")
                   if l.lower().startswith(b"sec-websocket-key")][0]
            acc = base64.b64encode(hashlib.sha1(key + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
                                   .digest()).decode()
            c.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                       "Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n" % acc).encode())
            buf = bytearray()
            try:
                ws_send(c, b"RFB 003.008\n")
                ws_recv(c, buf)
                ws_send(c, b"\x01\x01")
                ws_recv(c, buf)
                ws_send(c, struct.pack(">I", 0))
                ws_recv(c, buf)                                    # ClientInit
                ws_send(c, struct.pack(">HH16sI", 64, 32, b"\0" * 16, 4) + b"fake")
                while True:
                    msg = ws_recv(c, buf)
                    if msg[0] == 3:                                # FramebufferUpdateRequest
                        inc = msg[1]
                        w, h = (8, 8) if inc else (64, 32)
                        if inc:
                            time.sleep(1.0 / frames_per_s)
                        ws_send(c, struct.pack(">BxHHHHHi", 0, 1, 0, 0, w, h, 0) + b"\x07" * (w * h))
            except (EOFError, OSError):
                pass

    def loop():
        while True:
            try:
                c, _ = listener.accept()
            except OSError:
                return
            threading.Thread(target=serve, args=(c,), daemon=True).start()
    threading.Thread(target=loop, daemon=True).start()


class DesktopCheckTest(unittest.TestCase):
    """desktop-check.sh through the agent's real token proxy to a fake KasmVNC."""

    def test_desktop_check_end_to_end(self):
        import shutil
        import subprocess
        if not shutil.which("curl"):
            self.skipTest("curl not installed")
        backend = socket.socket()
        backend.bind(("127.0.0.1", 0))
        backend.listen(8)
        fake_kasmvnc(backend)
        srv = agent.start_proxy(port=0, backend_addr=backend.getsockname(), host="127.0.0.1")
        agent._desktop["token"] = tok = "d" * 32
        try:
            link = "http://127.0.0.1:%d/?token=%s" % (srv.getsockname()[1], tok)
            r = subprocess.run(["bash", os.path.join(HERE, "..", "desktop-check.sh"), link, "1"],
                               capture_output=True, text=True, timeout=60)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIn("METRIC desktop_first_frame_ms=", r.stdout)
            fps = float(r.stdout.split("METRIC desktop_fps=")[1].split()[0])
            self.assertGreater(fps, 10, r.stdout)
            bad = subprocess.run(["bash", os.path.join(HERE, "..", "desktop-check.sh"),
                                  link.replace(tok, "x" * 32), "1"],
                                 capture_output=True, text=True, timeout=60)
            self.assertNotEqual(bad.returncode, 0)
        finally:
            agent._desktop["token"] = None
            srv.close()
            backend.close()


if __name__ == "__main__":
    unittest.main()
