"""Tests for detached commands and file ops (python3 -m unittest test_boat_ops)."""
import base64
import os
import shutil
import signal
import stat
import tempfile
import time
import unittest

import boat_ops as ops


def call(fn, req, *args):
    out = []
    fn(req, out.append, *args)
    assert len(out) == 1, out
    return out[0]


def spec(argv, **kw):
    return dict({"argv": argv, "env": {"PATH": os.environ["PATH"], "HOME": "/tmp"}, "cwd": None}, **kw)


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.saved = ops.CMD_ROOT, ops.LOG_SEGMENT, ops.CMD_KEEP
        ops.CMD_ROOT = os.path.join(self.tmp, "cmds")
        self.addCleanup(self.restore)

    def restore(self):
        ops.CMD_ROOT, ops.LOG_SEGMENT, ops.CMD_KEEP = self.saved

    def path(self, *parts):
        return os.path.join(self.tmp, *parts)

    def write(self, name, data=b"x"):
        p = self.path(name)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as f:
            f.write(data)
        return p


class CmdTest(Base):
    def start(self, argv, **kw):
        m = call(ops.op_cmd_start, {}, spec(argv, **kw))
        self.assertTrue(m["ok"], m)
        self.addCleanup(self.kill_quietly, m["cmd_id"])
        return m["cmd_id"]

    def kill_quietly(self, cmd_id):
        try:
            call(ops.op_cmd_signal, {"cmd_id": cmd_id, "signal": "KILL"})
        except ops.OpError:
            pass

    def status(self, cmd_id):
        return call(ops.op_cmd_status, {"cmd_id": cmd_id})

    def wait_done(self, cmd_id, timeout=10):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            s = self.status(cmd_id)
            if not s["running"]:
                return s
            time.sleep(0.05)
        self.fail("still running: %r" % s)

    def logs(self, cmd_id, **kw):
        return call(ops.op_cmd_logs, dict({"cmd_id": cmd_id}, **kw))

    def test_exit_status_and_merged_logs(self):
        cid = self.start(["sh", "-c", "echo out; echo err >&2; exit 3"])
        s = self.wait_done(cid)
        self.assertEqual((s["state"], s["exit"], s["running"]), ("exited", 3, False))
        self.assertGreaterEqual(s["ended"], s["started"])
        self.assertEqual(s["argv"][0], "sh")
        self.assertNotIn("env", s)
        for _ in range(100):  # the supervisor may still be writing the last chunk
            m = self.logs(cid)
            if m["size"] >= 8:
                break
            time.sleep(0.05)
        self.assertEqual(sorted(base64.b64decode(m["data_b64"]).split()), [b"err", b"out"])
        self.assertEqual((m["offset"], m["next_offset"], m["truncated"]), (0, 8, False))

    def test_spawn_failure_is_an_error_and_leaves_nothing(self):
        with self.assertRaisesRegex(ops.OpError, "No such file"):
            call(ops.op_cmd_start, {}, spec(["/nonexistent/binary"]))
        with self.assertRaisesRegex(ops.OpError, "No such file"):
            call(ops.op_cmd_start, {}, spec(["true"], cwd="/nonexistent/dir"))
        self.assertEqual(call(ops.op_cmd_list, {})["cmds"], [])

    def test_env_and_cwd_reach_the_command_and_secrets_leave_the_disk(self):
        cid = self.start(["sh", "-c", 'printf "%s %s" "$FOO" "$PWD"'],
                         env={"PATH": os.environ["PATH"], "FOO": "secret-value"},
                         cwd=self.tmp)
        self.wait_done(cid)
        for _ in range(100):
            if self.logs(cid)["size"]:
                break
            time.sleep(0.05)
        text = base64.b64decode(self.logs(cid)["data_b64"]).decode()
        self.assertTrue(text.startswith("secret-value "), text)
        self.assertEqual(os.path.realpath(text.split(" ", 1)[1]), os.path.realpath(self.tmp))
        for root, _, names in os.walk(ops.CMD_ROOT):
            self.assertEqual(stat.S_IMODE(os.stat(root).st_mode), 0o700)
            for n in names:
                with open(os.path.join(root, n), "rb") as f:
                    if not n.startswith("seg."):
                        self.assertNotIn(b"secret-value", f.read(), n)

    def test_survives_the_starter(self):
        # The supervisor is detached: it is not our child, so nothing we do to our
        # own process tree (an agent restart) reaches it.
        cid = self.start(["sh", "-c", "sleep 30"])
        s = self.status(cid)
        self.assertTrue(s["running"], s)
        with self.assertRaises(ChildProcessError):
            os.waitpid(s["pid"], os.WNOHANG)
        self.assertNotEqual(os.getsid(s["pid"]), os.getsid(0))

    def test_signal_reaches_the_process_group(self):
        marker = self.path("survived")
        cid = self.start(["sh", "-c", "(sleep 2; touch %s) & wait" % marker])
        self.assertEqual(call(ops.op_cmd_signal, {"cmd_id": cid, "signal": "TERM"}), {"ok": True})
        s = self.wait_done(cid)
        self.assertEqual((s["signal"], s["exit"]), ("SIGTERM", 128 + signal.SIGTERM))
        time.sleep(2.3)
        self.assertFalse(os.path.exists(marker), "signal missed the background child")
        with self.assertRaisesRegex(ops.OpError, "not running"):
            call(ops.op_cmd_signal, {"cmd_id": cid, "signal": "TERM"})

    def test_signal_forms(self):
        for form in ("KILL", "SIGKILL", int(signal.SIGKILL)):
            cid = self.start(["sleep", "30"])
            call(ops.op_cmd_signal, {"cmd_id": cid, "signal": form})
            self.assertEqual(self.wait_done(cid)["signal"], "SIGKILL")
        cid = self.start(["sleep", "30"])
        for bad in ("NOPE", 0, 9999, None):
            with self.assertRaisesRegex(ops.OpError, "signal"):
                call(ops.op_cmd_signal, {"cmd_id": cid, "signal": bad})

    def test_unknown_and_malicious_ids(self):
        for bad in ("c_0000000000000000", "../../etc", "", None, 7):
            with self.assertRaisesRegex(ops.OpError, "No such command"):
                self.status(bad)

    def test_lost_when_the_supervisor_is_gone(self):
        cid = self.start(["sleep", "30"])
        s = self.status(cid)
        os.killpg(s["pid"], signal.SIGKILL)
        os.kill(ops.read_raw_status(cid)["supervisor"], signal.SIGKILL)
        time.sleep(0.2)
        s = self.status(cid)
        self.assertEqual((s["state"], s["running"]), ("lost", False))

    def test_logs_paging_and_size_cap(self):
        ops.LOG_SEGMENT = 1000
        chunk = "head -c 500 /dev/zero | tr '\\0' a; sleep 0.03; "
        cid = self.start(["sh", "-c", chunk * 10 + "printf END"])
        self.wait_done(cid)
        for _ in range(100):
            m = self.logs(cid, offset=0, limit=10)
            if m["size"] == 5003:
                break
            time.sleep(0.05)
        self.assertEqual(m["size"], 5003)
        # only the newest segments are kept, and a stale offset says so
        self.assertTrue(m["truncated"])
        self.assertGreater(m["offset"], 0)
        self.assertEqual(m["next_offset"], m["offset"] + 10)
        total = sum(os.path.getsize(os.path.join(ops.CMD_ROOT, cid, n))
                    for n in os.listdir(os.path.join(ops.CMD_ROOT, cid)) if n.startswith("seg."))
        self.assertLess(total, 3100)  # two segments, each may overshoot by one read
        # paging from the base reaches the end exactly once, across segments
        got, off = b"", m["offset"]
        while off < m["size"]:
            p = self.logs(cid, offset=off, limit=700)
            self.assertFalse(p["truncated"])
            self.assertEqual(p["offset"], off)
            got += base64.b64decode(p["data_b64"])
            off = p["next_offset"]
        self.assertTrue(got.endswith(b"aEND"))
        self.assertEqual(len(got), m["size"] - m["offset"])
        past = self.logs(cid, offset=999999)
        self.assertEqual((past["data_b64"], past["next_offset"]), ("", 5003))

    def test_logs_limit_is_clamped(self):
        cid = self.start(["true"])
        self.wait_done(cid)
        for bad in (-1, "x"):
            with self.assertRaises((ops.OpError, ValueError)):
                self.logs(cid, limit=bad)

    def test_list_and_prune_keep_running_ones(self):
        ops.CMD_KEEP = 2
        long_running = self.start(["sleep", "30"])
        done = []
        for _ in range(4):
            done.append(self.start(["true"]))
            self.wait_done(done[-1])
        ids = [c["cmd_id"] for c in call(ops.op_cmd_list, {})["cmds"]]
        self.assertIn(long_running, ids)
        self.assertIn(done[-1], ids)
        self.assertNotIn(done[0], ids)
        # the running one, CMD_KEEP finished, and the newest if it finished after its prune
        self.assertLessEqual(len(ids), 4)
        self.assertEqual(ops.running_count(), 1)


class FileTest(Base):
    def test_list(self):
        self.write("d/a.txt", b"hello")
        os.mkdir(self.path("d/sub"))
        os.symlink("a.txt", self.path("d/ln"))
        os.chmod(self.path("d/a.txt"), 0o640)
        m = call(ops.op_file_list, {"path": self.path("d")})
        got = {e["name"]: e for e in m["entries"]}
        self.assertEqual(sorted(got), ["a.txt", "ln", "sub"])
        self.assertEqual((got["a.txt"]["type"], got["a.txt"]["size"], got["a.txt"]["mode"]),
                         ("file", 5, "640"))
        self.assertEqual((got["sub"]["type"], got["ln"]["type"]), ("dir", "symlink"))
        self.assertGreater(got["a.txt"]["mtime"], 0)
        self.assertFalse(m["truncated"])

    def test_list_errors(self):
        with self.assertRaisesRegex(OSError, "No such file"):
            call(ops.op_file_list, {"path": self.path("nope")})
        with self.assertRaisesRegex(ops.OpError, "absolute"):
            call(ops.op_file_list, {"path": "relative"})

    def test_list_cap(self):
        saved = ops.FILE_LIST_MAX
        ops.FILE_LIST_MAX = 2
        self.addCleanup(setattr, ops, "FILE_LIST_MAX", saved)
        for n in "abc":
            self.write("c/" + n)
        m = call(ops.op_file_list, {"path": self.path("c")})
        self.assertEqual((len(m["entries"]), m["truncated"]), (2, True))

    def test_delete(self):
        f = self.write("x/f")
        self.assertEqual(call(ops.op_file_delete, {"path": f}), {"ok": True})
        self.assertFalse(os.path.exists(f))
        self.write("x/g")
        with self.assertRaisesRegex(ops.OpError, "recursive"):
            call(ops.op_file_delete, {"path": self.path("x")})
        self.assertTrue(os.path.exists(self.path("x/g")))
        call(ops.op_file_delete, {"path": self.path("x"), "recursive": True})
        self.assertFalse(os.path.exists(self.path("x")))
        with self.assertRaisesRegex(OSError, "No such file"):
            call(ops.op_file_delete, {"path": self.path("x")})

    def test_delete_does_not_follow_symlinks(self):
        keep = self.write("keep/precious")
        os.mkdir(self.path("t"))
        os.symlink(self.path("keep"), self.path("t/ln"))
        call(ops.op_file_delete, {"path": self.path("t"), "recursive": True})
        self.assertTrue(os.path.exists(keep))
        os.symlink(self.path("keep"), self.path("ln2"))
        call(ops.op_file_delete, {"path": self.path("ln2"), "recursive": True})
        self.assertTrue(os.path.exists(keep))

    def test_delete_refuses_system_roots(self):
        for p in ("/", "//", "/.", "/proc", "/sys/", "/dev", "/usr/../"):
            with self.assertRaisesRegex(ops.OpError, "refus"):
                call(ops.op_file_delete, {"path": p, "recursive": True})

    def test_move(self):
        a = self.write("a", b"A")
        b = self.path("new/dir/b")
        self.assertEqual(call(ops.op_file_move, {"from": a, "to": b}), {"ok": True})
        self.assertFalse(os.path.exists(a))
        with open(b, "rb") as f:
            self.assertEqual(f.read(), b"A")

    def test_move_will_not_overwrite_unless_asked(self):
        a, c = self.write("a", b"A"), self.write("c", b"C")
        with self.assertRaisesRegex(ops.OpError, "exists"):
            call(ops.op_file_move, {"from": a, "to": c})
        with open(c, "rb") as f:
            self.assertEqual(f.read(), b"C")
        call(ops.op_file_move, {"from": a, "to": c, "overwrite": True})
        with open(c, "rb") as f:
            self.assertEqual(f.read(), b"A")
        with self.assertRaisesRegex(OSError, "No such file"):
            call(ops.op_file_move, {"from": a, "to": c, "overwrite": True})
        with self.assertRaisesRegex(ops.OpError, "absolute"):
            call(ops.op_file_move, {"from": "rel", "to": c})

    def test_move_directory(self):
        self.write("src/inner", b"I")
        call(ops.op_file_move, {"from": self.path("src"), "to": self.path("dst")})
        with open(self.path("dst/inner"), "rb") as f:
            self.assertEqual(f.read(), b"I")


@unittest.skipUnless(shutil.which("rg"), "ripgrep not installed (test-in-vm.sh covers it)")
class SearchTest(Base):
    def setUp(self):
        super().setUp()
        self.write("s/one.txt", b"alpha\nneedle here\nomega\n")
        self.write("s/two.py", b"needle = 1\nneed.e\n")
        self.write("s/sub/three.md", b"nothing\n")

    def search(self, **kw):
        return call(ops.op_file_search, dict({"path": self.path("s")}, **kw))

    def test_content_is_the_default_and_fixed_string(self):
        m = self.search(query="need.e")
        self.assertEqual([(os.path.basename(x["path"]), x["line"], x["text"]) for x in m["matches"]],
                         [("two.py", 2, "need.e")])
        self.assertFalse(m["truncated"])

    def test_regex(self):
        self.assertEqual(len(self.search(query="need.e", regex=True)["matches"]), 3)
        with self.assertRaisesRegex(ops.OpError, "search"):
            self.search(query="(", regex=True)

    def test_no_match_is_empty_not_an_error(self):
        self.assertEqual(self.search(query="zzzz")["matches"], [])

    def test_limit(self):
        m = self.search(query="needle", limit=1)
        self.assertEqual((len(m["matches"]), m["truncated"]), (1, True))

    def test_query_cannot_be_a_flag(self):
        self.assertEqual(self.search(query="--version")["matches"], [])

    def test_filenames(self):
        m = self.search(query="THREE", content=False)
        self.assertEqual([os.path.basename(x["path"]) for x in m["matches"]], ["three.md"])
        self.assertNotIn("line", m["matches"][0])

    def test_errors(self):
        with self.assertRaisesRegex(OSError, "No such file"):
            call(ops.op_file_search, {"path": self.path("nope"), "query": "x"})
        with self.assertRaisesRegex(ops.OpError, "query"):
            self.search(query="")


if __name__ == "__main__":
    unittest.main()
