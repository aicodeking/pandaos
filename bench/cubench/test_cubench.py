#!/usr/bin/env python3
"""cubench self-tests, no VM and no API: python3 bench/cubench/test_cubench.py"""
import glob
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import agent  # noqa: E402
import cubench  # noqa: E402
import qmp  # noqa: E402
import score  # noqa: E402

OS_NAMES = sorted(os.path.basename(p)[:-5] for p in glob.glob(os.path.join(HERE, "os", "*.json")))


class KeyMap(unittest.TestCase):
    def test_all_printable_ascii(self):
        shifted = set('~!@#$%^&*()_+{}|:"<>?ABCDEFGHIJKLMNOPQRSTUVWXYZ')
        for c in map(chr, range(32, 127)):
            keys = qmp.char_keys(c)
            self.assertEqual(keys[0] == "shift", c in shifted, c)
            self.assertEqual(len(keys), 2 if c in shifted else 1, c)
        self.assertEqual(qmp.char_keys("A"), ["shift", "a"])
        self.assertEqual(qmp.char_keys("7"), ["7"])
        self.assertEqual(qmp.char_keys("&"), ["shift", "7"])
        self.assertEqual(qmp.char_keys(" "), ["spc"])
        self.assertEqual(qmp.char_keys("\n"), ["ret"])
        self.assertEqual(qmp.char_keys('"'), ["shift", "apostrophe"])
        self.assertEqual(qmp.char_keys("`"), ["grave_accent"])
        for bad in ("é", "\x00", "€"):
            with self.assertRaises(ValueError):
                qmp.char_keys(bad)

    def test_combos(self):
        self.assertEqual(qmp.combo_keys("ctrl+shift+n"), ["ctrl", "shift", "n"])
        self.assertEqual(qmp.combo_keys("ctrl+S"), ["ctrl", "s"])
        self.assertEqual(qmp.combo_keys("Return"), ["ret"])
        self.assertEqual(qmp.combo_keys("alt+F4"), ["alt", "f4"])
        self.assertEqual(qmp.combo_keys("super"), ["meta_l"])
        self.assertEqual(qmp.combo_keys("ctrl++"), ["ctrl", "shift", "equal"])
        self.assertEqual(qmp.combo_keys("ctrl+?"), ["ctrl", "shift", "slash"])
        for bad in ("hyper+x", "ctrl+foo", "a++b"):
            with self.assertRaises(ValueError):
                qmp.combo_keys(bad)


class Tasks(unittest.TestCase):
    def test_real_tasks_load_and_cover_categories(self):
        tasks = cubench.load_tasks(os.path.join(HERE, "tasks"))
        self.assertGreaterEqual(len(tasks), 20)
        ids = " ".join(t["id"] for t in tasks)
        for cat in ("editor", "files", "terminal", "browser", "clipboard", "settings", "dialog",
                    "multi-window", "scroll", "drag", "keyboard", "multi-step"):
            self.assertIn(cat, ids)
        self.assertTrue(all(t.get("tools_exclude") for t in tasks if t["id"].startswith("keyboard")))

    def test_rejects_bad_tasks(self):
        d = tempfile.mkdtemp()
        good = {"id": "x", "title": "t", "instruction": "i", "setup": [], "verify": "true",
                "timeout_s": 10, "max_steps": 5}
        for patch in ({"verify": 1}, {"id": "y"}, {"max_steps": 0}, {"setup": "echo"},
                      {"tools_exclude": ["type"]}):
            with open(os.path.join(d, "x.json"), "w") as f:
                json.dump({**good, **patch}, f)
            with self.assertRaises(ValueError, msg=patch):
                cubench.load_tasks(d)
        with open(os.path.join(d, "x.json"), "w") as f:
            json.dump(good, f)
        self.assertEqual(len(cubench.load_tasks(d)), 1)
        with self.assertRaises(ValueError):
            cubench.load_tasks(d, "nomatch")
        shutil.rmtree(d)

    @unittest.skipUnless(shutil.which("bash"), "needs bash")
    def test_shell_parses(self):
        tasks = cubench.load_tasks(os.path.join(HERE, "tasks"))
        for name in OS_NAMES:
            p = cubench.load_os(name)
            for t in tasks:
                script = cubench.prelude(p) + "\n".join(p["reset"] + t["setup"]) + "\n" + t["verify"]
                r = subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True)
                self.assertEqual(r.returncode, 0, f"{name}/{t['id']}: {r.stderr}")

    @unittest.skipUnless(shutil.which("bash"), "needs bash")
    def test_verify_fails_on_untouched_setup(self):
        """Every checker must fail if the agent does nothing (setup only, no apps)."""
        p = cubench.load_os("pandaos-desktop")
        home = tempfile.mkdtemp()
        stub = "launch() { :; }\nserve_form() { :; }\npkill() { :; }\n"
        for t in cubench.load_tasks(os.path.join(HERE, "tasks")):
            script = (f"export HOME={home}\n" + cubench.prelude(p) + stub + 'rm -rf "$B"; mkdir -p "$B"\n'
                      + "\n".join(t["setup"]) + "\nsleep() { :; }\n" + t["verify"])
            r = subprocess.run(["bash"], input=script, text=True, capture_output=True,
                               env={"PATH": os.environ["PATH"], "HOME": home})
            self.assertNotEqual(r.returncode, 0, f"{t['id']} passes without the agent")
        shutil.rmtree(home)


class Scoring(unittest.TestCase):
    def test_wilson(self):
        lo, hi = score.wilson(0, 10)
        self.assertEqual(lo, 0.0)
        self.assertAlmostEqual(hi, 0.2775, places=4)
        lo, hi = score.wilson(10, 10)
        self.assertAlmostEqual(lo, 0.7225, places=4)
        self.assertEqual(hi, 1.0)
        lo, hi = score.wilson(5, 10)
        self.assertAlmostEqual(lo, 0.2366, places=4)
        self.assertAlmostEqual(hi, 0.7634, places=4)
        self.assertEqual(score.wilson(0, 0), (0.0, 0.0))

    def test_fail_reason(self):
        self.assertIsNone(score.fail_reason({}, True))
        self.assertEqual(score.fail_reason({"error": "x", "out_of_budget": True}, False), "crash")
        self.assertEqual(score.fail_reason({"clicks": 4, "noop_clicks": 2, "out_of_budget": True}, False), "misclick")
        self.assertEqual(score.fail_reason({"clicks": 4, "noop_clicks": 1, "out_of_budget": True}, False), "timeout")
        self.assertEqual(score.fail_reason({"clicks": 1, "noop_clicks": 1}, False), "wrong_result")

    def test_summary_and_table(self):
        trials = [{"os": "a", "tools": "generic", "task": "t1", "passed": True, "steps": 3, "seconds": 10, "cost_usd": 0.1},
                  {"os": "a", "tools": "generic", "task": "t2", "passed": False, "reason": "timeout", "steps": 9,
                   "seconds": 30, "cost_usd": 0.2}]
        rows = score.summarize(trials)
        self.assertEqual(rows[0]["passes"], 1)
        self.assertEqual(rows[0]["medianSteps"], 6)
        self.assertEqual(rows[0]["failReasons"]["timeout"], 1)
        md = score.markdown(rows, trials)
        self.assertIn("| a | generic | 50% |", md)
        self.assertIn("| t2 | 0/1 |", md)


class Providers(unittest.TestCase):
    def test_openai_prunes_old_screenshots(self):
        os.environ.setdefault("OPENAI_API_KEY", "test")
        m = agent.OpenAICompat("m", agent.tool_specs("generic"))
        m.start("sys", "task", b"png0")
        for i in range(5):
            m.add_results([(f"c{i}", "ok", b"png", False)])
        m._prune()
        imgs = sum(1 for x in m.messages if x["role"] == "user" and isinstance(x["content"], list)
                   for part in x["content"] if part.get("type") == "image_url")
        self.assertEqual(imgs, agent.KEEP_IMAGES)

    def test_tool_specs(self):
        names = [n for n, _, _ in agent.tool_specs("generic", ["click", "drag"])]
        self.assertNotIn("click", names)
        self.assertIn("done", names)
        self.assertIn("cu_tree", [n for n, _, _ in agent.tool_specs("panda")])
        with self.assertRaises(ValueError):
            agent.tool_specs("bogus")

    def test_panda_cu_contract(self):
        calls = []

        def sh(script, timeout):
            calls.append(script)
            if "fail" in script:
                return 2, "", "boom"
            return 0, json.dumps({"ok": True, "png_base64": "iVBORw=="}), ""
        env = agent.Env(None, sh, tempfile.mkdtemp())
        text, png = env.panda("cu_find", {"name": "Save's"})
        self.assertEqual(calls[0], "panda-cu call cu_find '{\"name\": \"Save'\"'\"'s\"}'")
        self.assertEqual(json.loads(text), {"ok": True})
        self.assertTrue(png.startswith(b"\x89PNG"))
        with self.assertRaises(RuntimeError):
            env.panda("cu_fail", {})

    def test_cost(self):
        self.assertEqual(agent.cost_usd("claude-sonnet-5-5", {"input": 1e6, "output": 1e5, "cache_read": 1e6}), 3.2)
        self.assertIsNone(agent.cost_usd("unknown", {"input": 1}))
        self.assertEqual(agent.cost_usd("unknown", {"input": 1e6}, (1.0, 2.0)), 1.0)


class DryRun(unittest.TestCase):
    def test_dry_run_against_fake_qmp(self):
        out = tempfile.mkdtemp()
        rc = cubench.main(["run", "--dry-run", "--os", "pandaos-desktop,debian-13-xfce", "--out", out,
                           "--repeats", "1", "--tasks", "[ek]*"])
        self.assertEqual(rc, 0)
        files = sorted(glob.glob(os.path.join(out, "results-*.json")))
        self.assertEqual(len(files), 2)
        d = json.load(open(files[0]))
        self.assertEqual(d["meta"]["display"], "1280x800")
        n_tasks = len(cubench.load_tasks(os.path.join(HERE, "tasks"), "[ek]*"))
        self.assertEqual(len(d["trials"]), n_tasks)
        self.assertTrue(all(t["passed"] and t["agent_done"] for t in d["trials"]), d["trials"])
        kbd = [t for t in d["trials"] if t["task"].startswith("keyboard")]
        self.assertTrue(kbd and all(t["clicks"] == 0 for t in kbd))
        tr = glob.glob(os.path.join(out, "transcripts", "pandaos-desktop-generic", "editor-new-file-r1", "*"))
        self.assertIn("transcript.jsonl", [os.path.basename(p) for p in tr])
        self.assertGreater(len([p for p in tr if p.endswith(".png")]), 3)
        rc = cubench.main(["report", *files, "--out", os.path.join(out, "report")])
        self.assertEqual(rc, 0)
        md = open(os.path.join(out, "report.md")).read()
        self.assertIn("DRY RUN", md)
        self.assertIn("| pandaos-desktop | generic | 100% |", md)
        shutil.rmtree(out)

    def test_smoke_dry_run_and_failures(self):
        out = tempfile.mkdtemp()
        rc = cubench.main(["run", "--dry-run", "--smoke", "--os", "pandaos-desktop", "--tools", "panda",
                           "--repeats", "1", "--tasks", "editor-new-file,files-*", "--out", out])
        self.assertEqual(rc, 0)
        d = json.load(open(os.path.join(out, "results-pandaos-desktop-panda.json")))
        self.assertEqual(d["meta"]["smokeFailures"], [])
        ids = {t["task"] for t in d["trials"]}
        self.assertIn("input-probe", ids)
        self.assertIn("editor-new-file", ids)
        self.assertTrue(any(i.startswith("files-") for i in ids))
        shutil.rmtree(out)
        ok = {"display": "1280x800"}
        crash = {"task": "editor-new-file", "passed": False, "reason": "crash", "error": "setup exited 1"}
        probe_fail = {"task": "input-probe", "passed": False, "reason": "wrong_result", "verifyOutput": "no wheel"}
        self.assertEqual(cubench.smoke_failures(ok, [{"task": "input-probe", "passed": True}], {"input-probe"}), [])
        self.assertEqual(len(cubench.smoke_failures({"display": "1024x768"}, [], set())), 1)
        self.assertEqual(len(cubench.smoke_failures(ok, [crash, probe_fail], {"input-probe"})), 2)
        self.assertIn("input-probe: did not run", cubench.smoke_failures(ok, [], {"input-probe"}))
        self.assertTrue(cubench.smoke_failures({**ok, "pandaCu": {"ok": False, "error": "x"}}, [], set()))

    def test_probecheck(self):
        sys.path.insert(0, os.path.join(HERE, "tasks", "assets"))
        import probecheck
        log = ["dpr=1|ev=load|h=800|w=1280", "b=0|ev=down|x=650|y=440", "b=0|ev=down|x=910.4|y=640",
               "dy=1|ev=wheel", "ev=keys|text=cubench probe 4", "ev=keys|text=cubench probe 42"]
        args = ((640, 400), (900, 600), "cubench probe 42")
        self.assertTrue(probecheck.check(log, *args)[0])  # offset (10, 40) everywhere: 1:1
        self.assertTrue(probecheck.check([line + "\n" for line in log], *args)[0])  # as read from the file
        self.assertFalse(probecheck.check(log[:2] + log[3:], *args)[0])  # one click
        skewed = log[:2] + ["b=0|ev=down|x=930|y=640"] + log[3:]
        self.assertFalse(probecheck.check(skewed, *args)[0])  # scaled pointer
        self.assertFalse(probecheck.check(log[:3] + log[4:], *args)[0])  # no wheel
        self.assertFalse(probecheck.check(log[:5], *args)[0])  # text cut short

    def test_qmp_wire_format(self):
        d = tempfile.mkdtemp()
        srv = qmp.FakeQMPServer(os.path.join(d, "s"))
        q = qmp.QMP(os.path.join(d, "s"))
        q.screendump(os.path.join(d, "a.png"))
        self.assertEqual(q.size, (1280, 800))
        n0 = len(srv.commands)
        q.click(1279, 0, "right")
        q.type("Hi")
        q.key("ctrl+s")
        q.scroll(10, 10, "up", 2)
        with self.assertRaises(ValueError):
            q.click(1280, 5)
        cmds = srv.commands[n0:]
        move = cmds[0]["arguments"]["events"]
        self.assertEqual(move[0]["data"], {"axis": "x", "value": 32767})
        self.assertEqual(move[1]["data"], {"axis": "y", "value": 0})
        self.assertEqual(cmds[1]["arguments"]["events"][0]["data"], {"down": True, "button": "right"})
        keys = [c["arguments"]["keys"] for c in cmds if c["execute"] == "send-key"]
        self.assertEqual([[k["data"] for k in ks] for ks in keys], [["shift", "h"], ["i"], ["ctrl", "s"]])
        wheel = [c for c in cmds if c["execute"] == "input-send-event"
                 and c["arguments"]["events"][0]["type"] == "btn" and "wheel" in c["arguments"]["events"][0]["data"]["button"]]
        self.assertEqual(len(wheel), 4)
        q.close()
        srv.close()
        shutil.rmtree(d)


if __name__ == "__main__":
    unittest.main(verbosity=1)
