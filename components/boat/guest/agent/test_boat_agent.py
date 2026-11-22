"""Protocol tests for boat-agent over a socketpair (python3 -m unittest)."""
import base64
import importlib.util
import importlib.machinery
import json
import os
import pwd
import socket
import tempfile
import threading
import time
import unittest
from unittest import mock
from pathlib import Path

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("boat_agent", os.path.join(HERE, "boat-agent.py"))
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)

# ci runs this module only, so it pulls in the other agent test classes.
from test_boat_ops import CmdTest, FileTest, SearchTest  # noqa: E402,F401


def types_ns(**kw):
    import types
    return types.SimpleNamespace(**kw)


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
        return self.exec_reply()

    def exec_reply(self):
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

    def test_exec_session_survives_a_dropped_connection(self):
        # A vsock transport reset (every Firecracker snapshot) drops the guest's side.
        sid = "s-%d" % time.monotonic_ns()
        self.c.send({"op": "exec", "session": sid, "timeout_s": 20,
                     "argv": ["sh", "-c", "echo begin; sleep 1; echo end; exit 7"]})
        first = self.c.recv()
        self.assertEqual(base64.b64decode(first["data_b64"]), b"begin\n")
        self.c.close()
        time.sleep(1.5)  # the exit is sent to the dead connection and parked
        c2 = Client()
        try:
            self.assertEqual(c2.call({"op": "exec_attach", "session": sid, "seen": 1}), {"attached": sid})
            out, _, end = c2.exec_reply()
            self.assertEqual((out, end["exit"]), (b"end\n", 7))
            c2.call({"op": "exec_attach", "session": sid, "seen": 0})  # a full replay, while it lingers
            out, _, end = c2.exec_reply()
            self.assertEqual((out, end["exit"]), (b"begin\nend\n", 7))
        finally:
            c2.close()
        self.c = Client()

    def test_exec_attach_takes_over_a_running_session(self):
        sid = "t-%d" % time.monotonic_ns()
        self.c.send({"op": "exec", "session": sid, "timeout_s": 20,
                     "argv": ["sh", "-c", "echo begin; sleep 1.5; echo end"]})
        self.c.recv()
        c2 = Client()  # the old connection is still open: the attach takes the stream anyway
        try:
            self.assertEqual(c2.call({"op": "exec_attach", "session": sid, "seen": 1}), {"attached": sid})
            out, _, end = c2.exec_reply()
            self.assertEqual((out, end["exit"]), (b"end\n", 0))
        finally:
            c2.close()

    def test_exec_attach_starts_a_lost_exec_once(self):
        sid = "v-%d" % time.monotonic_ns()
        req = {"op": "exec_attach", "session": sid, "seen": 0, "exec": {"argv": ["sh", "-c", "echo once"]}}
        self.assertEqual(self.c.call(req), {"attached": sid, "started": True})
        out, _, end = self.c.exec_reply()
        self.assertEqual((out, end["exit"]), (b"once\n", 0))
        self.assertEqual(self.c.call(req), {"attached": sid})  # now a replay, not a second run
        out, _, end = self.c.exec_reply()
        self.assertEqual((out, end["exit"]), (b"once\n", 0))

    def test_exec_session_spawn_error_is_final(self):
        sid = "w-%d" % time.monotonic_ns()
        self.assertIn("spawn", self.c.call({"op": "exec", "session": sid, "argv": ["/nonexistent/x"]})["error"])
        self.assertEqual(self.c.call({"op": "exec_attach", "session": sid, "seen": 0}), {"attached": sid})
        self.assertIn("spawn", self.c.recv()["error"])

    def test_exec_attach_errors(self):
        self.assertIn("no such", self.c.call({"op": "exec_attach", "session": "nope", "seen": 0})["error"])
        self.assertIn("bad session", self.c.call({"op": "exec", "session": "a b", "argv": ["true"]})["error"])
        sid = "u-%d" % time.monotonic_ns()
        _, _, end = self.c.exec(argv=["true"], session=sid)
        self.assertEqual(end["exit"], 0)
        self.assertIn("seen", self.c.call({"op": "exec_attach", "session": sid, "seen": -1})["error"])
        self.assertEqual(self.c.call({"op": "exec_attach", "session": sid, "seen": 5}), {"attached": sid})
        self.assertIn("lost", self.c.recv()["error"])
        self.assertIn("exists", self.c.call({"op": "exec", "session": sid, "argv": ["true"]})["error"])

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

    def test_exit_reply_bounded_for_an_unkillable_process(self):
        class Stuck:  # SIGKILL sent, still in D state
            pid = 4242

            def wait(self, timeout=None):
                if timeout is None:
                    raise AssertionError("unbounded wait after a timeout")
                raise agent.subprocess.TimeoutExpired("x", timeout)
        res = agent.exit_reply(Stuck(), True)
        self.assertTrue(res["timed_out"])
        self.assertEqual(res["exit"], 137)
        self.assertIn("did not exit after SIGKILL", res["error"])

    def test_exec_background_child_survives_writing_after_exit(self):
        # `server &` must not die of SIGPIPE on its first log line after exec returns.
        marker = os.path.join(self.tmp, "alive")
        t0 = time.monotonic()
        _, _, end = self.c.exec(
            argv=["sh", "-c", "(sleep 2; echo log; echo log >&2; touch %s) & exit 0" % marker],
            timeout_s=10)
        self.assertEqual(end["exit"], 0)
        self.assertLess(time.monotonic() - t0, 2, "exec must not wait for the background child")
        time.sleep(2.5)
        self.assertTrue(os.path.exists(marker), "background child died writing to a closed pipe")

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

    def test_get_env_roundtrip_preserves_values(self):
        values = {"BOAT_ID": "vm-own", "BOAT_API_KEY": "user-key",
                  "QUOTED": "it's \"quoted\" $HOME # literal",
                  "MULTILINE": "first line\nsecond line", "EMPTY": ""}
        self.assertTrue(self.c.call({"op": "set_env", "vars": values})["ok"])
        self.assertEqual(self.c.call({"op": "get_env"}).get("vars"), values)
        out, _, end = self.c.exec(argv=["sh", "-c", 'printf %s "$MULTILINE"'], timeout_s=10)
        self.assertEqual((out, end["exit"]), (b"first line\nsecond line", 0))

    def test_set_env_preserves_compatibility_link_and_replaces_runtime_file(self):
        compat = Path(self.tmp) / "etc-env"
        compat.symlink_to(agent.ENV_FILE)
        self.assertTrue(self.c.call({"op": "set_env", "vars": {"VALUE": "old"}})["ok"])
        with open(agent.ENV_FILE) as old:
            self.assertTrue(self.c.call({"op": "set_env", "vars": {"VALUE": "new"}})["ok"])
            self.assertIn("old", old.read())
        self.assertTrue(compat.is_symlink())
        self.assertIn("new", compat.read_text())
        self.assertEqual(os.stat(agent.ENV_FILE).st_uid, os.getuid())
        self.assertEqual(os.stat(agent.ENV_FILE).st_mode & 0o777, 0o600)

    def test_failed_env_replace_keeps_previous_values_and_removes_partial(self):
        self.assertTrue(self.c.call({"op": "set_env", "vars": {"VALUE": "old"}})["ok"])

        def failed_replace(src, dst):
            self.assertEqual(dst, agent.ENV_FILE)
            self.assertEqual(os.stat(src).st_mode & 0o777, 0o600)
            self.assertEqual(os.stat(src).st_uid, os.getuid())
            raise OSError("replacement unavailable")

        with mock.patch.object(agent.os, "replace", side_effect=failed_replace):
            self.assertIn("error", self.c.call({"op": "set_env", "vars": {"VALUE": "new"}}))
        self.assertEqual(agent.read_env_file(), {"VALUE": "old"})
        self.assertFalse(any(name.startswith(".env-") for name in os.listdir(self.tmp)))

    def test_get_env_refuses_missing_or_unreadable_file(self):
        self.assertIn("error", self.c.call({"op": "get_env"}))
        os.mkdir(agent.ENV_FILE)
        self.assertIn("error", self.c.call({"op": "get_env"}))
        os.rmdir(agent.ENV_FILE)
        with open(agent.ENV_FILE, "w") as f:
            f.write("export SECRET='unterminated")
        self.assertIn("error", self.c.call({"op": "get_env"}))

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
            m = self.c.call(ident)
            self.assertTrue(m["ok"], m)
            self.assertEqual(sorted(m["ms"]), ["network", "secrets"])
            self.assertEqual(calls, ["net", "keys"])
            self.assertIn("reseed required", self.c.call(ident)["error"])  # used up
            # generation must match when given
            self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x02" * 32).decode(),
                         "generation": 7})
            self.assertIn("generation", self.c.call(dict(ident, generation=6))["error"])
            self.assertTrue(self.c.call(dict(ident, generation=7))["ok"])
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

    def test_host_keys_regenerated_without_rsa(self):
        d = os.path.join(self.tmp, "ssh")
        os.mkdir(d)
        for name in ("ssh_host_rsa_key", "ssh_host_rsa_key.pub", "ssh_host_ed25519_key", "sshd_config"):
            with open(os.path.join(d, name), "w") as f:
                f.write("old")
        agent.regenerate_host_keys(d)
        self.assertEqual(sorted(os.listdir(d)),
                         ["ssh_host_ecdsa_key", "ssh_host_ecdsa_key.pub", "ssh_host_ed25519_key",
                          "ssh_host_ed25519_key.pub", "sshd_config"])
        with open(os.path.join(d, "ssh_host_ed25519_key")) as f:
            self.assertIn("OPENSSH PRIVATE KEY", f.read())
        with open(os.path.join(d, "sshd_config")) as f:
            self.assertEqual(f.read(), "old")

    def test_regenerate_secrets_never_uses_async_no_block_restart(self):
        from unittest import mock
        machine_id = os.path.join(self.tmp, "machine-id")
        ssh_dir = os.path.join(self.tmp, "ssh")
        os.mkdir(ssh_dir)
        sshd_cfg = os.path.join(ssh_dir, "sshd_config")
        auth_cfg = os.path.join(self.tmp, "ssh-auth.json")
        auth_alt = os.path.join(self.tmp, "ssh-authorize.json")

        real_write_atomic = agent.write_atomic

        def redirected_write_atomic(path, text, mode):
            real_write_atomic(machine_id if path == "/etc/machine-id" else path, text, mode)

        # 1. Fresh boot before ssh.service is active (inactive, not failed): no restart raced with ssh-authorize.
        calls = []

        def fake_run_inactive(argv, check=True):
            calls.append(list(argv))
            return types_ns(returncode=3, stdout="", stderr="")

        with mock.patch.object(agent, "write_atomic", side_effect=redirected_write_atomic), \
             mock.patch.object(agent, "regenerate_host_keys") as regen_keys, \
             mock.patch.object(agent, "SSH_AUTH_CONFIGS", (auth_cfg, auth_alt)), \
             mock.patch.object(agent, "SSHD_CONFIG_FILE", sshd_cfg), \
             mock.patch.object(agent, "run", side_effect=fake_run_inactive):
            agent.regenerate_secrets()
        regen_keys.assert_called_once_with("/etc/ssh")
        self.assertFalse(any("--no-block" in c for c in calls), calls)
        self.assertNotIn(["systemctl", "restart", "ssh.service"], calls)

        # 2. Managed SSH policy present & ssh.service inactive/failed: leave startup to boat-ssh-authorize.
        with open(auth_alt, "w") as f:
            f.write("{}")
        calls.clear()

        def fake_run_managed_failed(argv, check=True):
            calls.append(list(argv))
            rc = 0 if "is-failed" in argv else 3
            return types_ns(returncode=rc, stdout="", stderr="")

        with mock.patch.object(agent, "write_atomic", side_effect=redirected_write_atomic), \
             mock.patch.object(agent, "regenerate_host_keys"), \
             mock.patch.object(agent, "SSH_AUTH_CONFIGS", (auth_cfg, auth_alt)), \
             mock.patch.object(agent, "SSHD_CONFIG_FILE", sshd_cfg), \
             mock.patch.object(agent, "run", side_effect=fake_run_managed_failed):
            agent.regenerate_secrets()
        self.assertEqual(calls, [["systemctl", "is-active", "--quiet", "ssh.service"]])

        # 3. Managed sshd_config header also detected when json file is absent.
        os.unlink(auth_alt)
        with open(sshd_cfg, "w") as f:
            f.write("# Managed online authority; tenant keys require explicit enrollment.\n")
        calls.clear()
        with mock.patch.object(agent, "write_atomic", side_effect=redirected_write_atomic), \
             mock.patch.object(agent, "regenerate_host_keys"), \
             mock.patch.object(agent, "SSH_AUTH_CONFIGS", (auth_cfg, auth_alt)), \
             mock.patch.object(agent, "SSHD_CONFIG_FILE", sshd_cfg), \
             mock.patch.object(agent, "run", side_effect=fake_run_managed_failed):
            agent.regenerate_secrets()
        self.assertEqual(calls, [["systemctl", "is-active", "--quiet", "ssh.service"]])

        # 4. Active ssh.service restarts synchronously (never --no-block) so new host keys load before reply.
        calls.clear()

        def fake_run_active(argv, check=True):
            calls.append(list(argv))
            return types_ns(returncode=0, stdout="", stderr="")

        with mock.patch.object(agent, "write_atomic", side_effect=redirected_write_atomic), \
             mock.patch.object(agent, "regenerate_host_keys"), \
             mock.patch.object(agent, "SSH_AUTH_CONFIGS", (auth_cfg, auth_alt)), \
             mock.patch.object(agent, "SSHD_CONFIG_FILE", sshd_cfg), \
             mock.patch.object(agent, "run", side_effect=fake_run_active):
            agent.regenerate_secrets()
        self.assertEqual(calls, [
            ["systemctl", "is-active", "--quiet", "ssh.service"],
            ["systemctl", "restart", "ssh.service"],
        ])

        # 5. defer_ssh_restart=True writes machine-id + host keys without invoking systemctl.
        calls.clear()
        with mock.patch.object(agent, "write_atomic", side_effect=redirected_write_atomic), \
             mock.patch.object(agent, "regenerate_host_keys") as regen_keys2, \
             mock.patch.object(agent, "SSH_AUTH_CONFIGS", (auth_cfg, auth_alt)), \
             mock.patch.object(agent, "SSHD_CONFIG_FILE", sshd_cfg), \
             mock.patch.object(agent, "run", side_effect=fake_run_active):
            agent.regenerate_secrets(defer_ssh_restart=True)
        regen_keys2.assert_called_once_with("/etc/ssh")
        self.assertEqual(calls, [])

    def test_set_identity_defer_ssh_restart_option(self):
        fed, calls = [], []
        saved = agent.rnd_ioctl, agent.apply_network, agent.regenerate_secrets, agent.IDENTITY_FILE
        agent.rnd_ioctl = fed.append
        agent.apply_network = lambda ident: calls.append(("net", sorted(ident.keys())))

        def fake_regen(defer_ssh_restart=False):
            calls.append(("keys", defer_ssh_restart))

        agent.regenerate_secrets = fake_regen
        agent.IDENTITY_FILE = os.path.join(self.tmp, "identity.json")
        agent._reseed.update(armed=False, generation=None)
        ident = {"op": "set_identity", "hostname": "vm-defer", "ipv4_cidr": "10.0.0.2/30",
                 "gw": "10.0.0.1", "dns": ["1.1.1.1"]}
        try:
            # Non-boolean defer_ssh_restart fails validation without consuming reseed.
            self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x03" * 32).decode()})
            for bad in ("true", 1, []):
                self.assertIn("boolean", self.c.call(dict(ident, defer_ssh_restart=bad))["error"])
                self.assertIn("boolean", self.c.call(dict(ident, skip_ssh_restart=bad))["error"])
            self.assertEqual(calls, [])

            # defer_ssh_restart=True skips ssh restart and keeps identity.json clean.
            m = self.c.call(dict(ident, defer_ssh_restart=True))
            self.assertTrue(m["ok"], m)
            self.assertEqual(calls[-1], ("keys", True))
            with open(agent.IDENTITY_FILE) as f:
                saved_ident = json.load(f)
            self.assertNotIn("defer_ssh_restart", saved_ident)

            # Default (omitted or False) preserves synchronous ssh restart behavior.
            self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x04" * 32).decode()})
            self.assertTrue(self.c.call(dict(ident, defer_ssh_restart=False))["ok"])
            self.assertEqual(calls[-1], ("keys", False))

            # skip_ssh_restart=True alias also defers ssh restart.
            self.c.call({"op": "reseed", "entropy_b64": base64.b64encode(b"\x05" * 32).decode()})
            self.assertTrue(self.c.call(dict(ident, skip_ssh_restart=True))["ok"])
            self.assertEqual(calls[-1], ("keys", True))
        finally:
            agent.rnd_ioctl, agent.apply_network, agent.regenerate_secrets, agent.IDENTITY_FILE = saved

    def test_apply_network_batches_ip_commands(self):
        calls = []
        written = {}

        def fake_run(argv, check=True, input=None):
            calls.append((list(argv), check, input))
            return types_ns(returncode=0, stdout="", stderr="")

        def fake_write(path, text, mode):
            written[path] = (text, mode)

        ident = {
            "hostname": "vm-batch",
            "ipv4_cidr": "10.200.0.2/30",
            "gw": "10.200.0.1",
            "mac": "06:00:0a:c8:00:02",
            "ipv6_cidr": "2001:db8::2/64",
            "ipv6_gw": "2001:db8::1",
            "dns": ["1.1.1.1", "9.9.9.9"],
        }
        with mock.patch.object(agent, "IFACE", "eth0"), \
             mock.patch.object(agent, "run", side_effect=fake_run), \
             mock.patch.object(agent, "write_atomic", side_effect=fake_write):
            agent.apply_network(ident)

        self.assertEqual(len(calls), 2, calls)
        self.assertEqual(calls[0][0], ["ip", "-force", "-batch", "-"])
        self.assertIs(calls[0][1], False)
        self.assertEqual(calls[0][2], "\n".join([
            "link set eth0 down",
            "link set eth0 address 06:00:0a:c8:00:02",
            "link set eth0 up",
            "addr flush dev eth0 to 0.0.0.0/0",
            "addr add 10.200.0.2/30 dev eth0",
            "route replace default via 10.200.0.1 dev eth0",
            "addr flush dev eth0 to ::/0 scope global",
            "addr add 2001:db8::2/64 dev eth0",
            "route replace ::/0 via 2001:db8::1 dev eth0",
        ]) + "\n")
        self.assertEqual(calls[1], (["hostname", "vm-batch"], True, None))
        self.assertEqual(written["/etc/resolv.conf"], ("nameserver 1.1.1.1\nnameserver 9.9.9.9\n", 0o644))
        self.assertEqual(written["/etc/hostname"], ("vm-batch\n", 0o644))
        self.assertIn("127.0.1.1 vm-batch\n", written["/etc/hosts"][0])

    def test_apply_network_ipv4_only_and_sequential_fallback(self):
        calls = []

        def fake_run(argv, check=True, input=None):
            calls.append((list(argv), check, input))
            if argv[:3] == ["ip", "-force", "-batch"]:
                return types_ns(returncode=1, stdout="", stderr="batch unsupported")
            return types_ns(returncode=0, stdout="", stderr="")

        ident = {
            "hostname": "vm-fallback",
            "ipv4_cidr": "10.200.0.6/30",
            "gw": "10.200.0.5",
            "mac": None,
            "ipv6_cidr": None,
            "ipv6_gw": None,
            "dns": [],
        }
        with mock.patch.object(agent, "IFACE", "eth0"), \
             mock.patch.object(agent, "run", side_effect=fake_run), \
             mock.patch.object(agent, "write_atomic"):
            agent.apply_network(ident)

        self.assertEqual(calls[0][0], ["ip", "-force", "-batch", "-"])
        self.assertEqual(calls[0][2], "\n".join([
            "link set eth0 up",
            "addr flush dev eth0 to 0.0.0.0/0",
            "addr add 10.200.0.6/30 dev eth0",
            "route replace default via 10.200.0.5 dev eth0",
        ]) + "\n")
        self.assertEqual([c[0] for c in calls[1:]], [
            ["ip", "link", "set", "eth0", "up"],
            ["ip", "-4", "addr", "flush", "dev", "eth0"],
            ["ip", "addr", "add", "10.200.0.6/30", "dev", "eth0"],
            ["ip", "route", "replace", "default", "via", "10.200.0.5", "dev", "eth0"],
            ["hostname", "vm-fallback"],
        ])

    def test_sync(self):
        self.assertEqual(self.c.call({"op": "sync"}), {"ok": True})

    def test_set_clock_validates(self):
        self.assertIn("error", self.c.call({"op": "set_clock", "unix_ns": "soon"}))
        self.assertIn("error", self.c.call({"op": "set_clock", "unix_ns": True}))
        saved = agent.time.clock_settime_ns
        seen = []
        agent.time.clock_settime_ns = lambda clk, ns: seen.append(ns)
        try:
            self.assertEqual(self.c.call({"op": "set_clock", "unix_ns": 5}), {"ok": True})
        finally:
            agent.time.clock_settime_ns = saved
        self.assertEqual(seen, [5])

    def test_desktop_start_failure_explains_itself(self):
        saved = agent.run, agent.wait_port, agent.os.path.exists
        logs = types_ns(stdout="Xvnc did not start\n")
        agent.run = lambda argv, check=True: logs
        agent.wait_port = lambda addr, timeout: False
        agent.os.path.exists = lambda p: True
        try:
            err = self.c.call({"op": "desktop", "action": "start"})["error"]
        finally:
            agent.run, agent.wait_port, agent.os.path.exists = saved
        self.assertIn("did not come up", err)
        self.assertIn("Xvnc did not start", err)

    def test_desktop_bad_action(self):
        self.assertIn("error", self.c.call({"op": "desktop", "action": "dance"}))


class AgentOpsWiringTest(unittest.TestCase):
    """The cmd_*/file_* ops through the real protocol loop, env and all."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.saved = agent.ENV_FILE, agent.EXEC_USER, agent.boat_ops.CMD_ROOT
        agent.ENV_FILE = os.path.join(self.tmp, "env")
        agent.EXEC_USER = pwd.getpwuid(os.getuid()).pw_name
        agent.boat_ops.CMD_ROOT = os.path.join(self.tmp, "cmds")
        self.c = Client()

    def tearDown(self):
        self.c.close()
        agent.ENV_FILE, agent.EXEC_USER, agent.boat_ops.CMD_ROOT = self.saved

    def test_cmd_start_applies_set_env_and_request_env(self):
        self.assertTrue(self.c.call({"op": "set_env", "vars": {"FROM_SET_ENV": "yes"}})["ok"])
        m = self.c.call({"op": "cmd_start", "env": {"FROM_REQ": "too"}, "cwd": self.tmp,
                         "argv": ["sh", "-c", 'printf "%s %s" "$FROM_SET_ENV" "$FROM_REQ"']})
        self.assertTrue(m["ok"], m)
        cid = m["cmd_id"]
        for _ in range(200):
            logs = self.c.call({"op": "cmd_logs", "cmd_id": cid})
            if logs["size"] >= 7 and not logs["running"]:
                break
            time.sleep(0.02)
        self.assertEqual(base64.b64decode(logs["data_b64"]), b"yes too")
        st = self.c.call({"op": "cmd_status", "cmd_id": cid})
        self.assertEqual((st["ok"], st["state"], st["exit"]), (True, "exited", 0))
        self.assertEqual([c["cmd_id"] for c in self.c.call({"op": "cmd_list"})["cmds"]], [cid])

    def test_cmd_errors_are_replies(self):
        m = self.c.call({"op": "cmd_start", "argv": []})
        self.assertIn("argv", m["error"])
        self.assertEqual(m.get("code"), "invalid_argument")

        m = self.c.call({"op": "cmd_start", "argv": ["/nope/x"]})
        self.assertIn("No such", m["error"])
        self.assertIn("/nope/x", m["error"])
        self.assertEqual(m.get("code"), "spawn_failed")

        m = self.c.call({"op": "cmd_status", "cmd_id": "x"})
        self.assertIn("No such command", m["error"])
        self.assertEqual(m.get("code"), "not_found")

        m = self.c.call({"op": "file_list", "path": "/nope/x"})
        self.assertIn("No such file", m["error"])
        self.assertEqual(m.get("code"), "not_found")
        self.assertTrue(self.c.call({"op": "health"})["ok"])

    def test_protocol_structured_error_codes_and_cmd_start_pid(self):
        # cmd_start returns pid directly matching cmd_status
        started = self.c.call({"op": "cmd_start", "argv": ["true"]})
        self.assertTrue(started["ok"], started)
        self.assertIsInstance(started.get("pid"), int)
        self.assertGreater(started["pid"], 0)
        cid = started["cmd_id"]
        for _ in range(200):
            st = self.c.call({"op": "cmd_status", "cmd_id": cid})
            if not st["running"]:
                break
            time.sleep(0.02)
        self.assertEqual(started["pid"], st["pid"])

        # cmd_signal on non-running command returns code=not_running
        sig_err = self.c.call({"op": "cmd_signal", "cmd_id": cid, "signal": "TERM"})
        self.assertEqual(sig_err.get("code"), "not_running")
        self.assertIn("not running", sig_err["error"])

        # file_list on a regular file returns code=not_a_directory
        regular = os.path.join(self.tmp, "regular.txt")
        with open(regular, "w") as f:
            f.write("hi")
        list_err = self.c.call({"op": "file_list", "path": regular})
        self.assertEqual(list_err.get("code"), "not_a_directory")

        # file_delete protected path and non-empty directory
        prot_err = self.c.call({"op": "file_delete", "path": "/etc", "recursive": True})
        self.assertEqual(prot_err.get("code"), "protected_path")
        self.assertIn("refusing to delete", prot_err["error"])

        sub = os.path.join(self.tmp, "sub_nonempty")
        os.mkdir(sub)
        with open(os.path.join(sub, "item"), "w") as f:
            f.write("x")
        ne_err = self.c.call({"op": "file_delete", "path": sub})
        self.assertEqual(ne_err.get("code"), "not_empty")
        self.assertIn("non-empty directory", ne_err["error"])

        # file_move already_exists and is_a_directory
        other = os.path.join(self.tmp, "other.txt")
        with open(other, "w") as f:
            f.write("o")
        ex_err = self.c.call({"op": "file_move", "from": regular, "to": other})
        self.assertEqual(ex_err.get("code"), "already_exists")
        self.assertIn("exists", ex_err["error"])

        dir_err = self.c.call({"op": "file_move", "from": regular, "to": sub, "overwrite": True})
        self.assertEqual(dir_err.get("code"), "conflict")
        self.assertIn("is a directory", dir_err["error"])

        # exec spawn_failed (missing binary & missing cwd), permission_denied, and desktop unsupported
        exec_bin_err = self.c.call({"op": "exec", "argv": ["/nonexistent/bin"]})
        self.assertEqual(exec_bin_err.get("code"), "spawn_failed")
        self.assertIn("/nonexistent/bin", exec_bin_err["error"])

        exec_cwd_err = self.c.call({"op": "exec", "argv": ["true"], "cwd": "/nonexistent/dir"})
        self.assertEqual(exec_cwd_err.get("code"), "spawn_failed")
        self.assertIn("/nonexistent/dir", exec_cwd_err["error"])

        noexec = os.path.join(self.tmp, "noexec.sh")
        with open(noexec, "w") as f:
            f.write("#!/bin/sh\necho hi\n")
        os.chmod(noexec, 0o644)
        perm_err = self.c.call({"op": "exec", "argv": [noexec]})
        self.assertEqual(perm_err.get("code"), "permission_denied")

        # session exec spawn_failed propagates code
        sid = "sf-%d" % time.monotonic_ns()
        sess_err = self.c.call({"op": "exec", "session": sid, "argv": ["/nonexistent/bin"]})
        self.assertEqual(sess_err.get("code"), "spawn_failed")
        self.assertIn("/nonexistent/bin", sess_err["error"])

        # desktop start on slim image (unit absent) preserves code=unsupported
        with mock.patch.object(agent.os.path, "exists", return_value=False):
            desk_err = self.c.call({"op": "desktop", "action": "start"})
        self.assertEqual(desk_err.get("code"), "unsupported")

        # unknown user returns code=invalid_user with "no such user:" message
        with mock.patch.object(agent.boat_ops.os, "geteuid", return_value=0), \
             mock.patch.object(agent.boat_ops.pwd, "getpwnam", side_effect=KeyError("nobody_here")):
            user_err = self.c.call({"op": "exec", "user": "nobody_here", "argv": ["true"]})
        self.assertEqual(user_err.get("code"), "invalid_user")
        self.assertIn("no such user: nobody_here", user_err["error"])

    def test_relative_and_tilde_cwd_resolve_against_target_user_home(self):
        target_home = os.path.join(self.tmp, "target_home")
        sub = os.path.join(target_home, "proj", "sub")
        os.makedirs(sub)
        fake_pw = types_ns(pw_name="dev", pw_uid=1000, pw_gid=1000, pw_dir=target_home, pw_shell="/bin/bash")

        # run_spec resolves ~, ~/, ~/..., and plain relative cwd against pw.pw_dir (not daemon "/" or process HOME).
        with mock.patch.object(agent, "user_info", return_value=fake_pw), \
             mock.patch.object(agent.os, "getgrouplist", return_value=[1000]), \
             mock.patch.dict(os.environ, {"HOME": "/wrong/daemon/home"}):
            self.assertEqual(agent.run_spec({"argv": ["pwd"]})["cwd"], target_home)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "~"})["cwd"], target_home)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "~/"})["cwd"], target_home)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "~/proj/sub"})["cwd"], sub)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "proj/sub"})["cwd"], sub)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "./proj/sub"})["cwd"], sub)
            self.assertEqual(agent.run_spec({"argv": ["pwd"], "cwd": "/tmp"})["cwd"], "/tmp")

        # End-to-end exec and cmd_start with relative/~ cwd when running as test user.
        with mock.patch.dict(os.environ, {"HOME": target_home}):
            for raw_cwd, want in [("~", target_home), ("~/proj/sub", sub), ("proj/sub", sub)]:
                out, _, end = self.c.exec(argv=["pwd"], cwd=raw_cwd, timeout_s=10)
                self.assertEqual(end["exit"], 0)
                self.assertEqual(os.path.realpath(out.decode().strip()), os.path.realpath(want))

                m = self.c.call({"op": "cmd_start", "argv": ["pwd"], "cwd": raw_cwd})
                self.assertTrue(m["ok"], m)
                cid = m["cmd_id"]
                for _ in range(200):
                    logs = self.c.call({"op": "cmd_logs", "cmd_id": cid})
                    if logs["size"] and not logs["running"]:
                        break
                    time.sleep(0.02)
                got = base64.b64decode(logs["data_b64"]).decode().strip()
                self.assertEqual(os.path.realpath(got), os.path.realpath(want))

            # Missing relative or ~/ cwd after resolution -> spawn_failed naming resolved path.
            for missing in ("nope_rel", "~/nope_rel"):
                err = self.c.call({"op": "exec", "argv": ["true"], "cwd": missing})
                self.assertEqual(err.get("code"), "spawn_failed")
                self.assertIn(os.path.join(target_home, "nope_rel"), err["error"])

                cerr = self.c.call({"op": "cmd_start", "argv": ["true"], "cwd": missing})
                self.assertEqual(cerr.get("code"), "spawn_failed")
                self.assertIn(os.path.join(target_home, "nope_rel"), cerr["error"])

    def test_all_operror_literal_codes_within_contract(self):
        import ast

        allowed = {
            "invalid_user", "spawn_failed", "unsupported", "not_found",
            "conflict", "already_exists", "is_a_directory", "not_empty",
            "not_running", "permission_denied", "invalid_argument", "protected_path",
        }
        for filename in ("boat_ops.py", "boat-agent.py"):
            tree = ast.parse(Path(HERE, filename).read_text())
            for node in ast.walk(tree):
                if isinstance(node, ast.Call):
                    fn = node.func
                    name = fn.id if isinstance(fn, ast.Name) else (fn.attr if isinstance(fn, ast.Attribute) else None)
                    if name == "OpError":
                        for kw in node.keywords:
                            if kw.arg == "code" and isinstance(kw.value, ast.Constant) and isinstance(kw.value.value, str):
                                self.assertIn(kw.value.value, allowed, "%s:%d" % (filename, node.lineno))

        # invalid_argument on relative path
        rel_err = self.c.call({"op": "file_list", "path": "relative/path"})
        self.assertEqual(rel_err.get("code"), "invalid_argument")

    def test_running_command_is_activity(self):
        m = self.c.call({"op": "cmd_start", "argv": ["sleep", "30"]})
        agent._activity["last"] -= 1000
        try:
            self.assertEqual(self.c.call({"op": "health"})["idle_s"], 0.0)
        finally:
            self.c.call({"op": "cmd_signal", "cmd_id": m["cmd_id"], "signal": "KILL"})

    def test_file_ops_through_the_protocol(self):
        with open(os.path.join(self.tmp, "a"), "w") as f:
            f.write("x")
        self.assertEqual(self.c.call({"op": "file_move", "from": os.path.join(self.tmp, "a"),
                                      "to": os.path.join(self.tmp, "b")}), {"ok": True})
        names = [e["name"] for e in self.c.call({"op": "file_list", "path": self.tmp})["entries"]]
        self.assertIn("b", names)
        self.assertEqual(self.c.call({"op": "file_delete", "path": os.path.join(self.tmp, "b")}),
                         {"ok": True})


class DesktopProxyTest(unittest.TestCase):
    def test_websocket_needs_the_cookie_not_just_the_query_token(self):
        tok = "t" * 32
        ws = b"Upgrade: websocket\r\nConnection: Upgrade\r\nHost: h\r\n"
        self.assertFalse(agent.check_token(b"GET /websockify?token=%s HTTP/1.1\r\n%s\r\n"
                                           % (tok.encode(), ws), tok)[0])
        self.assertFalse(agent.check_token(b"GET /websockify HTTP/1.1\r\n%s\r\n" % ws, tok)[0])
        self.assertEqual(agent.check_token(b"GET /websockify HTTP/1.1\r\n%sCookie: boat_desktop=%s\r\n\r\n"
                                           % (ws, tok.encode()), tok), (True, None))

    def test_origin_must_match_host(self):
        def head(*lines):
            return b"GET / HTTP/1.1\r\n" + b"".join(l + b"\r\n" for l in lines) + b"\r\n"
        ok = agent.origin_ok
        self.assertTrue(ok(head(b"Host: vm.example")))                # no Origin: not a browser fetch
        self.assertTrue(ok(head(b"Host: vm.example", b"Origin: https://vm.example")))
        self.assertTrue(ok(head(b"Host: VM.example:8742", b"origin: http://vm.EXAMPLE:8742")))
        self.assertFalse(ok(head(b"Host: vm.example", b"Origin: https://evil.example")))
        self.assertFalse(ok(head(b"Host: vm.example", b"Origin: https://vm.example.evil.example")))
        self.assertFalse(ok(head(b"Host: vm.example:1", b"Origin: https://vm.example:2")))
        self.assertFalse(ok(head(b"Host: vm.example", b"Origin: null")))
        self.assertFalse(ok(head(b"Origin: https://vm.example")))     # Origin without Host
        self.assertFalse(ok(head(b"Host: vm.example", b"Origin: https://vm.example",
                                 b"Origin: https://evil.example")))   # every Origin must match

    def test_proxy_rejects_a_cross_origin_websocket_even_with_the_cookie(self):
        backend = socket.socket()
        backend.bind(("127.0.0.1", 0))
        backend.listen(1)
        srv = agent.start_proxy(port=0, backend_addr=backend.getsockname(), host="127.0.0.1")
        agent._desktop["token"] = tok = "o" * 32
        try:
            s = socket.create_connection(("127.0.0.1", srv.getsockname()[1]), timeout=5)
            with s:
                s.sendall(b"GET /websockify HTTP/1.1\r\nHost: vm\r\nOrigin: https://evil\r\n"
                          b"Upgrade: websocket\r\nCookie: boat_desktop=%s\r\n\r\n" % tok.encode())
                self.assertIn(b"403", s.recv(4096))
        finally:
            agent._desktop["token"] = None
            srv.close()
            backend.close()

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
        self.addCleanup(backend.close)
        backend.settimeout(5)
        backend.bind(("127.0.0.1", 0))
        backend.listen(1)

        def serve():
            try:
                c, _ = backend.accept()
                with c:
                    c.settimeout(5)
                    c.recv(65536)
                    c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
            except OSError:
                pass
        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        srv = agent.start_proxy(port=0, backend_addr=backend.getsockname(), host="127.0.0.1")
        self.addCleanup(srv.close)
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
            if b"meta=1" in head.split(b"\r\n")[0]:  # the gateway's session page
                body = b'<!doctype html><meta http-equiv="refresh" content="0;url=/"><a href="/">x</a>'
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body))
                return
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
            link = "http://127.0.0.1:%d/?token=%s&meta=1" % (srv.getsockname()[1], tok)
            env = dict(os.environ, DESKTOP_BROWSER="0")  # no real client behind the fake
            r = subprocess.run(["bash", os.path.join(HERE, "..", "desktop-check.sh"), link, "1"],
                               capture_output=True, text=True, timeout=60, env=env)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIn("METRIC desktop_first_frame_ms=", r.stdout)
            fps = float(r.stdout.split("METRIC desktop_raw_8bpp_fps=")[1].split()[0])
            self.assertGreater(fps, 10, r.stdout)
            bad = subprocess.run(["bash", os.path.join(HERE, "..", "desktop-check.sh"),
                                  link.replace(tok, "x" * 32), "1"],
                                 capture_output=True, text=True, timeout=60, env=env)
            self.assertNotEqual(bad.returncode, 0)
        finally:
            agent._desktop["token"] = None
            srv.close()
            backend.close()


class TrustedRootExecTest(unittest.TestCase):
    def req(self, **changes):
        return dict(dict(op="exec", user="root", argv=["/usr/bin/python3", "-I", "-c", "pass"], trusted_root_exec=True), **changes)

    def test_clean_environment_before_spawn(self):
        from unittest import mock
        poison = {"LD_PRELOAD":"/evil", "LD_LIBRARY_PATH":"/evil", "PATH":"/evil", "PYTHONPATH":"/evil", "PYTHONHOME":"/evil", "HTTPS_PROXY":"http://evil", "SSL_CERT_FILE":"/evil"}
        root = types_ns(pw_name="root", pw_uid=0, pw_gid=0, pw_dir="/root", pw_shell="/bin/bash")
        fake = types_ns(stdout=types_ns(fileno=lambda:10), stderr=types_ns(fileno=lambda:11), stdin=object())
        with mock.patch.object(agent.os, "geteuid", return_value=0), mock.patch.object(agent, "user_info", return_value=root), mock.patch.dict(os.environ, poison), mock.patch.object(agent, "read_env_file", return_value=poison) as tenant, mock.patch.object(agent.os,"getgrouplist",return_value=[0]), mock.patch.object(agent.subprocess,"Popen",return_value=fake) as spawn:
            agent.spawn(self.req())
        tenant.assert_not_called()
        self.assertEqual(spawn.call_args.kwargs["env"], {"PATH":"/usr/sbin:/usr/bin:/sbin:/bin","HOME":"/root","USER":"root","LOGNAME":"root","LANG":"C","SHELL":"/bin/sh"})
        self.assertEqual(spawn.call_args.kwargs["cwd"], "/root")
        self.assertEqual(spawn.call_args.kwargs["user"], 0)
        self.assertEqual(spawn.call_args.kwargs["extra_groups"], [])

    def test_rejects_malformed_and_misused_selector(self):
        from unittest import mock
        root=types_ns(pw_name="root",pw_uid=0,pw_gid=0,pw_dir="/root",pw_shell="/bin/bash")
        bad=[dict(trusted_root_exec=x) for x in [False,None,0,1,"true",{},[]]]
        bad += [dict(user=x) for x in ["dev","user","0",None,""]]
        bad += [dict(env=x) for x in [{"LD_PRELOAD":"/evil"},{"PATH":"/evil"},[],"bad"]]
        bad += [dict(tty=True),dict(cwd="/tmp")]
        for changes in bad:
            with self.subTest(changes=changes), mock.patch.object(agent.os,"geteuid",return_value=0), mock.patch.object(agent,"user_info",return_value=root), mock.patch.object(agent.os,"getgrouplist",return_value=[0]), self.assertRaises(agent.OpError):
                agent.run_spec(self.req(**changes))
        with mock.patch.object(agent.os,"geteuid",return_value=1000), mock.patch.object(agent,"user_info",return_value=None), self.assertRaises(agent.OpError):
            agent.run_spec(self.req())

    def test_normal_dev_environment_unchanged(self):
        from unittest import mock
        dev=types_ns(pw_name="dev",pw_uid=1000,pw_gid=1000,pw_dir="/home/dev",pw_shell="/bin/bash")
        with mock.patch.object(agent,"read_env_file",return_value={"TENANT":"yes","PATH":"/tenant/bin"}):
            env=agent.exec_env({"user":"dev","env":{"REQUEST":"yes"}},dev)
        self.assertEqual((env["TENANT"],env["REQUEST"],env["PATH"],env["HOME"]),("yes","yes","/tenant/bin","/home/dev"))

    def test_health_advertises_version_fence(self):
        reply=[]
        agent.op_health({},reply.append)
        self.assertIs(reply[0].get("trusted_root_exec"),True)


class UtmpIdleTest(unittest.TestCase):
    @staticmethod
    def _utmp_record(ut_type, line_bytes, user=b"dev", pid=1234):
        rec = bytearray(384)
        rec[0:2] = int(ut_type).to_bytes(2, "little", signed=True)
        rec[4:8] = int(pid).to_bytes(4, "little", signed=True)
        padded = line_bytes[:32].ljust(32, b"\x00")
        rec[8:40] = padded
        rec[44:44 + len(user)] = user[:32]
        return bytes(rec)

    def test_in_process_utmp_idle_without_subprocesses(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as tmp:
            dev_dir = os.path.join(tmp, "dev")
            pts_dir = os.path.join(dev_dir, "pts")
            os.makedirs(pts_dir)
            pts0 = os.path.join(pts_dir, "0")
            pts1 = os.path.join(pts_dir, "1")
            outside = os.path.join(tmp, "outside")
            for p in (pts0, pts1, outside):
                with open(p, "wb") as f:
                    f.write(b"")
            os.symlink(outside, os.path.join(pts_dir, "escape"))

            now_ts = 1_700_000_100.0
            os.utime(pts0, (now_ts - 12.0, now_ts - 12.0))
            os.utime(pts1, (now_ts - 3.5, now_ts - 3.5))
            os.utime(outside, (now_ts, now_ts))

            utmp_primary = os.path.join(tmp, "missing-utmp")
            utmp_fallback = os.path.join(tmp, "utmp")

            with mock.patch.object(agent.subprocess, "run", side_effect=AssertionError("unexpected subprocess.run")), \
                 mock.patch.object(agent.subprocess, "check_output", side_effect=AssertionError("unexpected check_output")), \
                 mock.patch.object(agent.subprocess, "Popen", side_effect=AssertionError("unexpected Popen")), \
                 mock.patch.object(agent, "UTMP_PATHS", (utmp_primary, utmp_fallback)), \
                 mock.patch.object(agent, "DEV_ROOT", dev_dir), \
                 mock.patch.object(agent.boat_ops, "running_count", return_value=0):

                # 1. Missing /run/utmp and /var/run/utmp -> clean fallback to elapsed time since last op.
                with agent._activity_lock:
                    agent._activity["running"] = 0
                    agent._activity["last"] = time.monotonic() - 40.0
                self.assertIsNone(agent.utmp_idle_s(now=now_ts))
                self.assertFalse(agent.ssh_sessions_active())
                self.assertGreaterEqual(agent.idle_seconds(), 39.0)

                # 2. DEAD_PROCESS (8) and unsafe traversal / symlink-escape USER_PROCESS (7) entries are ignored.
                with open(utmp_fallback, "wb") as f:
                    f.write(
                        self._utmp_record(8, b"pts/1") +
                        self._utmp_record(7, b"../outside") +
                        self._utmp_record(7, b"/etc/passwd") +
                        self._utmp_record(7, b"pts/../../outside") +
                        self._utmp_record(7, b"pts/escape")
                    )
                self.assertIsNone(agent.utmp_idle_s(now=now_ts))
                self.assertFalse(agent.ssh_sessions_active())
                self.assertGreaterEqual(agent.idle_seconds(), 39.0)

                # 3. Active USER_PROCESS (7) entries compute minimum TTY idle time from /dev/<ut_line> st_mtime.
                with open(utmp_fallback, "wb") as f:
                    f.write(
                        self._utmp_record(8, b"pts/dead") +
                        self._utmp_record(7, b"pts/0") +
                        self._utmp_record(7, b"pts/1")
                    )
                self.assertAlmostEqual(agent.utmp_idle_s(now=now_ts), 3.5, places=3)
                self.assertTrue(agent.ssh_sessions_active())

                with mock.patch.object(agent.time, "time", return_value=now_ts):
                    self.assertEqual(agent.idle_seconds(), 3.5)
                    os.utime(pts1, (now_ts, now_ts))
                    self.assertEqual(agent.idle_seconds(), 0.0)


class ManagedEnvSetupTest(unittest.TestCase):
    def setUp(self):
        path = Path(HERE).parent / "rootfs/usr/local/lib/boat/prepare-managed-env"
        self.assertTrue(path.is_file(), "managed environment boot helper is missing")
        loader = importlib.machinery.SourceFileLoader("prepare_managed_env", str(path))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        self.helper = importlib.util.module_from_spec(spec)
        loader.exec_module(self.helper)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.helper.BOAT_DIR = root / "etc/boat"
        self.helper.RUN_DIR = root / "run"
        self.helper.ROOT_UID = os.getuid()
        self.helper.ROOT_GID = os.getgid()
        self.helper.RUN_DIR.mkdir()
        self.helper.BOAT_DIR.mkdir(parents=True)
        self.helper.MOUNTINFO = root / "mountinfo"
        self.helper.MOUNTINFO.write_text(
            "1 0 0:1 / %s rw - tmpfs tmpfs rw\n" % self.helper.RUN_DIR)

    def test_migration_discards_legacy_env_and_keeps_identity(self):
        env = self.helper.BOAT_DIR / "env"
        env.write_text("export OLD_MANAGED_VALUE=old\n")
        (self.helper.BOAT_DIR / "env.boat-tmp").write_text("old partial value")
        ident = self.helper.BOAT_DIR / "identity.json"
        ident.write_text('{"hostname":"same-vm"}')
        self.helper.prepare()
        self.assertTrue(env.is_symlink())
        self.assertEqual(os.readlink(env), str(self.helper.RUN_DIR / "boat/env"))
        self.assertFalse(env.exists())
        self.assertFalse((self.helper.BOAT_DIR / "env.boat-tmp").exists())
        self.assertEqual(ident.read_text(), '{"hostname":"same-vm"}')
        self.assertEqual((self.helper.RUN_DIR / "boat").stat().st_mode & 0o777, 0o755)

    def test_repeated_prepare_preserves_this_boot_env(self):
        self.helper.prepare()
        runtime = self.helper.RUN_DIR / "boat/env"
        runtime.write_text("export VALUE=this-boot\n")
        self.helper.prepare()
        self.assertEqual((self.helper.BOAT_DIR / "env").read_text(), runtime.read_text())

    def test_disk_backed_run_is_refused(self):
        self.helper.MOUNTINFO.write_text(
            "1 0 8:1 / %s rw - ext4 /dev/vda rw\n" % self.helper.RUN_DIR)
        with self.assertRaisesRegex(RuntimeError, "tmpfs"):
            self.helper.prepare()

    def test_image_setup_needs_no_runtime_mount_and_discards_old_file(self):
        self.helper.MOUNTINFO.unlink()
        (self.helper.BOAT_DIR / "env").write_text("export OLD=old\n")
        self.helper.prepare(image_setup=True)
        self.assertTrue((self.helper.BOAT_DIR / "env").is_symlink())
        self.assertFalse((self.helper.RUN_DIR / "boat").exists())


if __name__ == "__main__":
    unittest.main()
