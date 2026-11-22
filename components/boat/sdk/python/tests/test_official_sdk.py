import os
import shlex
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "compat")))
import official_sdk  # noqa: E402


class _SshKeyReq:
    def __init__(self, key: str):
        self.key = key


class _SshKeyResp:
    def __init__(self, ssh_user: str = "dev", machine_ip: str = "10.200.0.2", ssh_endpoint=None):
        self.ssh_user = ssh_user
        self.machine_ip = machine_ip
        self.ssh_endpoint = ssh_endpoint


class _CmdOut:
    def __init__(self, exit_code: int, stdout: str, stderr: str = ""):
        self.exit_code = exit_code
        self.stdout = stdout
        self.stderr = stderr


class OfficialSdkSshTest(unittest.TestCase):
    def test_ed25519_key_generation_and_validation(self):
        kt, kb = official_sdk.parse_openssh_ed25519_key(official_sdk.DEFAULT_SSH_KEY)
        self.assertEqual(kt, "ssh-ed25519")
        self.assertTrue(len(kb) > 40)

        with tempfile.TemporaryDirectory() as tmpdir:
            priv_path, pub_line = official_sdk.generate_ssh_keypair(tmpdir, comment="real-check")
            kt2, kb2 = official_sdk.parse_openssh_ed25519_key(pub_line)
            self.assertEqual(kt2, "ssh-ed25519")
            self.assertIn("real-check", pub_line)
            if priv_path is not None:
                self.assertTrue(os.path.isfile(priv_path))

        for bad in (
            "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ== bad-type",
            "ssh-ed25519 not-valid-base64!!! real-check",
            "ssh-ed25519 QUFB real-check",
        ):
            with self.assertRaises(ValueError):
                official_sdk.parse_openssh_ed25519_key(bad)

    def test_parse_ssh_endpoint(self):
        self.assertEqual(
            official_sdk.parse_ssh_endpoint("10.200.0.5", None),
            ("10.200.0.5", 22, None),
        )
        self.assertEqual(
            official_sdk.parse_ssh_endpoint("10.200.0.5", "10.200.0.5:2222"),
            ("10.200.0.5", 2222, None),
        )
        self.assertEqual(
            official_sdk.parse_ssh_endpoint("10.200.0.5", "10.200.0.5:22 via jump@localhost:2222"),
            ("10.200.0.5", 22, "jump@localhost:2222"),
        )

    def test_verify_ssh_key_mock_mode_validates_key_without_guest_exec(self):
        seen_keys = []

        class FakeApi:
            def read_file(self, *a, **kw):
                raise AssertionError("must not read /home/user/.ssh/authorized_keys")

            def ssh_key(self, sid, req):
                seen_keys.append((sid, req.key))
                official_sdk.parse_openssh_ed25519_key(req.key)
                return _SshKeyResp(ssh_user="user", machine_ip=None, ssh_endpoint=None)

        checks = []
        official_sdk.verify_ssh_key_check(
            FakeApi(),
            "bx_fake",
            real=False,
            ssh_key_request_cls=_SshKeyReq,
            run_fn=lambda sid, cmd: (_ for _ in ()).throw(AssertionError("run_fn must not run in mock mode")),
            check_fn=lambda ok, what: (checks.append((ok, what)), self.assertTrue(ok, what)),
        )
        self.assertEqual(len(seen_keys), 1)
        self.assertEqual(seen_keys[0][0], "bx_fake")
        self.assertIn("real-check", seen_keys[0][1])
        self.assertEqual(checks, [(True, "authorize an ssh key")])

    def test_verify_ssh_key_real_mode_uses_live_authority_and_jump_ssh(self):
        granted = set()
        guest_cmds = []
        ssh_invocations = []

        class FakeRealApi:
            def read_file(self, *a, **kw):
                raise AssertionError("REAL mode must never read static ~/.ssh/authorized_keys")

            def ssh_key(self, sid, req):
                kt, kb = official_sdk.parse_openssh_ed25519_key(req.key)
                granted.add((sid, "dev", kt, kb))
                return _SshKeyResp(
                    ssh_user="dev",
                    machine_ip="10.200.0.9",
                    ssh_endpoint="10.200.0.9:22 via jump@localhost:2222",
                )

        def fake_guest_run(sid: str, cmd: str):
            guest_cmds.append(cmd)
            self.assertNotIn("authorized_keys", cmd)
            parts = shlex.split(cmd)
            self.assertIn(official_sdk.GUEST_SSH_AUTHORIZE, parts)
            idx = parts.index("authorize")
            user, kt, kb = parts[idx + 1], parts[idx + 2], parts[idx + 3]
            if (sid, user, kt, kb) in granted:
                return _CmdOut(0, f"{kt} {kb}\n")
            return _CmdOut(1, "")

        def fake_ssh_runner(argv, **kwargs):
            ssh_invocations.append(argv)
            return subprocess.CompletedProcess(argv, 0, stdout="real-check-ok\n", stderr="")

        checks = []
        official_sdk.verify_ssh_key_check(
            FakeRealApi(),
            "bx_real_1",
            real=True,
            ssh_key_request_cls=_SshKeyReq,
            run_fn=fake_guest_run,
            check_fn=lambda ok, what: (checks.append((ok, what)), self.assertTrue(ok, what)),
            ssh_runner=fake_ssh_runner,
        )
        self.assertEqual(len(guest_cmds), 2)
        self.assertTrue(all(ok for ok, _ in checks))
        if ssh_invocations:
            joined = " ".join(ssh_invocations[0])
            self.assertIn("ProxyCommand=", joined)
            self.assertIn("jump@localhost", joined)
            self.assertIn("dev@10.200.0.9", joined)

    def test_verify_ssh_key_real_mode_rejects_unclean_ungranted_denials(self):
        class FakeApi:
            def ssh_key(self, sid, req):
                return _SshKeyResp(machine_ip="10.200.0.9")

        label = "ungranted ssh key is rejected by live guest authority"
        for exit_code, stdout, stderr in (
            (0, "key", ""),
            (0, "", ""),
            (1, "", "sudo: a password is required"),
            (1, "key", ""),
        ):
            with self.subTest(exit_code=exit_code, stdout=stdout, stderr=stderr):
                calls = []
                checks = []

                def fake_guest_run(sid, cmd):
                    calls.append(cmd)
                    kt, kb = shlex.split(cmd)[-2:]
                    key_line = f"{kt} {kb}\n"
                    if len(calls) == 1:
                        return _CmdOut(0, key_line)
                    return _CmdOut(exit_code, key_line if stdout else "", stderr)

                def check(ok, what):
                    checks.append((ok, what))
                    self.assertTrue(ok, what)

                with self.assertRaisesRegex(AssertionError, label):
                    official_sdk.verify_ssh_key_check(
                        FakeApi(),
                        "bx_unclean_denial",
                        real=True,
                        ssh_key_request_cls=_SshKeyReq,
                        run_fn=fake_guest_run,
                        check_fn=check,
                    )
                self.assertEqual(len(calls), 2)
                self.assertEqual(checks[-1], (False, label))
                self.assertTrue(all(ok for ok, _ in checks[:-1]))

    def test_verify_ssh_key_real_mode_fails_when_live_authority_denies(self):
        class BrokenAuthorityApi:
            def ssh_key(self, sid, req):
                return _SshKeyResp(ssh_user="dev", machine_ip="10.200.0.9", ssh_endpoint=None)

        failures = []
        official_sdk.verify_ssh_key_check(
            BrokenAuthorityApi(),
            "bx_denied",
            real=True,
            ssh_key_request_cls=_SshKeyReq,
            run_fn=lambda sid, cmd: _CmdOut(0, ""),
            check_fn=lambda ok, what: None if ok else failures.append(what),
        )
        self.assertIn("the ssh key is authorized via live guest authority", failures)


if __name__ == "__main__":
    unittest.main()
