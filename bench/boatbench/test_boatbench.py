#!/usr/bin/env python3
"""Unit tests: stats, verdicts, cost, report rendering, output parsing, and the
repeat runner against a fake driver. python3 bench/boatbench/test_boatbench.py"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import boatbench  # noqa: E402
import drivers  # noqa: E402
import report  # noqa: E402
import workloads as W  # noqa: E402


def result(driver, node, create, nproc=4, mem=8192, tool="fio", errors=None):
    reps = []
    for i, (n, c) in enumerate(zip(node, create)):
        reps.append({"index": i, "metrics": {"node_runs_s": n, "create_ready_s": c, "npm_ci_s": 30.0},
                     "probe": {"nproc": nproc, "mem_mib": mem, "disk_gib": 51, "cpu_model": "X"},
                     "disk_tool": tool, "errors": errors or {}, "node": "v22.22.3"})
    return {"driver": driver, "size": "default", "started": "2026-11-02T00:00:00Z", "repeats": reps,
            "host": {"desc": f"{driver} host"}, "client": {"cpu_model": "c", "egress": "FRA DE"}}


class Stats(unittest.TestCase):
    def test_percentile_interpolates(self):
        self.assertEqual(report.percentile([1, 2, 3, 4], 50), 2.5)
        self.assertEqual(report.percentile([10], 95), 10)
        self.assertAlmostEqual(report.percentile(list(range(1, 101)), 95), 95.05)
        self.assertIsNone(report.percentile([], 50))

    def test_summarize_median_iqr(self):
        s = report.summarize([5, 1, 3, None, 2, 4])
        self.assertEqual((s["n"], s["median"], s["q1"], s["q3"], s["min"], s["max"]), (5, 3, 2, 4, 1, 5))
        self.assertIsNone(report.summarize([None]))

    def test_samples_pool_lists_and_scalars(self):
        r = result("pandaos", [[10, 11, 12], [13, None, 14]], [1.0, None])
        self.assertEqual(report.samples(r, "node_runs_s"), [10, 11, 12, 13, 14])
        self.assertEqual(report.samples(r, "create_ready_s"), [1.0])


class Verdicts(unittest.TestCase):
    def s(self, med, q1, q3):
        return {"median": med, "q1": q1, "q3": q3, "n": 5}

    def test_higher_and_lower_is_better(self):
        self.assertEqual(report.verdict(self.s(20, 19, 21), self.s(10, 9, 11), True), "PandaOS wins")
        self.assertEqual(report.verdict(self.s(20, 19, 21), self.s(10, 9, 11), False), "**PandaOS loses**")

    def test_overlap_and_missing(self):
        self.assertIn("IQRs overlap", report.verdict(self.s(10, 8, 12), self.s(11, 9, 13), True))
        self.assertEqual(report.verdict(None, self.s(1, 1, 1), True), "only one side measured")
        self.assertEqual(report.ratio(self.s(20, 0, 0), self.s(10, 0, 0)), "2.00x")


class Cost(unittest.TestCase):
    def test_cost_from_price_and_time(self):
        summ = {"pandaos": {"node_runs_s": {"median": 20.0}, "npm_ci_s": {"median": 36.0}}}
        prices = {"pandaos": {"usd_per_hour": 0.036, "source": "x"}}
        (side, _, per_k, per_npm), = report.cost_rows(summ, prices)
        self.assertEqual(side, "pandaos")
        self.assertAlmostEqual(per_k, 0.036 / 3600 * 50)  # 1000 runs at 20 runs/s = 50 s
        self.assertAlmostEqual(per_npm, 0.036 / 100)

    def test_prices_file_optional_and_validated(self):
        self.assertIsNone(report.load_prices("/nonexistent/prices.json"))
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"pandaos": {"usd_per_hour": "cheap"}, "boatdev": {"usd_per_hour": 0.036}}, f)
        try:
            self.assertEqual(list(report.load_prices(f.name)), ["boatdev"])
        finally:
            os.unlink(f.name)


class Render(unittest.TestCase):
    def test_both_sides_with_losses_stated(self):
        md = report.render([result("pandaos", [[10]] * 3, [1, 2, 3]), result("boatdev", [[20]] * 3, [5, 6, 7])],
                           None, report.load_published())
        self.assertIn("**PandaOS loses**", md)  # node runs/s 10 vs 20
        self.assertIn("PandaOS wins", md)  # create 2 s vs 6 s
        self.assertIn("boat.dev PUBLISHED", md)
        self.assertIn("27.67", md)
        self.assertIn("NOT a dedicated server", md)
        self.assertIn("runs/s only", md)  # no prices
        self.assertIn("pandaos host", md)

    def test_single_side_and_auto_caveats(self):
        md = report.render([result("pandaos", [[10]], [1], nproc=2, tool="dd", errors={"net": "boom"}),
                            result("boatdev", [[9]], [1])])
        self.assertIn("2 vCPU", md)
        self.assertIn("Disk tool differs", md)
        self.assertIn("r0:net", md)

    def test_prices_add_cost_table(self):
        md = report.render([result("pandaos", [[20]], [1])], {"pandaos": {"usd_per_hour": 0.036, "source": "s"}})
        self.assertIn("USD per 1000 Node loop runs", md)
        self.assertIn("0.000500", md)


class Normalization(unittest.TestCase):
    def run_render(self, cpu="AMD EPYC 7763 64-Core Processor x4"):
        host = result("host", [[12.0]], [0.0])
        host["client"]["cpu_model"] = cpu
        host["repeats"][0]["metrics"]["rand_read_iops"] = 100000
        vm = result("pandaos", [[10.8]], [3.0])
        vm["repeats"][0]["metrics"]["rand_read_iops"] = 40000
        return report.render([host, vm], None, report.load_published())

    def test_host_vs_vm_ratio_and_projection_stay_labelled(self):
        md = self.run_render()
        self.assertIn("Runner host vs PandaOS VM (MEASURED", md)
        self.assertIn("| 0.900 |", md)  # node 10.8 / 12
        self.assertIn("| 0.400 |", md)  # rand read 40k / 100k
        self.assertIn("(NOT A MEASUREMENT)", md)
        self.assertIn("| 24.9 |", md)  # 27.67 x 0.9
        self.assertIn("| 25.9 |", md)  # 28.8 x 0.9
        self.assertIn("| 30.2 |", md)  # 12 x 28.8/10.3 x 0.9
        self.assertIn("| 20.3 |", md)  # 12 x 4727/2518 x 0.9
        self.assertIn("**20.3-30.2 runs/s**", md)
        measured = md.split("## Measured results")[1].split("## Published")[0]
        self.assertNotIn("host", measured.lower().replace("harness", ""))  # host is not a side

    def test_cpu_ratios_need_the_runner_cpu(self):
        md = self.run_render(cpu="Intel Xeon x4")
        self.assertNotIn("x CPU ratio (", md)
        self.assertIn("**24.9-25.9 runs/s**", md)

    def test_no_host_result_no_projection(self):
        md = report.render([result("pandaos", [[10]], [1])], None, report.load_published())
        self.assertNotIn("PROJECTION", md)
        self.assertIn("Ryzen 9 9950X", md)

    def test_published_says_the_vendor_score_is_a_vm(self):
        pub = report.load_published()
        self.assertIn("KVM guest", pub["note"])
        self.assertEqual(pub["rows"][0]["runs_s"], pub["metrics"]["node_runs_s"]["value"])


class Options(unittest.TestCase):
    def test_warm_pool_is_labelled_with_the_aslr_cost(self):
        warm = result("pandaos", [[10]], [1])
        warm["boatd_options"] = '{"warmPool": true}'
        md = report.render([warm], None, report.load_published())
        self.assertIn('{"warmPool": true}', md)
        self.assertIn("ASLR", md)
        self.assertNotIn("ASLR", report.render([result("pandaos", [[10]], [1])]))

    def test_published_disk_uses_the_pts_job_and_keeps_the_nvme_label(self):
        for key in ("pts_rand_read_iops", "pts_rand_write_iops", "pts_seq_write_mibps"):
            self.assertIn("cannot speak for NVMe", report.load_published()["metrics"][key]["note"])
        for flag in ("--iodepth=64", "--size=1g", "--startdelay=20", "--ramp_time=5", "--runtime=60"):
            self.assertIn(flag, W.DISK)
        r = result("pandaos", [[10]], [1])
        r["repeats"][0]["metrics"]["pts_rand_read_iops"] = 40000
        self.assertIn("40,000", report.render([r], None, report.load_published()).split("## Published")[1])


class Prices(unittest.TestCase):
    def test_vm_counts_follow_manager_admission_and_prices_recompute(self):
        with open(os.path.join(report.HERE, "prices.json")) as f:
            p = json.load(f)
        a = p["assumptions"]
        for s in p["servers"]:
            for key, oc in (("vms_overcommit_1_0", 1.0), ("vms_overcommit_1_5", 1.5)):
                n = int((s["ram_gb"] * 1024 - a["host_reserve_mib"]) * oc // a["vm_mem_mib"])
                self.assertEqual(s[key], min(n, int(s["vcpu_threads"] * a["cpu_overcommit_default"] // 4)), s["name"])
        ax = next(s for s in p["servers"] if s["name"] == "Hetzner AX102")
        month = ax["usd_per_month"] + ax["setup_usd"] / 12 + ax["backup_usd_per_month"]
        self.assertAlmostEqual(p["pandaos"]["usd_per_hour"], month / (15 * 730), places=4)
        self.assertAlmostEqual(p["pandaos"]["usd_per_hour_overcommit_1_5"], month / (23 * 730), places=4)


class HostDriver(unittest.TestCase):
    def test_runs_scripts_locally_with_chosen_home(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["BOATBENCH_HOST_DIR"] = d
            try:
                h = drivers.Host()
            finally:
                del os.environ["BOATBENCH_HOST_DIR"]
            rc, out, _ = h.exec(h.create("x"), 'echo "$HOME"; exit 3')
            self.assertEqual((rc, out.strip()), (3, d))
            self.assertEqual(h.run_true("host"), 0)

    def test_repeat_on_host_measures_a_and_d(self):
        class Fake(drivers.Host):
            def exec(self, vm, cmd, timeout=600):
                return FakeDriver().exec(vm, cmd, timeout)
        rec = boatbench.Repeat(Fake(), "abc", 0, set("ad")).run()
        self.assertEqual(rec["errors"], {})
        self.assertEqual(rec["metrics"]["node_runs_s"], [21.5] * W.PTS_PASSES)
        self.assertEqual(rec["metrics"]["rand_read_iops"], 50000)

    def test_host_driver_rejects_vm_only_workloads(self):
        with self.assertRaises(SystemExit):
            boatbench.main(["run", "--driver", "host", "--only", "ab"])


class Parsing(unittest.TestCase):
    def test_geomean(self):
        out = "  typescript: 35.87 runs/s\n-------------------------------------\nGeometric mean: 28.85 runs/s\n"
        self.assertEqual(W.parse_geomean(out), 28.85)
        with self.assertRaises(ValueError):
            W.parse_geomean("nothing")

    def test_bb_json_last_line_wins(self):
        self.assertEqual(W.parse_bb_json('noise\nBB_JSON {"a": 1}\nBB_JSON {"a": 2}\ntrailer'), {"a": 2})
        with self.assertRaises(ValueError):
            W.parse_bb_json("no json")

    def test_net_script_keeps_curl_format(self):
        self.assertIn("%{size_download} %{time_total}", W.NET)
        self.assertIn("dl.google.com", W.NET)
        self.assertIn("is not a throughput sample", W.NET)


class FakeDriver:
    name, size = "fake", "default"

    def __init__(self, fail_fork=False):
        self.n, self.live, self.snaps, self.fail_fork, self.files = 0, set(), set(), fail_fork, {}

    def _new(self):
        self.n += 1
        self.live.add(f"vm{self.n}")
        return f"vm{self.n}"

    def create(self, name):
        return self._new()

    def run_true(self, vm):
        return 0 if vm in self.live else None

    def exec(self, vm, cmd, timeout=600):
        if cmd == W.PROBE:
            return 0, 'BB_JSON {"nproc": 4, "mem_mib": 8000}', ""
        if cmd == W.NODE_SETUP:
            return 0, 'BB_JSON {"node": "v22.22.3"}', ""
        if cmd == W.NODE_RUN:
            return 0, "Geometric mean: 21.5 runs/s", ""
        if cmd == W.DISK:
            return 0, 'BB_JSON {"tool": "fio", "seq_read_mbps": 900, "rand_read_iops": 50000}', ""
        if cmd == W.NET:
            return 0, 'BB_JSON {"dl_go_tarball_googlecdn_mbit": 800}', ""
        if cmd == W.NPM_COLD:
            assert set(self.files) == {f"{W.NPM_SRC}/package.json", f"{W.NPM_SRC}/package-lock.json"}
            return 0, 'BB_JSON {"npm_ci_s": 31.5, "packages": 500}', ""
        return 0, "", ""

    def put_file(self, vm, path, data):
        self.files[path] = data

    def fork(self, vm, name):
        if self.fail_fork:
            raise RuntimeError("fork broke")
        return self._new()

    def stop(self, vm):
        self.live.discard(vm)

    def resume(self, vm):
        self.live.add(vm)

    def snapshot(self, vm, name):
        self.snaps.add(name)
        return name

    def from_snapshot(self, ref, name):
        return self._new()

    def delete(self, vm):
        self.live.discard(vm)

    def delete_snapshot(self, ref):
        self.snaps.discard(ref)


class Runner(unittest.TestCase):
    def test_repeat_measures_everything_and_cleans_up(self):
        d = FakeDriver()
        rec = boatbench.Repeat(d, "abc", 0, set("abdef")).run()
        m = rec["metrics"]
        self.assertEqual(rec["errors"], {})
        self.assertEqual(m["node_runs_s"], [21.5] * W.PTS_PASSES)
        for k in ("create_ready_s", "exec_rtt_p50_ms", "exec_rtt_p95_ms", "fork_ready_s", "stop_s",
                  "resume_ready_s", "snapshot_s", "from_snapshot_ready_s", "npm_ci_s", "seq_read_mbps"):
            self.assertIn(k, m)
        self.assertEqual(len(rec["exec_rtt_ms"]), boatbench.RTT_CALLS)
        self.assertEqual(rec["disk_tool"], "fio")
        self.assertEqual((d.live, d.snaps), (set(), set()))

    def test_failed_step_is_recorded_not_fatal(self):
        d = FakeDriver(fail_fork=True)
        rec = boatbench.Repeat(d, "abc", 1, set("b")).run()
        self.assertIn("fork", rec["errors"])
        self.assertIn("resume_ready_s", rec["metrics"])
        self.assertEqual(d.live, set())

    def test_concurrent_create(self):
        d = FakeDriver()
        wall, each = boatbench.concurrent_create(d, "abc", 0, n=10)
        self.assertEqual(len(each), 10)
        self.assertGreaterEqual(wall, each[-1])
        self.assertEqual(d.live, set())


if __name__ == "__main__":
    unittest.main()
