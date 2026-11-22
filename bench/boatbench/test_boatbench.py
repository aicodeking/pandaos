#!/usr/bin/env python3
"""Unit tests: stats, verdicts, cost, report rendering, output parsing, and the
repeat runner against a fake driver. python3 bench/boatbench/test_boatbench.py"""
import copy
import http.client
import json
import os
import socket
import struct
import sys
import tempfile
import threading
import unittest
import uuid
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import admission  # noqa: E402
import cost_tables  # noqa: E402
import boatbench  # noqa: E402
import density  # noqa: E402
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


class BenchmarkSemanticsTests(unittest.TestCase):
    def test_labels_do_not_claim_completed_jobs(self):
        md = report.render([result("pandaos", [20], [.1])])
        self.assertIn("geometric-mean index", md)
        self.assertNotIn("Node.js build/test loop", md)

    def test_completed_suites_use_complete_trial_pass_counts_and_wall_time(self):
        r = {"repeats": [{"density": {"levels": [
            {"n": 2, "vms_measured": 2, "complete": True, "pass_counts": [2, 2],
             "passes_required": 2, "per_vm_each": [10, 10], "per_vm_runs_s": 10,
             "host_runs_s": 20, "cpu_per_loop": 1, "wall_s": 40},
            {"n": 2, "vms_measured": 1, "complete": False, "wall_s": 1},
        ]}}]}
        levels = report.density_levels(r)
        self.assertAlmostEqual(levels[2]["completed_suites_s"]["median"], .1)
        del r["repeats"][0]["density"]["levels"][0]["pass_counts"]
        self.assertNotIn("completed_suites_s", report.density_levels(r)[2])
        r["repeats"][0]["density"]["passes"] = 2
        self.assertAlmostEqual(report.density_levels(r)[2]["completed_suites_s"]["median"], .1)
        r["repeats"][0]["density"]["errors"] = ["n=2: pass failed"]
        self.assertEqual(report.density_levels(r), {})


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
        self.assertEqual(report.samples(r, "node_runs_s"), [11, 13.5])
        self.assertEqual(report.samples(r, "create_ready_s"), [1.0])

    def test_node_runs_s_per_vm_median_prevents_pseudoreplication(self):
        r = result("pandaos", [[20.0, 21.0, 25.0], [22.0, 23.0, 24.0], [18.0, 19.0, 20.0]], [1.0, 1.0, 1.0])
        per_vm = report.samples(r, "node_runs_s")
        self.assertEqual(per_vm, [21.0, 23.0, 19.0])
        s = report.summarize(per_vm)
        self.assertEqual(s["n"], 3)
        self.assertEqual(s["median"], 21.0)


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


class ShapeCostTests(unittest.TestCase):
    def setUp(self):
        self.prices = report.load_prices(os.path.join(report.HERE, "prices.json"), "ax42")

    def test_boatdev_prices_recorded_sizes_and_suppresses_unknown(self):
        for size, rate in (("small", .018), ("default", .036), ("large", .072), ("xlarge", .2)):
            with self.subTest(size=size):
                r = result("boatdev", [20], [1])
                r["size"] = size
                md = report.render([r], self.prices)
                self.assertIn(f"| boatdev | {rate:.4f} |", md)
                self.assertIn(f"{rate / 3.6 / 20:.6f}", md)
                self.assertIn(size, md.split("## Cost")[1])
        for size in ("unsupported", None):
            r["size"] = size
            md = report.render([r], self.prices)
            self.assertIn("cost suppressed", md)
            self.assertNotIn("| boatdev |", md.split("## Cost")[1])

    def test_pandaos_other_shapes_do_not_use_default_admission(self):
        d = FakeDriver()
        d.name = "pandaos"
        trial, _ = density.Density(d, "fixture", (1,), lambda m: None, boatbench.gexec, boatbench.ready).run()
        r = result("pandaos", [20], [1])
        r["size"] = "large"
        r["repeats"][0]["density"] = trial
        md = report.render([r], self.prices)
        self.assertIn("cost suppressed", md)
        self.assertNotIn("PROJECTION: Hetzner", md)
        self.assertNotIn("boatd admits", md)
        self.assertNotIn("| pandaos |", md.split("## Cost")[1])

    def test_boatdev_density_uses_recorded_rate(self):
        d = FakeDriver()
        trial, _ = density.Density(d, "fixture", (1,), lambda m: None, boatbench.gexec, boatbench.ready).run()
        r = result("boatdev", [20], [1])
        r["size"] = "large"
        r["repeats"][0]["density"] = trial
        md = report.render([r], self.prices)
        self.assertIn("price $0.072/h", md)
        self.assertIn("0.002000 USD", md)
        r["size"] = "unsupported"
        self.assertNotIn("concurrent sandboxes (what each one delivers", report.render([r], self.prices))


class Render(unittest.TestCase):
    def test_both_sides_with_losses_stated(self):
        md = report.render([result("pandaos", [[10]] * 3, [1, 2, 3]), result("boatdev", [[20]] * 3, [5, 6, 7])],
                           None, report.load_published())
        self.assertIn("**PandaOS loses**", md)  # node runs/s 10 vs 20
        self.assertIn("PandaOS wins", md)  # create 2 s vs 6 s
        self.assertIn("boat.dev PUBLISHED", md)
        self.assertIn("27.67", md)
        self.assertIn("from a dedicated server", md)  # no CI run URL / runner CPU recorded
        self.assertNotIn("no `BOATDEV_API_KEY`", md)  # boat.dev was measured here
        self.assertIn("index units only", md)  # no prices
        self.assertIn("pandaos host", md)

    def test_single_side_and_auto_caveats(self):
        md = report.render([result("pandaos", [[10]], [1], nproc=2, tool="dd", errors={"net": "boom"}),
                            result("boatdev", [[9]], [1])])
        self.assertIn("2 vCPU", md)
        self.assertIn("Disk tool differs", md)
        self.assertIn("r0:net", md)

    def test_prices_add_cost_table(self):
        md = report.render([result("pandaos", [[20]], [1])], {"pandaos": {"usd_per_hour": 0.036, "source": "s"}})
        self.assertIn("USD per 1000 index units", md)
        self.assertIn("0.000500", md)


class ReportPooling(unittest.TestCase):
    def artifact(self, preset, timestamp):
        path = os.path.join(report.HERE, "results", "ax42-ubuntu-2026-11-11", preset,
                            f"results-pandaos-2026-11-11T{timestamp}Z.json")
        with open(path) as f:
            return json.load(f)

    def setUp(self):
        self.writeback = self.artifact("trusted-writeback", "110039")
        self.unsafe = self.artifact("trusted", "082254")

    def test_render_rejects_incompatible_real_artifacts_in_both_orders(self):
        self.assertEqual(report.samples(self.writeback, "fsync_rand_write_iops"), [38, 39, 38])
        self.assertEqual(report.samples(self.unsafe, "fsync_rand_write_iops"), [23872])
        for results in ([self.writeback, self.unsafe], [self.unsafe, self.writeback]):
            with self.subTest(first=results[0]["fc_cache_type"]):
                with self.assertRaisesRegex(ValueError, "incompatible.*fc_cache_type"):
                    report.render(results)

    def test_cache_mismatch_is_rejected_even_with_matching_preset_and_options(self):
        for cache in (self.unsafe["fc_cache_type"], None, "unknown (boatd source not found)"):
            other = copy.deepcopy(self.writeback)
            other["fc_cache_type"] = cache
            for render in (report.render, report.render_presets):
                for results in ([self.writeback, other], [other, self.writeback]):
                    with self.subTest(cache=cache, render=render.__name__, first=results[0]["fc_cache_type"]):
                        with self.assertRaisesRegex(ValueError, "incompatible.*fc_cache_type"):
                            render(results)

    def test_other_measurement_metadata_must_match(self):
        changes = {"driver": "host", "size": "small", "preset": "trusted-warm",
                   "boatd_options": '{"diskCache":"Writeback","warmPool":true}',
                   "host": {"desc": "another server or store"},
                   "client": dict(self.writeback["client"], cpu_model="another CPU"),
                   "source_commit": "another-revision"}
        for field, value in changes.items():
            other = dict(self.writeback, **{field: value})
            for results in ([self.writeback, other], [other, self.writeback]):
                with self.subTest(field=field, first=results[0].get(field)):
                    with self.assertRaisesRegex(ValueError, f"incompatible.*{field}"):
                        report.merged(results)

    def test_compatible_real_artifacts_pool_repeats(self):
        other = self.artifact("trusted", "084024")
        for results in ([self.unsafe, other], [other, self.unsafe]):
            with self.subTest(kind="repeat", first=results[0]["run_id"]):
                pooled = report.merged(results)
                self.assertEqual(pooled["repeats"], results[0]["repeats"] + results[1]["repeats"])
                self.assertEqual(report.samples(pooled, "fsync_rand_write_iops"),
                                 report.samples(results[0], "fsync_rand_write_iops")
                                 + report.samples(results[1], "fsync_rand_write_iops"))
                md = report.render(results)
                self.assertIn("not durable-write numbers", md)
                self.assertIn(f"| Repeats (fresh VMs) | {len(pooled['repeats'])} |", md)
        subset = self.artifact("trusted", "091516")
        for results in ([self.unsafe, subset], [subset, self.unsafe]):
            with self.subTest(kind="subset", first=results[0]["run_id"]):
                pooled = report.merged(results)
                self.assertEqual(pooled["repeats"], results[0]["repeats"] + results[1]["repeats"])
                self.assertEqual(report.samples(pooled, "fsync_rand_write_iops"), [23872])
                self.assertEqual(report.samples(pooled, "fork_ready_s"), report.samples(subset, "fork_ready_s"))
                md = report.render(results)
                self.assertIn("not durable-write numbers", md)
                self.assertIn("| Repeats (fresh VMs) | 1-3 |", md)

    def test_split_run_disjoint_repeats_rtt_propagation_and_disk_tool_filter(self):
        part1 = {
            "driver": "pandaos", "size": "default", "preset": "trusted", "fc_cache_type": "Unsafe",
            "host": {"desc": "ax42"}, "client": {"cpu_model": "Ryzen 7", "egress": "HEL FI"},
            "repeats": [
                {"index": i, "metrics": {"create_ready_s": 1.0, "api_rtt_p50_ms": 60.0, "fork_ready_s": 0.4,
                                         "node_runs_s": [20.0, 21.0, 22.0], "concurrent8_wall_s": 3.0},
                 "disk_tool": "-", "errors": {}}
                for i in range(3)
            ],
        }
        part2 = {
            "driver": "pandaos", "size": "default", "preset": "trusted", "fc_cache_type": "Unsafe",
            "host": {"desc": "ax42"}, "client": {"cpu_model": "Ryzen 7", "egress": "HEL FI"},
            "repeats": [
                {"index": i, "metrics": {"create_ready_s": 1.2, "seq_read_mbps": 9000.0, "fsync_rand_write_iops": 25000.0},
                 "disk_tool": "fio", "errors": {}}
                for i in range(3)
            ],
        }
        md = report.render([part1, part2])
        self.assertIn("| Repeats (fresh VMs) | 3 |", md)
        self.assertNotIn("Disk tool differs", md)
        pooled = report.merged([part1, part2])
        net_creates = report.samples(pooled, "create_ready_s_net")
        self.assertEqual(len(net_creates), 6)
        # part2 repeats lacked api_rtt_p50_ms, so they inherit 60.0 ms -> 1.2 - 2 * 0.060 = 1.08
        for v in net_creates[3:]:
            self.assertAlmostEqual(v, 1.08)

        # Overlapping workload 'a' across both parts produces a range "3-6".
        part2_overlap = copy.deepcopy(part2)
        for rep in part2_overlap["repeats"]:
            rep["metrics"]["node_runs_s"] = [19.0, 20.0, 21.0]
        self.assertIn("| Repeats (fresh VMs) | 3-6 |", report.render([part1, part2_overlap]))

    def test_run_metadata_and_equivalent_config_do_not_prevent_pooling(self):
        for original in (self.writeback, self.unsafe):
            other = copy.deepcopy(original)
            other.update(run_id="another-run", started="2026-11-12T00:00:00Z", run_url="https://example.com/run/2",
                         parts="abcdefg", fc_cache_type=original["fc_cache_type"].split(" ")[0])
            options = json.loads(other["boatd_options"])
            other["boatd_options"] = json.dumps(dict(reversed(list(options.items()))), indent=2)
            for results in ([original, other], [other, original]):
                with self.subTest(cache=original["fc_cache_type"], first=results[0]["run_id"]):
                    before = copy.deepcopy(results)
                    expected = report.samples(original, "fsync_rand_write_iops") * 2
                    self.assertEqual(report.samples(report.merged(results), "fsync_rand_write_iops"), expected)
                    for render in (report.render, report.render_presets):
                        self.assertIn(f"n={len(expected)}", render(results))
                    self.assertEqual(results, before)

    def test_host_side_and_type_strict_options_pooling_rejected(self):
        h1, h2 = result("host", [[12.0]], [0.0]), result("host", [[12.0]], [0.0])
        h2["host"] = {"desc": "different host store"}
        with self.assertRaisesRegex(ValueError, "incompatible.*host"):
            report.render([h1, h2])
        compatible = self.artifact("trusted", "084024")
        with self.assertRaisesRegex(ValueError, "incompatible.*fc_cache_type"):
            report.render([self.unsafe, compatible, self.writeback])
        bool_opt = dict(self.writeback, boatd_options='{"warmPool": true}')
        int_opt = dict(self.writeback, boatd_options='{"warmPool": 1}')
        with self.assertRaisesRegex(ValueError, "incompatible.*boatd_options"):
            report.merged([bool_opt, int_opt])


class CacheCpuPresets(unittest.TestCase):
    def test_unsafe_cache_marks_write_rows(self):
        r = result("pandaos", [[10]], [1])
        r["fc_cache_type"] = "Unsafe (boatd default diskCache)"
        r["repeats"][0]["metrics"].update(seq_write_mbps=900, seq_read_mbps=1000)
        md = report.render([r])
        self.assertIn("(d) disk seq write 1M †", md)
        self.assertNotIn("seq read 1M †", md)
        self.assertIn("not durable-write numbers", md)
        r["fc_cache_type"] = "Writeback (boatd.json diskCache)"
        self.assertNotIn("†", report.render([r]))

    def test_cost_labels_mixed_cpus_and_gives_the_consistent_figure(self):
        prices = {"pandaos": {"usd_per_hour": 0.0279, "cpu": "AMD Ryzen 9 7950X3D (Zen 4), Hetzner AX102"},
                  "boatdev": {"usd_per_hour": 0.036, "cpu": "AMD Ryzen 9 9950X (Zen 5)"}}
        r = result("pandaos", [[10]], [1])
        r["repeats"][0]["probe"]["cpu_model"] = "AMD EPYC 7763 64-Core Processor"
        md = report.render([r], prices, report.load_published())
        self.assertIn("(MIXED CPUs)", md)
        self.assertIn("**0.000356 USD per 1000 index units**", md)  # 0.036 / 3600 * 1000 / 28.06
        r["repeats"][0]["probe"]["cpu_model"] = "AMD Ryzen 9 7950X3D 16-Core Processor"
        cost = report.render([r], prices).split("## Cost")[1].split("## Caveats")[0]
        self.assertIn("| pandaos |", cost)
        self.assertNotIn("| pandaos | 0.0279 | AMD Ryzen 9 7950X3D (Zen 4), Hetzner AX102 | AMD Ryzen 9 7950X3D "
                         "16-Core Processor | 0.000776 (MIXED", cost)

    def test_presets_table_groups_pandaos_by_preset(self):
        a, b = result("pandaos", [[10]], [1.0]), result("pandaos", [[8]], [2.0])
        a["preset"], b["preset"] = "trusted", "untrusted"
        b["repeats"][0]["errors"] = {"concurrent": "boom"}
        md = report.render_presets([a, b, result("boatdev", [[20]], [5])])
        self.assertIn("| trusted | untrusted |", md)
        self.assertIn("| 10.00 [10.00-10.00] n=1 | 8.00 [8.00-8.00] n=1 |", md)
        self.assertIn("r0:concurrent", md)
        self.assertNotIn("20.00", md)


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
        self.assertIn("| 25.3 |", md)  # 28.06 x 0.9
        self.assertIn("| 25.9 |", md)  # 28.8 x 0.9
        self.assertIn("| 30.2 |", md)  # 12 x 28.8/10.3 x 0.9
        self.assertIn("| 20.3 |", md)  # 12 x 4727/2518 x 0.9
        self.assertIn("**20.3-30.2 index units**", md)
        measured = md.split("## Measured results")[1].split("## Published")[0]
        self.assertNotIn("host", measured.lower().replace("harness", ""))  # host is not a side

    def test_cpu_ratios_need_the_runner_cpu(self):
        md = self.run_render(cpu="Intel Xeon x4")
        self.assertNotIn("x CPU ratio (", md)
        self.assertIn("**25.3-25.9 index units**", md)

    def test_no_host_result_no_projection(self):
        md = report.render([result("pandaos", [[10]], [1])], None, report.load_published())
        self.assertNotIn("PROJECTION", md)
        self.assertIn("Ryzen 9 9950X", md)

    def test_published_is_the_october_starsling_run_with_august_kept_as_history(self):
        pub = report.load_published()
        self.assertEqual(pub["metrics"]["node_runs_s"]["value"], 28.06)
        self.assertIn("37073680030", pub["source"])
        hist = pub["historical"]["node_runs_s_2026_08_24"]
        self.assertEqual(hist["value"], 27.67)
        self.assertEqual(hist["rows"][0]["runs_s"], 27.67)
        self.assertIn("27.67", pub["note"])  # the superseded figure is dated, not dropped
        self.assertAlmostEqual(report.boatdev_ref({"boatdev": {"usd_per_hour": 0.036}}, pub), 0.036 / 3.6 / 28.06)

    def test_published_says_the_vendor_score_is_a_vm(self):
        pub = report.load_published()
        self.assertIn("KVM guest", pub["note"])
        self.assertEqual(pub["rows"][0]["runs_s"], pub["metrics"]["node_runs_s"]["value"])


class Context(unittest.TestCase):
    def test_server_results_get_server_caveats_not_runner_ones(self):
        r = result("pandaos", [[10]], [1])
        r["host"]["desc"] = "Hetzner AX42, native btrfs"
        r["source_commit"] = "abc1234"
        md = report.render([r], None, report.load_published())
        caveats = md.split("## Caveats")[1]
        for wrong in ("Azure", "GitHub-hosted runner", "loop file", "US runner"):
            self.assertNotIn(wrong, caveats)
        self.assertIn("from a dedicated server", caveats)
        self.assertIn("| Source commit (boat / PandaOS) | abc1234 |", md)
        self.assertIn("no `BOATDEV_API_KEY`", md)

    def test_runner_results_keep_runner_caveats(self):
        r = result("pandaos", [[10]], [1])
        r["run_url"] = "https://github.com/o/r/actions/runs/1"
        self.assertIn("Azure", report.render([r]).split("## Caveats")[1])

    def test_source_commit_from_env_else_unknown(self):
        env = dict(os.environ)
        try:
            os.environ.pop("GITHUB_SHA", None)
            os.environ["BOATBENCH_SOURCE_COMMIT"] = "deadbee"
            self.assertEqual(boatbench.source_commit(), "deadbee")
            del os.environ["BOATBENCH_SOURCE_COMMIT"]
            self.assertTrue(boatbench.source_commit())  # nixos-version's revision, or "unknown"
        finally:
            os.environ.clear()
            os.environ.update(env)


class Options(unittest.TestCase):
    def test_warm_pool_is_labelled_with_the_aslr_cost(self):
        warm = result("pandaos", [[10]], [1])
        warm["boatd_options"] = '{"warmPool": true}'
        md = report.render([warm], None, report.load_published())
        self.assertIn('{"warmPool": true}', md)
        self.assertIn("ASLR", md)
        self.assertNotIn("ASLR", report.render([result("pandaos", [[10]], [1])]))

    def test_server_unsafe_and_warm_pool_caveats_on_deployed_artifacts(self):
        # Trusted run with unknown fc_cache_type and warm create latency emits both Unsafe and warmPool caveats.
        r = result("pandaos", [[10]], [0.31])
        r["preset"] = "trusted"
        r["fc_cache_type"] = "unknown (boatd source not found)"
        r["repeats"][0]["metrics"]["fsync_rand_write_iops"] = 25000
        md = report.render([r])
        self.assertIn("†", md)
        self.assertIn("not durable-write numbers", md)
        self.assertIn("ASLR", md)

        # Explicit warm_pool == 0 suppresses the implicit cold-create warmPool heuristic; host warm_pool > 0 enables it.
        r_cold_fast = copy.deepcopy(r)
        r_cold_fast["host"]["warm_pool"] = 0
        self.assertNotIn("ASLR", report.render([r_cold_fast]))
        r_host_warm = result("pandaos", [[10]], [1.8])
        r_host_warm["host"]["warm_pool"] = 2
        self.assertIn("ASLR", report.render([r_host_warm]))

        # Untrusted with unknown fc_cache_type ALSO defaults to Unsafe in boatd and must emit the caveat;
        # only explicit Writeback / trusted-writeback suppresses it.
        r_untrusted = copy.deepcopy(r)
        r_untrusted["preset"] = "untrusted"
        md_untrusted = report.render([r_untrusted])
        self.assertIn("†", md_untrusted)
        self.assertIn("not durable-write numbers", md_untrusted)
        r_wb_preset = copy.deepcopy(r)
        r_wb_preset["preset"] = "trusted-writeback"
        self.assertNotIn("†", report.render([r_wb_preset]))
        r_wb = copy.deepcopy(r)
        r_wb["fc_cache_type"] = "Writeback"
        self.assertNotIn("†", report.render([r_wb]))

    def test_render_presets_preserves_unsafe_markers_footnote_and_disk_metrics(self):
        t = result("pandaos", [[21.0]], [1.4])
        t["preset"] = "trusted"
        t["fc_cache_type"] = "Unsafe (boatd default diskCache)"
        t["repeats"][0]["metrics"].update(
            rand_write_iops=148000, fsync_rand_write_iops=24000, pts_seq_write_mibps=1800,
            rand_read_iops=82000, rand_read_irq_cpu_iops=59000, rand_read_other_cpu_iops=219000,
        )
        u = copy.deepcopy(t)
        u["preset"] = "untrusted"
        u["fc_cache_type"] = "unknown (boatd source not found)"
        wb = copy.deepcopy(t)
        wb["preset"] = "trusted-writeback"
        wb["fc_cache_type"] = "Writeback (boatd.json diskCache)"
        wb["repeats"][0]["metrics"].update(
            rand_write_iops=143000, fsync_rand_write_iops=38, pts_seq_write_mibps=1700,
        )
        md = report.render_presets([t, u, wb])
        self.assertIn("(d) disk rand write 4k †", md)
        self.assertIn("(d) rand write 4k, fsync after every write, iodepth 1 †", md)
        self.assertIn("(d) PTS fio job: seq write 1M †", md)
        self.assertIn("(d) disk rand read 4k, fio unpinned", md)
        self.assertIn("(d) disk rand read 4k, fio pinned to the vCPU taking the virtio-blk IRQ", md)
        self.assertIn("(d) disk rand read 4k, fio pinned off that vCPU", md)
        self.assertIn("148,000 [148,000-148,000] n=1 †", md)
        self.assertIn("| 143,000 [143,000-143,000] n=1 |", md)  # trusted-writeback cell has no †
        self.assertIn("† PandaOS write numbers", md)
        self.assertIn("not durable-write numbers", md)

    def test_published_disk_uses_the_pts_job_and_keeps_the_nvme_label(self):
        for key in ("pts_rand_read_iops", "pts_rand_write_iops", "pts_seq_write_mibps"):
            self.assertIn("cannot speak for boat.dev's NVMe", report.load_published()["metrics"][key]["note"])
        for flag in ("--iodepth=64", "--size=1g", "--startdelay=20", "--ramp_time=5", "--runtime=60"):
            self.assertIn(flag, W.DISK)
        r = result("pandaos", [[10]], [1])
        r["repeats"][0]["metrics"]["pts_rand_read_iops"] = 40000
        self.assertIn("40,000", report.render([r], None, report.load_published()).split("## Published")[1])

    def test_rand_read_is_run_pinned_on_and_off_the_irq_vcpu(self):
        src = W.DISK.split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        ns = {}
        exec(src.split("def blk_irq_cpus", 1)[1].split("\nif out[", 1)[0].join(["import os\ndef blk_irq_cpus", ""]), ns)
        with tempfile.TemporaryDirectory() as t:
            f = os.path.join(t, "f")
            open(f, "w").close()
            st = os.stat(f)
            dev = os.path.join(t, "devices", "virtio1")
            os.makedirs(os.path.join(dev, "block", "vda", "vda1"))
            os.symlink("../../../virtio1", os.path.join(dev, "block", "vda", "device"))
            open(os.path.join(dev, "block", "vda", "vda1", "partition"), "w").close()
            os.makedirs(os.path.join(t, "dev", "block"))
            os.symlink(os.path.join(dev, "block", "vda", "vda1"),
                       os.path.join(t, "dev", "block", f"{os.major(st.st_dev)}:{os.minor(st.st_dev)}"))
            irq = os.path.join(t, "interrupts")
            with open(irq, "w") as fh:
                fh.write("  CPU0  CPU1  CPU2  CPU3\n"
                         " 26:  0  0  60  0  IO-APIC  5-edge  virtio0\n"
                         " 27:  9  7  0  80563  IO-APIC  6-edge  virtio1\n"
                         " 28:  6883  0  0  0  IO-APIC  7-edge  virtio2\n")
            self.assertEqual(ns["blk_irq_cpus"](f, t, irq), (3, 2))
            with open(irq, "w") as fh:  # no interrupt yet, or not virtio: no pinned runs
                fh.write("  CPU0  CPU1\n 27:  0  0  IO-APIC  6-edge  virtio1\n")
            self.assertEqual(ns["blk_irq_cpus"](f, t, irq), (None, None))
            self.assertEqual(ns["blk_irq_cpus"](f, os.path.join(t, "none"), irq), (None, None))
        self.assertIn('f"--cpus_allowed={cpu}"', W.DISK)
        r = result("pandaos", [[10]], [1])
        r["repeats"][0]["metrics"].update(rand_read_iops=60000, rand_read_irq_cpu_iops=59000,
                                          rand_read_other_cpu_iops=219000)
        md = report.render([r])
        for row in ("fio unpinned | IOPS | higher | 60,000", "virtio-blk IRQ | IOPS | higher | 59,000",
                    "off that vCPU | IOPS | higher | 219,000"):
            self.assertIn(row, md)
        self.assertNotIn("virtio-blk IRQ", report.render([result("pandaos", [[10]], [1])]))


class Prices(unittest.TestCase):
    """VM counts come from boatd's admission with its constants read from the Go source."""

    def setUp(self):
        with open(os.path.join(report.HERE, "prices.json")) as f:
            self.p = json.load(f)
        self.d = admission.boatd_defaults()

    def test_admission_constants_are_read_from_boatd(self):
        self.assertTrue(admission.admission_counts_overhead(), "manager.go no longer counts VMOverheadMiB: recheck")
        self.assertEqual(self.d["vm_mem_mib"], 8192)
        self.assertGreater(self.d["vm_overhead_mib"], 0)
        recorded = self.p["assumptions"]["boatd_defaults_read_from_source"]
        self.assertEqual(recorded, {k: self.d[k] for k in recorded}, "boatd defaults changed: rerun the cost model")

    def test_vm_counts_follow_boatd_admission(self):
        for s in self.p["servers"]:
            mt, sold = admission.est_memtotal_mib(s["ram_gb"]), s["ram_gb"] * 1024
            self.assertEqual(s["est_memtotal_mib"], mt, s["name"])
            for oc, key in ((1.0, "1_0"), (1.5, "1_5")):
                per_vm = self.d["vm_mem_mib"] + self.d["vm_overhead_mib"]
                cpu = int(s["vcpu_threads"] * self.d["cpu_overcommit"] // self.d["vm_vcpu"])
                self.assertEqual(s[f"vms_overcommit_{key}"],
                                 min(int((mt - self.d["host_reserve_mib"]) * oc // per_vm), cpu), s["name"])
                self.assertEqual(s[f"vms_as_sold_overcommit_{key}"],
                                 min(int((sold - self.d["host_reserve_mib"]) * oc // per_vm), cpu), s["name"])
            self.assertEqual(s["vms_untrusted"], admission.vms(s, 1.0, self.d, smt_off=True), s["name"])

    def test_ax102_threshold_rows(self):
        ax = next(s for s in self.p["servers"] if s["name"] == "Hetzner AX102")
        self.assertEqual((ax["vms_overcommit_1_0"], ax["vms_overcommit_1_5"], ax["vms_as_sold_overcommit_1_5"]), (15, 22, 23))
        # boatd admits n VMs only if MemTotal reaches this (manager.go's > is strict, so <= fits).
        self.assertEqual(admission.memtotal_needed(15, 1.0, self.d), 127744)
        self.assertEqual(admission.memtotal_needed(23, 1.5, self.d), 130560)
        self.assertEqual(admission.vms_by_ram(127744, 1.0, self.d), 15)
        self.assertEqual(admission.vms_by_ram(127743, 1.0, self.d), 14)

    def test_prices_recompute(self):
        ax = next(s for s in self.p["servers"] if s["key"] == "ax102")
        month = ax["usd_per_month"] + ax["setup_usd"] / 12 + ax["backup_usd_per_month"]
        pan = report.load_prices(os.path.join(report.HERE, "prices.json"))["pandaos"]  # default_server ax102
        self.assertAlmostEqual(pan["usd_per_hour"], month / (ax["vms_overcommit_1_0"] * 730), places=6)
        self.assertAlmostEqual(pan["usd_per_hour"], 0.0279, places=4)
        self.assertEqual(pan["density_model"]["untrusted"], {"vms": 15, "cpus": 16})
        self.assertEqual(pan["density_model"]["trusted"], {"vms": 22, "cpus": 32})
        self.assertIn("7950X3D", pan["cpu"])
        self.assertIn("9950X", self.p["boatdev"]["cpu"])

    def test_server_is_selectable(self):
        path = os.path.join(report.HERE, "prices.json")
        ax42 = report.load_prices(path, "ax42")["pandaos"]
        self.assertAlmostEqual(ax42["density_model"]["host_usd_per_hour"], 119.04 / 730, places=3)
        self.assertEqual(ax42["density_model"]["untrusted"], {"vms": 7, "cpus": 8})
        self.assertIn("8700GE", ax42["cpu"])
        self.assertEqual(len({s["key"] for s in self.p["servers"]}), len(self.p["servers"]))
        with self.assertRaises(ValueError):
            report.load_prices(path, "nope")

    def test_ai_proxy_fee_model(self):
        cfg = self.p["boatdev"]["ai_proxy"]
        self.assertEqual((cfg["usd_per_utc_day_on"], cfg["usd_per_1000_requests"]), (1.0, 1.0))
        self.assertIn("docs.boat.dev/ai-credentials.md", cfg["source"])
        hours = self.p["assumptions"]["hours_per_month"]
        days = hours / 24
        # proxy on, idle: $1 per UTC day only; 100k requests/day adds $100/day
        self.assertAlmostEqual(cost_tables.ai_proxy_monthly(cfg, 0, hours), days * 1.0)
        self.assertAlmostEqual(cost_tables.ai_proxy_monthly(cfg, 100_000, hours), days * 101.0)
        # the day fee is per account: it amortizes over the fleet; list rate stays 0.036 when the fee is 0
        self.assertAlmostEqual(cost_tables.effective_vm_hour(0.036, 0.0, 15, hours), 0.036)
        self.assertAlmostEqual(cost_tables.effective_vm_hour(0.036, days * 101.0, 15, hours), 0.036 + 101.0 / (15 * 24))
        self.assertEqual(cost_tables.hosts_for(100, 15), 7)
        # fee-free break-even matches the existing AX42 figure (3,307 VM-h of 5,110 = 65%)
        ax42 = next(s for s in self.p["servers"] if s["key"] == "ax42")
        m = cost_tables.monthly(ax42, self.p["assumptions"])
        self.assertAlmostEqual(cost_tables.breakeven_duty(m, 0.0, 7, 0.036, hours), 0.647, places=3)
        # the fee lowers the break-even duty cycle; once it exceeds the hosts, self-hosting wins at any duty
        with_fee = cost_tables.breakeven_duty(m, days * 3.0, 7, 0.036, hours)
        self.assertLess(with_fee, 0.647)
        self.assertLessEqual(cost_tables.breakeven_duty(m, days * 101.0, 7, 0.036, hours), 0)
        self.assertEqual(cost_tables._fmt_duty(-0.2), "any (fee alone > hosts)")
        self.assertEqual(cost_tables._fmt_duty(1.5), "never (150%)")
        self.assertEqual(cost_tables._fmt_duty(0.48), "48%")

    def test_ai_proxy_table_rows_and_render(self):
        rows = cost_tables.ai_proxy_table(self.p)
        self.assertEqual(len(rows), 2 + len(cost_tables.PROXY_REQ_PER_DAY) * len(cost_tables.PROXY_FLEETS))
        self.assertIn("| 0 | 7 | $30.42 | $0.042 | 48% (1 host) | never (150%) (1 host) |", rows)
        self.assertIn("| 2,000 | 15 | $91.25 | $0.044 | 67% (3 hosts) | 54% (1 host) |", rows)
        self.assertIn("| 100,000 | 15 | $3,072.08 | $0.317 | any (fee alone > hosts) (3 hosts) | any (fee alone > hosts) (1 host) |", rows)
        md = cost_tables.render(self.p)
        self.assertIn("### boat.dev AI-proxy fee sensitivity", md)
        for r in rows:
            self.assertIn(r, md)
        no_proxy = json.loads(json.dumps(self.p))
        del no_proxy["boatdev"]["ai_proxy"]
        self.assertEqual(cost_tables.ai_proxy_table(no_proxy), [])
        self.assertNotIn("AI-proxy fee", cost_tables.render(no_proxy))

    def test_cluster_archive_is_shared_across_serving_hosts(self):
        a = self.p["assumptions"]
        topologies = a["ha_and_backup"]["topologies"]
        for server in self.p["servers"]:
            hardware = server["usd_per_month"] + server["setup_usd"] / 12
            single, cluster, pair = [cost_tables.topology_monthly_tco(server, a, t) for t in topologies]
            self.assertAlmostEqual(single, hardware + server["backup_usd_per_month"])
            self.assertAlmostEqual(cluster * 4, hardware * 5 + 14.784)
            self.assertAlmostEqual(pair, hardware * 2 + 14.784)
        ax42 = self.p["servers"][0]
        per_serving = cost_tables.topology_monthly_tco(ax42, a, topologies[1])
        self.assertAlmostEqual(per_serving, 148.01266666666666)
        self.assertAlmostEqual(per_serving / (6 * 730 * .036), .9386901741755454)
        md = "\n".join(cost_tables.retention_table(self.p, ax42))
        self.assertIn("raw-capacity sparse", md)
        self.assertIn("full-disk reserve", md)

    def test_cost_tables(self):
        md = cost_tables.render(self.p)
        self.assertIn("| Hetzner AX102, conservative / untrusted (15) | $0.279 | $0.112 | $0.056 | $0.028 | $0.042 |", md)
        # AX42 models both 7 RAM-bound sparse VMs ($0.023) and 6 worst-case 100%-full 59 GiB disk VMs ($0.027).
        self.assertIn("| Hetzner AX42, conservative / untrusted (7) | $0.233 | $0.093 | $0.047 | $0.023 | $0.053 |", md)
        self.assertIn(
            "| Hetzner AX42, untrusted full-disk worst-case, 100%-full 59 GiB disks (6) | $0.272 | $0.109 | $0.054 | $0.027 | $0.061 |",
            md,
        )
        self.assertIn("Hetzner AX42 full-disk worst-case (6)", md)
        self.assertIn("424, $0.720/h", md)  # AX102 usable 1,519 GiB (25 slots), 8 h sessions kept 14 days
        self.assertIn("MODEL, thin: 3.4 GiB", md)
        self.assertIn("$0.0288/h", md)  # boat.dev max +25% credit bonus
        self.assertIn("4+1 N+1 cluster", md)
        self.assertIn("single-host daemons", md)
        self.assertIn("+ op ($150/mo) 100% $/VM-h", md)
        # Verify multi-host TCO does not double-count single-host backup on top of the $14.784 5 TB BX21 box.
        self.assertIn("| Hetzner AX42 (7) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $148.01 |", md)
        self.assertIn("| Hetzner AX42 (7) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $245.69 |", md)
        self.assertIn("| Hetzner AX102 (15) | 4+1 N+1 cluster + 5 TB off-host backup (1.25x) | $381.35 |", md)
        self.assertIn("| Hetzner AX102 (15) | 1+1 HA pair + 5 TB off-host backup (2.00x) | $619.02 |", md)
        # Verify boat.dev plan tiers match $20, $100, $500, $2,000 with no conflicting $200 Startup plan.
        plan_fees = [pl["monthly_fee_usd"] for pl in self.p["boatdev"]["plans"] if pl["monthly_fee_usd"] > 0]
        self.assertEqual(plan_fees, [20, 100, 500, 2000])
        self.assertNotIn("Startup", md)
        self.assertEqual(cost_tables.capacity(15, 25, 24, 336, 730), 25 * 730 * 24 / 360)
        # All 5 servers must carry complete storage fields and render a retention table.
        self.assertEqual(len(self.p["servers"]), 5)
        for s in self.p["servers"]:
            for k in ("raw_nvme_gb", "usable_nvme_gb", "store_gib", "usable_store_gib", "store_source"):
                self.assertIn(k, s, f"missing {k} on {s['key']}")
            self.assertEqual(
                cost_tables.usable_store_gib(s, self.p["assumptions"]),
                int(s["store_gib"] * self.p["assumptions"]["storage_reserve"]["usable_disk_factor"]),
            )
            self.assertIn(f"### Stopped-VM retention: {s['name']}", md)


class Presets(unittest.TestCase):
    def test_untrusted_preset_is_module_nix_untrusted_settings(self):
        with open(os.path.join(report.HERE, "presets.json")) as f:
            presets = json.load(f)
        nix = admission.untrusted_settings()
        self.assertNotIn("_untrusted_not_applied", presets)
        self.assertEqual(presets["untrusted"], nix)
        self.assertEqual(presets["untrusted"]["memOvercommit"], 1)
        self.assertEqual(presets["trusted"], {})
        self.assertEqual(presets["trusted-warm"], {"warmPool": True})

    def test_cache_type_from_options_then_boatd(self):
        self.assertTrue(boatbench.fc_cache_type('{"diskCache": "Writeback"}').startswith("Writeback"))
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "fc.go"), "w") as f:
                f.write('drive := map[string]any{"drive_id": "rootfs"}')
            with open(os.path.join(d, "config.go"), "w") as f:
                f.write("")
            self.assertTrue(boatbench.fc_cache_type("{}", d).startswith("Unsafe (Firecracker's default"))
            with open(os.path.join(d, "fc.go"), "w") as f:
                f.write('"cache_type": cfg.DiskCache')
            with open(os.path.join(d, "config.go"), "w") as f:
                f.write('\t\tDiskCache:         "Writeback",\n')
            self.assertEqual(boatbench.fc_cache_type("-", d), "Writeback (boatd default diskCache)")

    def test_deployed_etc_boatd_json_cache_and_warm_pool(self):
        self.assertEqual(boatbench._cache_from_boatd_json({}), "Unsafe")
        self.assertEqual(boatbench._cache_from_boatd_json({"diskCache": "writeback"}), "Writeback")
        self.assertEqual(boatbench._cache_from_boatd_json({"diskCache": "Writeback"}), "Writeback")
        self.assertEqual(boatbench._cache_from_boatd_json({"unsafeCache": False}), "Writeback")
        self.assertEqual(boatbench._cache_from_boatd_json({"unsafeCache": True}), "Unsafe")
        with tempfile.TemporaryDirectory() as d:
            missing_src = os.path.join(d, "no-boatd-src")
            cfg_path = os.path.join(d, "boatd.json")
            self.assertEqual(boatbench.fc_cache_type("-", missing_src, config_path=cfg_path),
                             "unknown (boatd source not found)")
            with open(cfg_path, "w") as f:
                json.dump({"warmPool": 2}, f)
            self.assertEqual(boatbench.fc_cache_type("-", missing_src, config_path=cfg_path), "Unsafe")
            self.assertEqual(boatbench.host_metadata("ax42", "-", config_path=cfg_path),
                             {"desc": "ax42", "warm_pool": 2})
            self.assertEqual(boatbench.host_metadata("ax42", '{"warmPool": false}', config_path=cfg_path),
                             {"desc": "ax42", "warm_pool": 0})
            with open(cfg_path, "w") as f:
                json.dump({"unsafeCache": True}, f)
            self.assertEqual(boatbench.fc_cache_type("-", missing_src, config_path=cfg_path), "Unsafe")
            self.assertEqual(boatbench.host_metadata("ax42", "-", config_path=cfg_path),
                             {"desc": "ax42", "warm_pool": 0})
            with open(cfg_path, "w") as f:
                json.dump({"unsafeCache": False, "warmPool": True}, f)
            with mock.patch.dict(os.environ, {"BOATD_CONFIG": cfg_path}):
                self.assertEqual(boatbench.fc_cache_type("-", missing_src), "Writeback")
                self.assertEqual(boatbench.host_metadata("ax42", "-")["warm_pool"], 1)


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

    ROUND_TRIPS = {"create_ready_s": 2, "stop_s": 1, "exec_rtt_p50_ms": 1}

    def __init__(self, fail_fork=False, max_vms=None):
        self.n, self.live, self.snaps, self.fail_fork, self.files = 0, set(), set(), fail_fork, {}
        self.max_vms, self.pings, self.setups = max_vms, 0, 0

    def _new(self):
        self.n += 1
        self.live.add(f"vm{self.n}")
        return f"vm{self.n}"

    def create(self, name):
        if self.max_vms is not None and len(self.live) >= self.max_vms:
            raise RuntimeError("503 host_capacity")
        return self._new()

    def ping(self, vm):
        self.pings += 1

    def run_true(self, vm):
        return 0 if vm in self.live else None

    def exec(self, vm, cmd, timeout=600):
        if cmd == W.PROBE:
            return 0, 'BB_JSON {"nproc": 4, "mem_mib": 8000}', ""
        if cmd == W.NODE_SETUP:
            self.setups += 1
            return 0, 'BB_JSON {"node": "v22.22.3"}', ""
        if cmd == density.RUN:
            return 0, "Geometric mean: 10.0 runs/s\nBB_CPU 20.0 22.0 2.0", ""
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

    def snapshot(self, vm, name, on_accepted=None):
        self.snaps.add(name)
        if on_accepted:
            on_accepted(name)
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
        self.assertEqual(len(rec["api_rtt_ms"]), boatbench.API_RTT_CALLS)
        self.assertEqual(d.pings, boatbench.API_RTT_CALLS)
        self.assertIn("api_rtt_p50_ms", m)

    def test_add_net_subtracts_round_trips_without_mutating(self):
        m = {
            "api_rtt_p50_ms": 100.0,
            "create_ready_s": 2.0,
            "stop_s": 0.5,
            "exec_rtt_p50_ms": 150.0,
            "exec_rtt_p95_ms": 225.0,
        }
        out = boatbench.add_net(m, FakeDriver.ROUND_TRIPS)
        self.assertAlmostEqual(out["create_ready_s_net"], 1.8)
        self.assertAlmostEqual(out["stop_s_net"], 0.4)
        self.assertAlmostEqual(out["exec_rtt_p50_ms_net"], 50.0)
        self.assertAlmostEqual(out["exec_rtt_p50_net_ms"], 50.0)
        self.assertAlmostEqual(out["exec_rtt_p95_ms_net"], 125.0)
        self.assertAlmostEqual(out["exec_rtt_p95_net_ms"], 125.0)
        clamped = boatbench.add_net({"api_rtt_p50_ms": 100.0, "exec_rtt_p95_ms": 80.0}, FakeDriver.ROUND_TRIPS)
        self.assertEqual(clamped["exec_rtt_p95_net_ms"], 0.0)
        self.assertEqual(clamped["exec_rtt_p95_ms_net"], 0.0)
        self.assertNotIn("create_ready_s_net", m)
        self.assertEqual(boatbench.add_net({"stop_s": 1}, FakeDriver.ROUND_TRIPS), {"stop_s": 1})

        # Recorded actual api_calls override heuristic ROUND_TRIPS in both add_net and report.samples.
        out_actual = boatbench.add_net(m, FakeDriver.ROUND_TRIPS, {"create_ready_s": 5, "stop_s": 3})
        self.assertAlmostEqual(out_actual["create_ready_s_net"], 1.5)
        self.assertAlmostEqual(out_actual["stop_s_net"], 0.2)
        r = {
            "driver": "boatdev",
            "size": "default",
            "started": "2026-11-02T00:00:00Z",
            "host": {"desc": "boatdev host"},
            "client": {"cpu_model": "c", "egress": "FRA DE"},
            "repeats": [{"index": 0, "metrics": {"api_rtt_p50_ms": 50.0, "create_ready_s": 2.0, "stop_s": 1.0,
                                                 "exec_rtt_p50_ms": 70.0, "exec_rtt_p95_ms": 110.0},
                         "api_calls": {"create_ready_s": 6, "stop_s": 4}}],
        }
        self.assertAlmostEqual(report.samples(r, "create_ready_s_net")[0], 1.7)
        self.assertAlmostEqual(report.samples(r, "stop_s_net")[0], 0.8)
        self.assertAlmostEqual(report.samples(r, "exec_rtt_p95_net_ms")[0], 60.0)
        self.assertAlmostEqual(report.samples(r, "exec_rtt_p95_ms_net")[0], 60.0)
        rendered = report.render([r])
        self.assertIn("(b) exec round trip `true`, p50 of 50, NET of API round trips", rendered)
        self.assertIn("(b) exec round trip `true`, p95 of 50, NET of API round trips", rendered)

    def test_serial_poll_rtt_accounting_and_non_negative_clamping(self):
        """Multiple poll=True retries during wait_exec_ready and BoatDev._poll count only 1 serial
        completion RTT in api_calls while incrementing diagnostic poll_http_calls and http_calls;
        both add_net and report.samples clamp *_net >= 0.0 and ignore poll_http_calls."""
        responses = [
            # create + 4 run_true polls (3 x 409, 1 x 0)
            {"sandbox": {"id": "bx_23456789", "state": "provisioning"}},
            drivers.ApiError(409, '{"code":"boat_starting"}', "POST /commands"),
            drivers.ApiError(409, '{"code":"boat_starting"}', "POST /commands"),
            drivers.ApiError(409, '{"code":"boat_starting"}', "POST /commands"),
            {"exitCode": 0, "stdout": "", "stderr": ""},
            # stop + 5 GET polls (4 x archiving, 1 x archived)
            {"id": "bx_23456789", "status": "archiving"},
            {"sandbox": {"id": "bx_23456789", "state": "archiving", "stop": {"status": "pending"}}},
            {"sandbox": {"id": "bx_23456789", "state": "archiving", "stop": {"status": "pending"}}},
            {"sandbox": {"id": "bx_23456789", "state": "archiving", "stop": {"status": "pending"}}},
            {"sandbox": {"id": "bx_23456789", "state": "archiving", "stop": {"status": "pending"}}},
            {"sandbox": {"id": "bx_23456789", "state": "archived", "stop": {"status": "completed"}}},
            # snapshot + 4 GET polls (3 x saving, 1 x ready)
            {"status": "saving", "snapshot": {"name": "bb-snap", "status": "saving"}},
            {"snapshot": {"name": "bb-snap", "status": "saving"}},
            {"snapshot": {"name": "bb-snap", "status": "saving"}},
            {"snapshot": {"name": "bb-snap", "status": "saving"}},
            {"snapshot": {"name": "bb-snap", "status": "ready"}},
        ]

        def fake_http(method, url, key, body=None, raw=None, timeout=660, ctype="application/json",
                      headers=None, poll=False):
            nxt = responses.pop(0)
            if isinstance(nxt, Exception):
                raise nxt
            return json.dumps(nxt)

        with mock.patch.object(drivers, "_http", fake_http), mock.patch.object(drivers, "POLL_S", 0):
            d = drivers.BoatDev(key="boat_test")
            r = boatbench.Repeat(d, "abc", 0, set())
            vm, _ = r.new(lambda: d.create("bb-a"), "create_ready_s")
            self.assertEqual(vm, "bx_23456789")
            self.assertEqual(r.rec["api_calls"]["create_ready_s"], 2)  # 1 POST + 1 final readiness poll
            self.assertEqual((d.http_calls, d.poll_http_calls, d.thread_calls()), (5, 4, 2))

            c_stop = boatbench._op_calls(d)
            d.stop(vm)
            r._record_calls("stop_s", c_stop)
            self.assertEqual(r.rec["api_calls"]["stop_s"], 2)  # 1 POST + 1 final completion GET
            self.assertEqual((d.http_calls, d.poll_http_calls, d.thread_calls()), (11, 9, 4))

            c_snap = boatbench._op_calls(d)
            self.assertEqual(d.snapshot(vm, "bb-snap"), "bb-snap")
            r._record_calls("snapshot_s", c_snap)
            self.assertEqual(r.rec["api_calls"]["snapshot_s"], 2)  # 1 POST + 1 final completion GET
            self.assertEqual((d.http_calls, d.poll_http_calls, d.thread_calls()), (16, 13, 6))

        # Clamp to >= 0.0 when rtt_ms * calls > wall_ms in both add_net and report.samples.
        jitter_metrics = {
            "api_rtt_p50_ms": 95.0,
            "create_ready_s": 0.12,
            "stop_s": 0.14,
            "snapshot_s": 0.10,
            "exec_rtt_p50_ms": 80.0,
            "exec_rtt_p95_ms": 90.0,
        }
        clamped_net = boatbench.add_net(
            jitter_metrics,
            drivers.BoatDev.ROUND_TRIPS,
            {"create_ready_s": 2, "stop_s": 2, "snapshot_s": 2},
        )
        for key in ("create_ready_s_net", "stop_s_net", "snapshot_s_net",
                    "exec_rtt_p50_ms_net", "exec_rtt_p95_ms_net"):
            self.assertEqual(clamped_net[key], 0.0, key)

        # report.samples ignores poll_http_calls when api_calls / *_calls are absent and clamps >= 0.0.
        legacy_with_poll_diag = {
            "driver": "boatdev",
            "size": "default",
            "started": "2026-11-15T00:00:00Z",
            "host": {"desc": "boatdev host"},
            "client": {"cpu_model": "c", "egress": "FRA DE"},
            "repeats": [
                {
                    "index": 0,
                    "metrics": {"api_rtt_p50_ms": 90.0, "stop_s": 0.50, "snapshot_s": 0.12},
                    "poll_http_calls": {"stop_s": 12, "snapshot_s": 10},
                },
                {
                    "index": 1,
                    "metrics": {"api_rtt_p50_ms": 90.0, "stop_s": 0.50, "stop_s_calls": 3, "snapshot_s": 0.50},
                    "poll_http_calls": 25,
                },
            ],
        }
        # Repeat 0 ignores poll_http_calls["stop_s"]=12 and uses BoatDev.ROUND_TRIPS["stop_s"]=2 -> 0.50 - 0.18 = 0.32;
        # Repeat 1 uses explicit stop_s_calls=3 -> 0.50 - 0.27 = 0.23.
        stop_samples = report.samples(legacy_with_poll_diag, "stop_s_net")
        self.assertAlmostEqual(stop_samples[0], 0.32)
        self.assertAlmostEqual(stop_samples[1], 0.23)
        # Repeat 0 snapshot_s=0.12 - 2*0.09 = -0.06 -> clamped to 0.0; Repeat 1 -> 0.32.
        snap_samples = report.samples(legacy_with_poll_diag, "snapshot_s_net")
        self.assertEqual(snap_samples[0], 0.0)
        self.assertAlmostEqual(snap_samples[1], 0.32)

    def test_drivers_declare_round_trips_and_ping(self):
        for cls in (drivers.PandaOS, drivers.BoatDev):
            self.assertTrue(callable(getattr(cls, "ping")))
            self.assertTrue(set(cls.ROUND_TRIPS) <= {k for k in report.NET_KEYS})
        self.assertEqual(drivers.BoatDev.ROUND_TRIPS["stop_s"], 2)  # POST + at least one GET poll

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


class CleanupTests(unittest.TestCase):
    def setUp(self):
        self.d = FakeDriver()
        self.d.live.add("unowned")
        patch = mock.patch.object(self.d, "delete", wraps=self.d.delete)
        self.delete = patch.start()
        self.addCleanup(patch.stop)

    def assert_cleaned(self):
        self.assertEqual(self.d.live, {"unowned"})
        self.assertCountEqual(self.delete.call_args_list,
                              [mock.call(f"vm{i}") for i in range(1, self.d.n + 1)])
        self.assertEqual(self.d.snaps, set())

    @staticmethod
    def partly_ready(d, vm, t0):
        if vm != "vm1":
            raise TimeoutError("readiness broke")
        return 0.1

    def test_rejected_snapshot_does_not_register_a_resource(self):
        d = drivers.BoatDev(base="http://fixture.invalid", key="fixture")
        r = boatbench.Repeat(d, "abc", 0, set())
        with mock.patch.object(d, "_j", side_effect=RuntimeError("snapshot rejected")), \
                mock.patch.object(d, "delete_snapshot") as delete_snapshot:
            r.step("snapshot", lambda: r.snapshot("owned-vm"))
            r.cleanup()
        self.assertEqual(r.snaps, [])
        delete_snapshot.assert_not_called()
        self.assertIn("snapshot rejected", r.rec["errors"]["snapshot"])

    def test_remote_snapshot_timeout_cleans_accepted_ref(self):
        d = drivers.BoatDev(base="http://fixture.invalid", key="fixture")
        r = boatbench.Repeat(d, "abc", 0, set())
        with mock.patch.object(d, "_j", return_value={"snapshot": {"status": "saving"}}) as request, \
                mock.patch.object(drivers.time, "monotonic", side_effect=[0, 0, 1, 901]), \
                mock.patch.object(drivers.time, "sleep"):
            r.step("snapshot", lambda: r.snapshot("owned-vm"))
            r.cleanup()
        self.assertIn("TimeoutError", r.rec["errors"]["snapshot"])
        self.assertEqual(request.call_args_list, [
            mock.call("POST", "/saved-snapshots", {"sandboxId": "owned-vm", "name": "bb-abc-0-snap"}),
            mock.call("GET", "/saved-snapshots/bb-abc-0-snap", poll=True),
            mock.call("DELETE", "/saved-snapshots/bb-abc-0-snap")])
        self.assertEqual(r.snaps, [])
        self.assertNotIn("snapshot_s", r.rec["metrics"])

    def test_cleanup_failure_is_recorded_and_successful_retry_clears_it(self):
        r = boatbench.Repeat(self.d, "abc", 0, set())
        vm = self.d.create("owned")
        ref = self.d.snapshot(vm, "owned-snap")
        r.vms.append(vm)
        r.snaps.append(ref)
        with mock.patch.object(self.d, "delete", side_effect=RuntimeError("VM cleanup failed")), \
                mock.patch.object(self.d, "delete_snapshot", side_effect=RuntimeError("snapshot cleanup failed")):
            r.cleanup()
        self.assertEqual(r.rec["errors"][f"cleanup_vm:{vm}"], "RuntimeError: VM cleanup failed")
        self.assertEqual(r.rec["errors"][f"cleanup_snapshot:{ref}"], "RuntimeError: snapshot cleanup failed")
        r.cleanup()
        self.assertEqual(r.rec["errors"], {})
        self.assertEqual(r.vms, [])
        self.assertEqual(r.snaps, [])

    def test_density_cleanup_preserves_failed_ids_and_allows_retry(self):
        trial = density.Density(self.d, "fixture", (1, 2), lambda m: None, boatbench.gexec, boatbench.ready)
        original = self.d.delete
        def delete(vm):
            if vm == "vm2":
                raise RuntimeError("fixture deletion failed")
            original(vm)
        with mock.patch.object(self.d, "delete", side_effect=delete) as deletion:
            rec, _ = trial.run()
        self.assertEqual(rec["cleanup_pending_vms"], ["vm2"])
        self.assertEqual(rec["cleanup_errors"], {"vm2": "RuntimeError: fixture deletion failed"})
        self.assertEqual(deletion.call_args_list.count(mock.call("vm1")), 1)
        self.assertEqual(deletion.call_args_list.count(mock.call("vm2")), 2)
        md = report.render([{"driver": "pandaos", "size": "default", "repeats": [{"density": rec}]}])
        self.assertIn("unresolved VM cleanup", md)
        self.assertIn("vm2", md)
        trial.cleanup()
        self.assertEqual(trial.cleanup_pending_vms, [])
        self.assertEqual(trial.cleanup_errors, {})
        self.assertEqual(self.d.live, {"unowned"})

    def test_concurrent_cleanup_records_unresolved_ids_even_after_readiness_failure(self):
        cleanup = {}
        with mock.patch.object(self.d, "delete", side_effect=RuntimeError("fixture deletion failed")), \
                mock.patch.object(boatbench, "wait_exec_ready", side_effect=TimeoutError("readiness failed")):
            with self.assertRaisesRegex(RuntimeError, "vm1"):
                boatbench.concurrent_create(self.d, "fixture", 0, n=2, cleanup_out=cleanup)
        self.assertCountEqual(cleanup["pending_vms"], ["vm1", "vm2"])
        self.assertEqual(set(cleanup["errors"]), {"vm1", "vm2"})
        self.assertEqual(self.d.live, {"unowned", "vm1", "vm2"})

    def test_concurrent_cleanup_retries_only_failed_owned_vms(self):
        original = self.d.delete
        failed = set()
        def transient(vm):
            if vm == "vm1" and vm not in failed:
                failed.add(vm)
                raise RuntimeError("transient delete failure")
            original(vm)
        cleanup = {}
        with mock.patch.object(self.d, "delete", side_effect=transient) as deletion:
            _, each = boatbench.concurrent_create(self.d, "fixture", 0, n=2, cleanup_out=cleanup)
        self.assertEqual(len(each), 2)
        self.assertEqual(cleanup, {"pending_vms": [], "errors": {}})
        self.assertEqual(deletion.call_args_list.count(mock.call("vm1")), 2)
        self.assertEqual(deletion.call_args_list.count(mock.call("vm2")), 1)
        self.assertEqual(self.d.live, {"unowned"})

    def test_command_persists_cleanup_failures_and_returns_failure(self):
        for part, field in (("c", "concurrent_cleanup"), ("g", "density_cleanup")):
            with self.subTest(part=part), tempfile.TemporaryDirectory() as outdir:
                d = FakeDriver()
                d.name = "pandaos"
                rec = {"index": 0, "metrics": {}, "errors": {}}
                with mock.patch.dict(drivers.DRIVERS, {"pandaos": lambda: d}), \
                        mock.patch.object(boatbench.Repeat, "run", return_value=rec), \
                        mock.patch.object(boatbench, "client_info", return_value={}), \
                        mock.patch.object(boatbench, "source_commit", return_value="fixture"), \
                        mock.patch.object(boatbench, "host_metadata", return_value={"desc": "fixture"}), \
                        mock.patch.dict(os.environ, {"BOATBENCH_CONCURRENT_N": "2", "BOATBENCH_DENSITY_LEVELS": "1,2"}), \
                        mock.patch.object(d, "delete", side_effect=RuntimeError("fixture delete failure")):
                    status = boatbench.main(["run", "--driver", "pandaos", "--only", part, "--repeats", "1", "--out", outdir])
                with open(os.path.join(outdir, os.listdir(outdir)[0])) as f:
                    saved = json.load(f)
                cleanup = saved["repeats"][0][field]
                self.assertEqual(set(cleanup["pending_vms"]), {"vm1", "vm2"})
                self.assertEqual(set(cleanup["errors"]), {"vm1", "vm2"})
                self.assertIn("unresolved VM cleanup", report.render([saved]))
                self.assertEqual(status, 1)

    def test_repeat_create_timeout_cleans_owned_vm(self):
        with mock.patch.object(boatbench, "READY_TIMEOUT_S", 0):
            rec = boatbench.Repeat(self.d, "abc", 0, set()).run()
        self.assertIn("TimeoutError:", rec["errors"]["create"])
        self.assert_cleaned()

    def test_repeat_fork_and_snapshot_timeouts_clean_owned_vms(self):
        with mock.patch.object(boatbench, "wait_exec_ready", self.partly_ready):
            rec = boatbench.Repeat(self.d, "abc", 0, set("b")).run()
        self.assertEqual(rec["errors"], {"snapshot": "TimeoutError: readiness broke",
                                         "fork": "TimeoutError: readiness broke"})
        self.assert_cleaned()

    def test_repeat_workload_failure_cleans_owned_vm(self):
        with mock.patch.object(self.d, "exec", side_effect=RuntimeError("workload broke")):
            rec = boatbench.Repeat(self.d, "abc", 0, set()).run()
        self.assertEqual(rec["errors"], {"probe": "RuntimeError: workload broke"})
        self.assert_cleaned()

    def test_concurrent_timeouts_clean_all_owned_vms(self):
        with mock.patch.object(boatbench, "READY_TIMEOUT_S", 0):
            with self.assertRaisesRegex(RuntimeError, "3/3 creates failed: TimeoutError:"):
                boatbench.concurrent_create(self.d, "abc", 0, n=3)
        self.assert_cleaned()

    def test_concurrent_partial_create_and_readiness_failure_clean_owned_vms(self):
        self.d.max_vms = 3  # two new VMs plus the unrelated VM
        with mock.patch.object(boatbench, "wait_exec_ready", self.partly_ready):
            with self.assertRaisesRegex(RuntimeError, "2/3 creates failed:"):
                boatbench.concurrent_create(self.d, "abc", 0, n=3)
        self.assertEqual(self.d.n, 2)
        self.assert_cleaned()

    def test_density_readiness_failure_cleans_owned_vms(self):
        with mock.patch.object(boatbench, "wait_exec_ready", self.partly_ready):
            rec, _ = density.Density(self.d, "bb-x-0", (1, 2, 3), lambda m: None,
                                     boatbench.gexec, boatbench.ready).run()
        self.assertEqual(rec["max_vms"], 1)
        self.assertEqual(len(rec["errors"]), 2)
        self.assertTrue(all("TimeoutError: readiness broke" in e for e in rec["errors"]))
        self.assert_cleaned()

    def test_density_setup_and_workload_failures_clean_owned_vms(self):
        execute = self.d.exec
        for failed_script in (W.NODE_SETUP, density.RUN):
            with self.subTest(script="setup" if failed_script == W.NODE_SETUP else "workload"):
                def fail(vm, cmd, timeout=600):
                    if cmd == failed_script:
                        raise RuntimeError("workload broke")
                    return execute(vm, cmd, timeout)

                with mock.patch.object(self.d, "exec", side_effect=fail):
                    rec, _ = density.Density(self.d, "bb-x-0", (1, 2, 3), lambda m: None,
                                             boatbench.gexec, boatbench.ready).run()
                self.assertTrue(rec["errors"])
                self.assertTrue(all("RuntimeError: workload broke" in e for e in rec["errors"]))
                self.assert_cleaned()


class DensityTests(unittest.TestCase):
    def run_density(self, d, levels=(1, 2, 4)):
        os.environ["BOATBENCH_HOST_CPUS"] = "4"
        try:
            return density.Density(d, "bb-x-0", levels, lambda m: None, boatbench.gexec, boatbench.ready).run()
        finally:
            del os.environ["BOATBENCH_HOST_CPUS"]

    def test_levels_measure_host_throughput_and_clean_up(self):
        d = FakeDriver()
        d.name = "pandaos"
        rec, m = self.run_density(d)
        self.assertEqual([lv["n"] for lv in rec["levels"]], [1, 2, 4])
        self.assertEqual(rec["max_vms"], 4)
        self.assertEqual(m["density_host_runs_s_n4"], 40.0)
        self.assertEqual(m["density_per_vm_runs_s_n4"], 10.0)
        self.assertEqual(m["density_efficiency_n4"], 1.0)
        self.assertAlmostEqual(m["density_cpu_per_loop_n1"], 1.2)
        self.assertAlmostEqual(rec["levels"][2]["loops_per_cpu"], 4 * 1.2 / 4)
        self.assertEqual(d.setups, 4)  # fresh VMs, each set up: no forks
        self.assertEqual(d.live, set())

    def test_failed_density_worker_is_excluded_and_warned(self):
        d = FakeDriver()
        d.name = "pandaos"
        original = d.exec

        def execute(vm, cmd, timeout=600):
            if cmd == density.RUN and vm == "vm2":
                raise RuntimeError("injected worker failure")
            return original(vm, cmd, timeout)

        d.exec = execute
        rec, metrics = self.run_density(d, (1, 2))
        failed = rec["levels"][1]
        self.assertEqual(failed["vms_measured"], 1)
        self.assertIsNone(failed["host_runs_s"])
        self.assertNotIn("loops_per_cpu", failed)
        self.assertFalse(any(k.endswith("_n2") for k in metrics))
        self.assertEqual(d.live, set())
        result = {"driver": "pandaos", "repeats": [{"index": 0, "density": rec}]}
        self.assertEqual(set(report.density_levels(result)), {1})
        md = report.render([result], report.load_prices(os.path.join(report.HERE, "prices.json")))
        self.assertIn("incomplete density", md)
        self.assertIn("injected worker failure", md)
        self.assertIn("n=2", md)
        self.assertNotIn("| 2 |", "\n".join(report.density_section({"pandaos": result}, None)))
        host_result = dict(result, driver="host")
        host_md = report.render([host_result])
        self.assertIn("host: r0: incomplete density", host_md)
        self.assertIn("injected worker failure", host_md)

    def test_second_pass_failure_and_bad_output_exclude_trial(self):
        for bad_output in (None, "Geometric mean: 0 runs/s\nBB_CPU 1 1 0",
                           "Geometric mean: 10 runs/s", "Geometric mean: 10 runs/s\nBB_CPU 0 1 0"):
            with self.subTest(output=bad_output):
                d = FakeDriver()
                d.name = "pandaos"
                original = d.exec
                calls = []

                def execute(vm, cmd, timeout=600):
                    if cmd == density.RUN and vm == "vm2":
                        calls.append(vm)
                        if len(calls) == 2:
                            if bad_output is None:
                                raise RuntimeError("second pass failed")
                            return 0, bad_output, ""
                    return original(vm, cmd, timeout)

                d.exec = execute
                rec, metrics = self.run_density(d, (1, 2))
                self.assertEqual(d.live, set())
                self.assertTrue(rec["errors"])
                self.assertFalse(any(k.endswith("_n2") for k in metrics))
                self.assertEqual(set(report.density_levels({"repeats": [{"density": rec}]})), {1})

    def test_partial_and_invalid_density_passes_are_excluded(self):
        for passes in ([[(10.0, 1.2), (10.0, 1.2)], [(10.0, 1.2)]],
                       [[(10.0, 1.2)] * 2, [(None, 1.2)] * 2],
                       [[(10.0, 1.2)] * 2, [(float("nan"), 1.2)] * 2],
                       [[(10.0, 1.2)] * 2, [(10.0, None)] * 2]):
            with self.subTest(passes=passes):
                lv = density.summarize_level(2, passes, 4)
                self.assertIsNone(lv["host_runs_s"])
                self.assertEqual(density.metrics_of(density.finish([lv])), {})

    def test_legacy_incomplete_density_is_not_admitted(self):
        d = FakeDriver()
        d.name = "pandaos"
        rec, _ = self.run_density(d, (1, 2))
        for lv in rec["levels"]:
            for key in ("complete", "pass_counts", "passes_required"):
                del lv[key]  # actual historical shape
        complete = json.loads(json.dumps(rec))
        rec["levels"][1]["vms_measured"] = 1
        rec["levels"][1]["per_vm_each"] = [10.0]
        rec["errors"] = ["n=2: RuntimeError: legacy worker failure"]
        result = {"driver": "pandaos", "repeats": [{"index": 0, "density": rec}]}
        self.assertEqual(set(report.density_levels(result)), {1})
        self.assertIn("legacy worker failure", " ".join(report.auto_caveats({"pandaos": result})))
        result["repeats"].append({"index": 1, "density": complete})
        self.assertEqual(report.density_levels(result)[2]["host_runs_s"]["n"], 1)
        complete["errors"] = ["n=2: RuntimeError: partial passes"]
        result["repeats"] = [{"index": 1, "density": complete}]
        self.assertEqual(set(report.density_levels(result)), {1})
        complete["errors"] = []
        # An invalid solo baseline must not supply derived efficiencies at other N.
        complete["levels"][0]["vms_measured"] = 0
        result["repeats"] = [{"index": 1, "density": complete}]
        lv = report.density_levels(result)
        self.assertNotIn(1, lv)
        self.assertNotIn("efficiency", lv[2])
        self.assertIsNone(report.density_projection(lv, {"vms": 15, "cpus": 16}))

    def test_capacity_refusal_leaves_vms_out(self):
        d = FakeDriver(max_vms=2)
        d.name = "boatdev"
        rec, m = self.run_density(d)
        self.assertEqual(rec["max_vms"], 2)
        self.assertEqual([lv["n"] for lv in rec["levels"]], [1, 2])
        self.assertIsNone(rec["levels"][0]["host_cpus"])  # not one host: no loads per CPU
        self.assertNotIn("loops_per_cpu", rec["levels"][1])
        self.assertEqual(len(rec["errors"]), 2)
        self.assertTrue(all("host_capacity" in e for e in rec["errors"]))
        self.assertEqual(d.live, set())

    def test_levels_env_and_cpu_parse(self):
        self.assertEqual(density.levels_from_env({}), density.DEFAULT_LEVELS)
        self.assertEqual(density.levels_from_env({"BOATBENCH_DENSITY_LEVELS": "4, 1 15"}), (1, 4, 15))
        with self.assertRaises(ValueError):
            density.levels_from_env({"BOATBENCH_DENSITY_LEVELS": "0,2"})
        self.assertAlmostEqual(density.parse_cpu("x\nBB_CPU 10.00 9.0 1.0\n"), 1.0)
        self.assertIsNone(density.parse_cpu("no timing"))

    def test_interpolate_and_projection(self):
        pts = [(0.3, 1.0), (0.6, 0.9), (1.2, 0.5)]
        self.assertEqual(report.interpolate(pts, 0.9), (0.7, True))
        self.assertEqual(report.interpolate(pts, 2.0), (0.5, False))
        lv = {1: {"cpu_per_loop": {"median": 1.0}, "per_vm_runs_s": {"median": 10.0}},
              2: {"loops_per_cpu": {"median": 0.5}, "efficiency": {"median": 0.8}}}
        lv[1].update(loops_per_cpu={"median": 0.25}, efficiency={"median": 1.0})
        vms, cpus, load, eff, inside = report.density_projection(lv, {"vms": 15, "cpus": 16})
        self.assertEqual((vms, cpus, inside), (15, 16, False))
        self.assertAlmostEqual(load, 15 / 16)
        self.assertAlmostEqual(eff, 0.5 * 0.8 / (15 / 16))  # past the last point: the plateau holds
        lv[3] = {"loops_per_cpu": {"median": 1.0}, "efficiency": {"median": 0.5}}
        _, _, _, eff, inside = report.density_projection(lv, {"vms": 15, "cpus": 20})  # load 0.75
        self.assertTrue(inside)
        self.assertAlmostEqual(eff, (0.4 + (0.5 - 0.4) * 0.5) / 0.75)  # host per CPU interpolated
        _, _, _, eff, _ = report.density_projection(lv, {"vms": 1, "cpus": 16})  # below the range
        self.assertEqual(eff, 1.0)

    def test_host_density_sets_up_once_and_runs_n_loops(self):
        d = FakeDriver()
        d.name = "host"
        rec, _ = self.run_density(d)
        self.assertEqual(d.setups, 1)  # one Node install, N loops side by side
        self.assertEqual(d.n, 0)  # no VMs created
        self.assertEqual([lv["vms_measured"] for lv in rec["levels"]], [1, 2, 4])
        self.assertEqual(rec["levels"][0]["host_cpus"], 4)

    def test_report_measured_cost_when_price_cpu_matches_and_host_baseline(self):
        d = FakeDriver()
        d.name = "pandaos"
        rec, m = self.run_density(d)
        h = FakeDriver()
        h.name = "host"
        hrec, hm = self.run_density(h)
        cpu = {"cpu_model": "AMD Ryzen 7 PRO 8700GE w/ Radeon 780M Graphics x16", "os": "fixture OS"}
        vm = {"driver": "pandaos", "size": "default", "host": {"desc": "fixture AX42"}, "preset": "pandaos-install", "client": cpu,
              "repeats": [{"index": 0, "metrics": m, "density": rec, "errors": {}}]}
        host = {"driver": "host", "host": {"desc": "fixture AX42"}, "client": cpu, "repeats": [{"index": 0, "metrics": hm, "density": hrec, "errors": {}}]}
        path = os.path.join(report.HERE, "prices.json")
        md = report.render([vm, host], report.load_prices(path, "ax42"), report.load_published())
        self.assertIn("Cost per index unit on Hetzner AX42, MEASURED", md)
        self.assertNotIn("PROJECTION: Hetzner AX42", md)
        self.assertIn("| 4 | 40.00 | $", md)  # 4 VMs x 10 runs/s
        self.assertIn("Host-native density baseline vs VMs", md)
        self.assertIn("| 1 | 10.00 / 10.00 | 1.00 | 10.00 / 10.00 | 1.00 | 1.00 |", md)
        md102 = report.render([vm], report.load_prices(path, "ax102"), report.load_published())
        self.assertIn("PROJECTION: Hetzner AX102, trusted", md102)

    def test_host_density_does_not_attribute_mismatched_or_missing_layout(self):
        d = FakeDriver()
        trial, _ = self.run_density(d)
        def evidence(driver, cpus, desc):
            rec = copy.deepcopy(trial)
            for lv in rec["levels"]:
                lv["host_cpus"] = cpus
            return {"driver": driver, "client": {"cpu_model": "fixture CPU x16", "os": "fixture OS"},
                    "host": {"desc": desc}, "repeats": [{"density": rec}]}
        host = evidence("host", 16, "fixture host SMT on")
        vm = evidence("pandaos", 8, "fixture host SMT off")
        for candidate in (vm, {**vm, "host": {}}, {**host, "driver": "pandaos", "client": {}}):
            md = "\n".join(report.host_density_lines([host], candidate))
            self.assertIn("UNMATCHED", md)
            self.assertIn("not attributable", md)
            self.assertNotIn("MEASURED, same host", md)
            self.assertTrue(all(line.endswith("| - |") for line in md.splitlines() if line.startswith(("| 1 |", "| 2 |", "| 4 |"))))
        vm = evidence("pandaos", 16, "fixture host SMT on")
        self.assertIn("MEASURED, same host", "\n".join(report.host_density_lines([host], vm)))

    def test_report_renders_density_with_projection(self):
        d = FakeDriver()
        d.name = "pandaos"
        rec, m = self.run_density(d)
        res = {"driver": "pandaos", "size": "default", "preset": "untrusted", "repeats": [{"index": 0, "metrics": m, "density": rec,
                                                                          "errors": {}}]}
        md = report.render([res], report.load_prices(os.path.join(report.HERE, "prices.json")))
        self.assertIn("(g) Density under load: pandaos (MEASURED)", md)
        self.assertIn("PROJECTION: Hetzner AX102, untrusted (15 VMs on 16 CPUs), NOT A MEASUREMENT", md)
        self.assertIn("MIXED CPUs", md)


class HarnessFiles(unittest.TestCase):
    def test_stack_derives_cpu_overcommit_from_online_cpus(self):
        with open(os.path.join(report.HERE, "stack.sh")) as f:
            sh = f.read()
        self.assertNotIn("BENCH_CPU_OVERCOMMIT:-12}", sh)
        self.assertIn('"cpuOvercommit":$cpu_oc', sh)
        self.assertIn("cpus=$(nproc)", sh)

    def test_writeback_preset(self):
        with open(os.path.join(report.HERE, "presets.json")) as f:
            self.assertEqual(json.load(f)["trusted-writeback"], {"diskCache": "Writeback"})
        self.assertTrue(boatbench.fc_cache_type('{"diskCache":"Writeback"}').startswith("Writeback"))

    def test_docs_cost_md_matches_generated_cost_tables(self):
        cost_md_path = os.path.normpath(os.path.join(report.HERE, "..", "..", "docs", "cost.md"))
        with open(cost_md_path) as f:
            cost_doc = f.read()
        with open(os.path.join(report.HERE, "prices.json")) as f:
            prices = json.load(f)
        checks = [
            ("vm_hour_table", cost_tables.vm_hour_table(prices)),
            ("sensitivity_tables", cost_tables.sensitivity_tables(prices)),
            ("ai_proxy_table", cost_tables.ai_proxy_table(prices)),
        ]
        for s in prices["servers"]:
            checks.append((f"retention_{s['key']}", cost_tables.retention_table(prices, s)))
        for name, tbl in checks:
            block = "\n".join(tbl).strip() if isinstance(tbl, list) else tbl.strip()
            self.assertIn(block, cost_doc, f"docs/cost.md drifted from cost_tables.{name}")


    def test_cost_docs_compat_route_count_matches_local_inventory(self):
        import runpy
        api = os.path.join(report.HERE, "..", "..", "components", "boat", "api", "tools", "compat_openapi.py")
        inventory = runpy.run_path(api)
        total = len(inventory["ROUTES"])
        unsupported = len(inventory["ALWAYS_UNSUPPORTED"])
        with open(os.path.join(report.HERE, "..", "..", "docs", "cost.md")) as f:
            doc = f.read()
        self.assertIn(f"{total - unsupported} of {total} operations mapped", doc)
        self.assertIn(f"{unsupported} SaaS-only routes", doc)

    def test_docs_cost_md_headline_prose_ratios_follow_the_published_reference(self):
        """Prose ratios in cost.md are derived from published.json + prices.json; a reference change must fail here."""
        with open(os.path.normpath(os.path.join(report.HERE, "..", "..", "docs", "cost.md"))) as f:
            doc = f.read()
        with open(os.path.join(report.HERE, "prices.json")) as f:
            prices = json.load(f)
        pub = report.load_published()
        ref = report.boatdev_ref(prices, pub)  # USD per run at boat.dev's price and published speed
        host = report.server_price(prices, "ax42")["density_model"]["host_usd_per_hour"]
        cost = lambda rs: host / 3.6 / rs  # noqa: E731 (USD per run on the AX42 at rs runs/s)
        trusted, untrusted, bare = 87.06, 75.91, 99.10  # measured AX42 runs/s quoted in cost.md
        self.assertIn(f"${ref:.6f}", doc)  # per 1000 runs
        self.assertIn(f"costs {cost(trusted) / ref:.2f}\u00d7 (trusted) to "
                      f"{cost(untrusted) / ref:.2f}\u00d7 (untrusted)", doc)
        self.assertIn(f"{cost(bare) / ref:.2f}\u00d7 boat.dev's published cost per index unit", doc)
        self.assertIn(f"needs {host / 3.6 / ref:.0f} index units", doc)
        self.assertIn(f"${cost(trusted):.6f}", doc)
        self.assertIn(f"${cost(bare):.6f}", doc)
        self.assertNotIn("27.67 runs/s reference", doc)


class ConnPoolSafetyTests(unittest.TestCase):
    @staticmethod
    def _read_http_request(conn):
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = conn.recv(4096)
            if not chunk:
                return None, {}, b""
            buf += chunk
        head, rest = buf.split(b"\r\n\r\n", 1)
        lines = head.decode("iso-8859-1").split("\r\n")
        req_line = lines[0]
        hdrs = {}
        for line in lines[1:]:
            if ":" in line:
                k, v = line.split(":", 1)
                hdrs[k.strip().lower()] = v.strip()
        clen = int(hdrs.get("content-length", "0"))
        while len(rest) < clen:
            chunk = conn.recv(4096)
            if not chunk:
                break
            rest += chunk
        return req_line, hdrs, rest[:clen]

    @staticmethod
    def _send_json(conn, status, payload, keep_alive=True):
        body = json.dumps(payload).encode("utf-8")
        conn_hdr = "keep-alive" if keep_alive else "close"
        resp = (
            f"HTTP/1.1 {status} OK\r\n"
            f"Content-Type: application/json\r\n"
            f"Content-Length: {len(body)}\r\n"
            f"Connection: {conn_hdr}\r\n\r\n"
        ).encode("ascii") + body
        conn.sendall(resp)

    def test_half_closed_idle_socket_discarded_before_post_sends_once(self):
        """(a) Server-closed idle keep-alive socket is caught by _checkout (_is_conn_alive)
        before POST writes any bytes, succeeding on a fresh socket with 1 POST received."""
        post_requests = []
        accept_count = 0
        idle_closed_evt = threading.Event()
        stop_evt = threading.Event()

        lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        lsock.bind(("127.0.0.1", 0))
        lsock.listen(8)
        lsock.settimeout(0.2)
        port = lsock.getsockname()[1]

        def serve():
            nonlocal accept_count
            while not stop_evt.is_set():
                try:
                    conn, _ = lsock.accept()
                except socket.timeout:
                    continue
                except OSError:
                    break
                accept_count += 1
                idx = accept_count
                conn.settimeout(2.0)
                try:
                    req_line, _, body = self._read_http_request(conn)
                    if req_line is None:
                        continue
                    if req_line.startswith("POST "):
                        post_requests.append((idx, req_line, body))
                    if idx == 1:
                        # Reply keep-alive to initial GET, then immediately half-close idle socket.
                        self._send_json(conn, 200, {"ok": True}, keep_alive=True)
                        conn.shutdown(socket.SHUT_WR)
                        conn.close()
                        idle_closed_evt.set()
                        continue
                    self._send_json(conn, 200, {"id": "vm-created"}, keep_alive=True)
                finally:
                    try:
                        conn.close()
                    except OSError:
                        pass

        worker = threading.Thread(target=serve)
        worker.start()
        pool = drivers._ConnPool()
        url_base = f"http://127.0.0.1:{port}"
        try:
            pool.request("GET", f"{url_base}/v1/vms/vm-0", "boat_k")
            self.assertEqual(pool.idle_count(), 1)
            self.assertTrue(idle_closed_evt.wait(timeout=2.0))
            # Wait briefly for kernel loopback FIN delivery to reach client socket buffer.
            with pool._lock:
                idle_sock = next(iter(pool._idle.values()))[0].sock
            for _ in range(50):
                if not drivers._is_conn_alive(idle_sock):
                    break
                threading.Event().wait(0.01)
            self.assertFalse(drivers._is_conn_alive(idle_sock))

            out = pool.request("POST", f"{url_base}/v1/vms", "boat_k", body={"name": "bb-1"})
            self.assertEqual(json.loads(out)["id"], "vm-created")
            self.assertEqual(len(post_requests), 1, "server must receive exactly 1 POST")
            self.assertEqual(accept_count, 2, "dead idle socket must be replaced by 1 fresh connection")
        finally:
            stop_evt.set()
            pool.close()
            lsock.close()
            worker.join(timeout=3)

    def test_non_idempotent_post_reset_after_send_never_retries_while_idempotent_does(self):
        """(b) Non-idempotent POST without Idempotency-Key raises on reset after send without
        sending a duplicate POST (server sees 1 POST, never 2), whereas BoatDev create/fork
        attach Idempotency-Key and retry safely with the same key."""
        for mode in ("rst", "eof"):
            with self.subTest(mode=mode):
                post_requests = []
                stop_evt = threading.Event()
                lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                lsock.bind(("127.0.0.1", 0))
                lsock.listen(8)
                lsock.settimeout(0.2)
                port = lsock.getsockname()[1]

                def serve(close_mode=mode):
                    while not stop_evt.is_set():
                        try:
                            conn, _ = lsock.accept()
                        except socket.timeout:
                            continue
                        except OSError:
                            break
                        conn.settimeout(2.0)
                        try:
                            # Request 1 on this keep-alive connection: GET succeeds and pools socket.
                            req1, _, _ = self._read_http_request(conn)
                            if req1 is None:
                                continue
                            self._send_json(conn, 200, {"ok": True}, keep_alive=True)

                            # Request 2 on the reused socket: server reads full POST, then resets before replying!
                            req2, hdrs2, body2 = self._read_http_request(conn)
                            if req2 is not None and req2.startswith("POST "):
                                post_requests.append((req2, hdrs2, body2))
                            if close_mode == "rst":
                                conn.setsockopt(
                                    socket.SOL_SOCKET,
                                    socket.SO_LINGER,
                                    struct.pack("ii", 1, 0),
                                )
                        except OSError:
                            pass
                        finally:
                            try:
                                conn.close()
                            except OSError:
                                pass

                worker = threading.Thread(target=serve)
                worker.start()
                pool = drivers._ConnPool()
                url_base = f"http://127.0.0.1:{port}"
                try:
                    pool.request("GET", f"{url_base}/v1/vms/vm-0", "boat_k")
                    self.assertEqual(pool.idle_count(), 1)
                    with self.assertRaises(drivers._STALE_CONN_ERRORS):
                        pool.request("POST", f"{url_base}/v1/vms/vm-0/exec", "boat_k", body={"cmd": ["true"]})
                    self.assertEqual(len(post_requests), 1, "non-idempotent POST must NEVER be sent twice")
                finally:
                    stop_evt.set()
                    pool.close()
                    lsock.close()
                    worker.join(timeout=3)

        # Verify BoatDev.create and BoatDev.fork attach a fresh UUID Idempotency-Key and retry safely with the same key.
        seen_idem = []
        stop_evt = threading.Event()
        lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        lsock.bind(("127.0.0.1", 0))
        lsock.listen(8)
        lsock.settimeout(0.2)
        port = lsock.getsockname()[1]

        def serve_boatdev():
            conn_idx = 0
            while not stop_evt.is_set():
                try:
                    conn, _ = lsock.accept()
                except socket.timeout:
                    continue
                except OSError:
                    break
                conn_idx += 1
                conn.settimeout(2.0)
                try:
                    while True:
                        req_line, hdrs, _ = self._read_http_request(conn)
                        if req_line is None:
                            break
                        if req_line.startswith("GET "):
                            self._send_json(conn, 200, {"sandbox": {"id": "bx_1", "state": "ready"}}, keep_alive=True)
                            continue
                        if req_line.startswith("POST "):
                            seen_idem.append((req_line.split()[1], hdrs.get("idempotency-key")))
                            if conn_idx == 1 and len(seen_idem) == 1:
                                # Simulate stale reset after reading first POST /sandboxes on reused conn 1.
                                conn.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                                break
                            if req_line.startswith("POST /sandboxes "):
                                self._send_json(conn, 202, {"sandbox": {"id": "bx_new"}}, keep_alive=True)
                            else:
                                self._send_json(conn, 202, {"id": "bx_fork"}, keep_alive=True)
                except OSError:
                    pass
                finally:
                    try:
                        conn.close()
                    except OSError:
                        pass

        worker = threading.Thread(target=serve_boatdev)
        worker.start()
        d = drivers.BoatDev(base=f"http://127.0.0.1:{port}", key="boat_k")
        try:
            d.ping("bx_1")
            self.assertEqual(d.create("bb-idem"), "bx_new")
            self.assertEqual(d.fork("bx_new", "bb-fork"), "bx_fork")
            self.assertEqual(len(seen_idem), 3)
            # Retry of create reused the exact same Idempotency-Key; fork generated a distinct fresh UUID.
            self.assertEqual(seen_idem[0], seen_idem[1])
            uuid.UUID(seen_idem[0][1])
            uuid.UUID(seen_idem[2][1])
            self.assertNotEqual(seen_idem[0][1], seen_idem[2][1])
        finally:
            stop_evt.set()
            d.close()
            lsock.close()
            worker.join(timeout=3)

    def test_put_poll_and_pandaos_idempotency_key_retry_stale_keepalive(self):
        """(d) _ConnPool retries stale keep-alive sockets on PUT (put_file), poll=True (run_true),
        and PandaOS/PandaBoat create & fork (which attach hex Idempotency-Key headers)."""
        seen = []
        stop_evt = threading.Event()
        lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        lsock.bind(("127.0.0.1", 0))
        lsock.listen(16)
        lsock.settimeout(0.2)
        port = lsock.getsockname()[1]
        reset_targets = {"POST /v1/vms", "POST /v1/vms/vm-new/fork", "PUT /v1/vms/vm-new/files", "POST /v1/vms/vm-new/exec"}
        reset_done = set()

        def serve_pandaos():
            while not stop_evt.is_set():
                try:
                    conn, _ = lsock.accept()
                except socket.timeout:
                    continue
                except OSError:
                    break
                conn.settimeout(2.0)
                try:
                    while True:
                        req_line, hdrs, body = self._read_http_request(conn)
                        if req_line is None:
                            break
                        method, target, _ = req_line.split(" ", 2)
                        base_target = target.split("?", 1)[0]
                        key_tag = f"{method} {base_target}"
                        seen.append((key_tag, hdrs.get("idempotency-key"), body))
                        if key_tag in reset_targets and key_tag not in reset_done:
                            reset_done.add(key_tag)
                            conn.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                            break
                        if key_tag == "GET /v1/vms/vm-0":
                            self._send_json(conn, 200, {"id": "vm-0"}, keep_alive=True)
                        elif key_tag == "POST /v1/vms":
                            self._send_json(conn, 201, {"id": "vm-new"}, keep_alive=True)
                        elif key_tag == "POST /v1/vms/vm-new/fork":
                            self._send_json(conn, 201, {"id": "vm-fork"}, keep_alive=True)
                        elif key_tag == "PUT /v1/vms/vm-new/files":
                            self._send_json(conn, 200, {"ok": True}, keep_alive=True)
                        elif key_tag == "POST /v1/vms/vm-new/exec":
                            ndjson = b'{"exit":0}\n'
                            resp = (
                                f"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\n"
                                f"Content-Length: {len(ndjson)}\r\nConnection: keep-alive\r\n\r\n"
                            ).encode("ascii") + ndjson
                            conn.sendall(resp)
                except OSError:
                    pass
                finally:
                    try:
                        conn.close()
                    except OSError:
                        pass

        worker = threading.Thread(target=serve_pandaos)
        worker.start()
        d = drivers.PandaBoat(url=f"http://127.0.0.1:{port}", key="boat_k")
        try:
            d.ping("vm-0")  # prime idle keep-alive socket -> next call (create) hits stale reset & retries
            self.assertEqual(d.create("bb-1"), "vm-new")
            # Next call (fork) reuses socket from create -> hits stale reset & retries with same Idempotency-Key
            self.assertEqual(d.fork("vm-new", "bb-fork"), "vm-fork")
            # Next call (PUT put_file) reuses socket from fork -> hits stale reset & retries
            d.put_file("vm-new", "/tmp/hello.txt", b"payload-bytes")
            # Next call (poll=True run_true) reuses socket from put_file -> hits stale reset & retries
            self.assertEqual(d.run_true("vm-new"), 0)

            create_attempts = [x for x in seen if x[0] == "POST /v1/vms"]
            fork_attempts = [x for x in seen if x[0] == "POST /v1/vms/vm-new/fork"]
            put_attempts = [x for x in seen if x[0] == "PUT /v1/vms/vm-new/files"]
            poll_attempts = [x for x in seen if x[0] == "POST /v1/vms/vm-new/exec"]

            self.assertEqual(len(create_attempts), 2)
            self.assertEqual(create_attempts[0][1], create_attempts[1][1])
            self.assertRegex(create_attempts[0][1], r"^[0-9a-f]{32}$")

            self.assertEqual(len(fork_attempts), 2)
            self.assertEqual(fork_attempts[0][1], fork_attempts[1][1])
            self.assertRegex(fork_attempts[0][1], r"^[0-9a-f]{32}$")
            self.assertNotEqual(create_attempts[0][1], fork_attempts[0][1])

            self.assertEqual(len(put_attempts), 2)
            self.assertEqual(put_attempts[1][2], b"payload-bytes")
            self.assertEqual(len(poll_attempts), 2)
        finally:
            stop_evt.set()
            d.close()
            lsock.close()
            worker.join(timeout=3)

    def test_idle_pool_capped_at_max_idle_conns_per_origin(self):
        """(c) Idle pool per origin never exceeds MAX_IDLE_CONNS_PER_ORIGIN."""
        total_workers = drivers.MAX_IDLE_CONNS_PER_ORIGIN + 8
        all_connected = threading.Barrier(total_workers)
        stop_evt = threading.Event()

        lsock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        lsock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        lsock.bind(("127.0.0.1", 0))
        lsock.listen(64)
        lsock.settimeout(0.2)
        port = lsock.getsockname()[1]

        def handle_client(conn):
            conn.settimeout(5.0)
            try:
                req_line, _, _ = self._read_http_request(conn)
                if req_line is None:
                    return
                all_connected.wait(timeout=5.0)
                self._send_json(conn, 200, {"ok": True}, keep_alive=True)
                # Hold server side open until test inspects client idle pool.
                stop_evt.wait(timeout=5.0)
            except (OSError, threading.BrokenBarrierError):
                pass
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

        client_threads = []

        def accept_loop():
            while not stop_evt.is_set():
                try:
                    conn, _ = lsock.accept()
                except socket.timeout:
                    continue
                except OSError:
                    break
                th = threading.Thread(target=handle_client, args=(conn,))
                th.start()
                client_threads.append(th)

        acceptor = threading.Thread(target=accept_loop)
        acceptor.start()
        pool = drivers._ConnPool()
        checked_in = []
        orig_checkin = pool._checkin

        def record_checkin(key, conn):
            checked_in.append(conn)
            return orig_checkin(key, conn)

        pool._checkin = record_checkin
        url = f"http://127.0.0.1:{port}/v1/health"
        errs = []

        def run_one():
            try:
                pool.request("GET", url, "boat_k", timeout=5)
            except BaseException as exc:
                errs.append(exc)

        try:
            workers = [threading.Thread(target=run_one) for _ in range(total_workers)]
            for w in workers:
                w.start()
            for w in workers:
                w.join(timeout=5)
            self.assertEqual(errs, [])
            self.assertEqual(len(checked_in), total_workers)
            self.assertEqual(pool.idle_count(), drivers.MAX_IDLE_CONNS_PER_ORIGIN)
            origin_key = ("http", "127.0.0.1", port)
            self.assertEqual(len(pool._idle[origin_key]), drivers.MAX_IDLE_CONNS_PER_ORIGIN)
            closed_excess = [c for c in checked_in if c.sock is None]
            self.assertEqual(len(closed_excess), total_workers - drivers.MAX_IDLE_CONNS_PER_ORIGIN)
        finally:
            stop_evt.set()
            pool.close()
            lsock.close()
            acceptor.join(timeout=3)
            for th in client_threads:
                th.join(timeout=3)


class CompatDriverTests(unittest.TestCase):
    def test_compat_driver_urls_and_env_setup_paths(self):
        d_def = drivers.PandaOSCompat(url="http://127.0.0.1:8740/", key="boat_k")
        self.assertTrue(d_def.no_env)

        d1 = drivers.PandaOSCompat(url="http://127.0.0.1:8740/", key="boat_k", size="large",
                                   no_env=False, environment="base", setup_script="echo hi")
        self.assertEqual(d1.url, "http://127.0.0.1:8740/api/v1")
        self.assertEqual(d1.name, "pandaos-compat")
        self.assertEqual(d1.api_mode, "compat")
        self.assertFalse(d1.no_env)

        d2 = drivers.PandaOS(url="http://127.0.0.1:8740/api/v1/", key="boat_k", api_mode="compat")
        self.assertIsInstance(d2, drivers.PandaOSCompat)
        self.assertEqual(d2.url, "http://127.0.0.1:8740/api/v1")
        self.assertTrue(d2.no_env)

        calls = []

        def fake_http(method, url, key, body=None, raw=None, timeout=660, ctype="application/json",
                      headers=None, poll=False):
            calls.append((method, url, body, headers))
            if method == "POST" and url.endswith("/api/v1/sandboxes"):
                return json.dumps({"sandbox": {"id": "bx_1", "state": "running"}})
            if method == "POST" and url.endswith("/api/v1/sandboxes/bx_1/commands"):
                return json.dumps({"exitCode": 0, "stdout": "ok\n", "stderr": ""})
            if method == "POST" and url.endswith("/api/v1/sandboxes/bx_1/fork"):
                return json.dumps({"id": "bx_2"})
            if method == "POST" and url.endswith("/api/v1/sandboxes/bx_1/stop"):
                return json.dumps({"sandbox": {"id": "bx_1", "state": "archived"}, "operation": {"status": "completed"}})
            if method == "POST" and url.endswith("/api/v1/sandboxes/bx_1/resume"):
                return json.dumps({"sandbox": {"id": "bx_1", "state": "running"}})
            if method == "DELETE" and url.endswith("/api/v1/sandboxes/bx_1"):
                return json.dumps({"operation": {"status": "completed"}})
            return json.dumps({})

        with mock.patch.object(drivers, "_http", side_effect=fake_http):
            sid = d1.create("bb-compat-a")
            self.assertEqual(sid, "bx_1")
            rc, out, err = d1.exec(sid, "echo hi")
            self.assertEqual((rc, out, err), (0, "ok\n", ""))
            fid = d1.fork(sid, "bb-compat-f")
            self.assertEqual(fid, "bx_2")
            d1.stop(sid)
            d1.resume(sid)
            d1.delete(sid)

        # Create uses normal env/setup path (no noEnv: true)
        m, u, body, hdrs = calls[0]
        self.assertEqual((m, u), ("POST", "http://127.0.0.1:8740/api/v1/sandboxes"))
        self.assertNotIn("noEnv", body)
        self.assertEqual(body["type"], "large")
        self.assertEqual(body["environment"], "base")
        self.assertEqual(body["setupScript"], "echo hi")
        self.assertIn("Idempotency-Key", hdrs)

        # Fork and resume preserve environment and omit noEnv
        self.assertEqual(calls[2][1], "http://127.0.0.1:8740/api/v1/sandboxes/bx_1/fork")
        self.assertNotIn("noEnv", calls[2][2])
        self.assertEqual(calls[2][2]["environment"], "base")
        # Immediate-completed stop did not issue an extra GET poll
        self.assertEqual(calls[3][1], "http://127.0.0.1:8740/api/v1/sandboxes/bx_1/stop")
        self.assertEqual(calls[4][1], "http://127.0.0.1:8740/api/v1/sandboxes/bx_1/resume")
        self.assertEqual(calls[4][2]["environment"], "base")
        # Delete sends X-Ascii-Confirm-Delete
        self.assertEqual(calls[5][0], "DELETE")
        self.assertEqual(calls[5][3]["X-Ascii-Confirm-Delete"], "bx_1")

    def test_native_and_compat_default_no_env_true(self):
        native = drivers.PandaOS(url="http://127.0.0.1:8740", key="boat_k")
        compat = drivers.PandaOSCompat(url="http://127.0.0.1:8740", key="boat_k")
        seen = []

        def fake_http(method, url, key, body=None, **kw):
            seen.append((method, url, body))
            if url.endswith("/v1/vms"):
                return json.dumps({"id": "vm_1"})
            return json.dumps({"sandbox": {"id": "bx_9"}})

        with mock.patch.object(drivers, "_http", side_effect=fake_http):
            self.assertEqual(native.create("n1"), "vm_1")
            self.assertEqual(compat.create("c1"), "bx_9")

        self.assertEqual(seen[0][1], "http://127.0.0.1:8740/v1/vms")
        self.assertEqual(seen[0][2]["no_env"], True)
        self.assertEqual(seen[0][2]["secrets"], [])
        self.assertEqual(seen[1][1], "http://127.0.0.1:8740/api/v1/sandboxes")
        self.assertEqual(seen[1][2]["noEnv"], True)

    def test_cli_wires_compat_and_rejects_invalid_flag_combos(self):
        with self.assertRaises(SystemExit):
            boatbench.main(["run", "--driver", "boatdev", "--compat"])
        with self.assertRaises(SystemExit):
            boatbench.main(["run", "--driver", "pandaos", "--compat", "--api-mode", "native"])
        with self.assertRaises(SystemExit):
            boatbench.main(["run", "--driver", "pandaos", "--with-env", "--no-env"])

        fake = FakeDriver()
        fake.name = "pandaos-compat"
        seen_kwargs = []

        def make_compat(**kw):
            seen_kwargs.append(kw)
            return fake

        with tempfile.TemporaryDirectory() as outdir, \
                mock.patch.dict(drivers.DRIVERS, {"pandaos-compat": make_compat}), \
                mock.patch.object(boatbench, "client_info", return_value={"cpu_model": "t", "os": "t", "egress": "-"}):
            rc = boatbench.main(["run", "--driver", "pandaos", "--compat", "--only", "a", "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            self.assertEqual(seen_kwargs[-1], {"no_env": True, "environment": None, "setup_script": None})

            rc = boatbench.main(["run", "--driver", "pandaos", "--compat", "--with-env", "--environment", "base",
                                 "--setup-script", "echo ok", "--only", "a", "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            self.assertEqual(seen_kwargs[-1], {"no_env": False, "environment": "base", "setup_script": "echo ok"})
            files = sorted(f for f in os.listdir(outdir) if f.endswith(".json"))
            with open(os.path.join(outdir, files[-1])) as fh:
                data = json.load(fh)
            self.assertEqual(data["api_mode"], "compat")


class MemoryDensityTests(unittest.TestCase):
    def test_mem_run_script_executes_locally_and_parses(self):
        script = density.mem_run_script(mem_mib=4, sweeps=2)
        rc, out, err = drivers.Host().exec("host", script, timeout=60)
        self.assertEqual(rc, 0, msg=err)
        info = density.parse_mem_info(out)
        self.assertIsNotNone(info)
        self.assertEqual(info["mem_mib"], 4)
        self.assertEqual(info["sweeps"], 2)
        self.assertTrue(density.positive_finite(density.parse_mem(out)))
        self.assertTrue(density.positive_finite(density.parse_cpu(out)))

    def test_memory_density_skips_node_setup_and_emits_mem_metrics(self):
        d = FakeDriver()
        d.name = "pandaos"
        orig_exec = d.exec

        def mem_exec(vm, cmd, timeout=600):
            if "BB_MEM" in cmd:
                return 0, 'BB_MEM {"mem_mib": 512, "sweeps": 3, "elapsed_s": 0.5, "mib_per_s": 3072.0, "rss_mib": 512.0}\nBB_CPU 0.50 0.40 0.10\n', ""
            return orig_exec(vm, cmd, timeout)

        d.exec = mem_exec
        os.environ["BOATBENCH_HOST_CPUS"] = "4"
        try:
            rec, metrics = density.Density(
                d, "bb-mem-0", (1, 2), lambda m: None, boatbench.gexec, boatbench.ready,
                workload="memory", mem_mib=512,
            ).run()
        finally:
            del os.environ["BOATBENCH_HOST_CPUS"]

        self.assertEqual(d.setups, 0)  # memory workload does not install Node
        self.assertEqual(rec["workload"], "memory")
        self.assertEqual(rec["mem_mib"], 512)
        self.assertEqual([lv["working_set_mib"] for lv in rec["levels"]], [512, 1024])
        self.assertEqual(metrics["density_mem_per_vm_mib_s_n2"], 3072.0)
        self.assertEqual(metrics["density_mem_host_mib_s_n2"], 6144.0)
        self.assertEqual(metrics["density_mem_efficiency_n2"], 1.0)
        self.assertNotIn("density_host_runs_s_n2", metrics)
        self.assertEqual(d.live, set())

    def test_memory_density_oom_worker_is_excluded_and_cleaned(self):
        d = FakeDriver()
        d.name = "pandaos"

        def oom_exec(vm, cmd, timeout=600):
            if vm == "vm2" and "BB_MEM" in cmd:
                return 137, "", "Killed"
            if "BB_MEM" in cmd:
                return 0, 'BB_MEM {"mem_mib": 1024, "sweeps": 3, "elapsed_s": 1.0, "mib_per_s": 3072.0}\nBB_CPU 1.0 0.8 0.2\n', ""
            return 0, "", ""

        d.exec = oom_exec
        rec, metrics = density.Density(
            d, "bb-oom-0", (1, 2), lambda m: None, boatbench.gexec, boatbench.ready,
            workload="memory", mem_mib=1024,
        ).run()
        self.assertEqual(d.live, set())
        self.assertEqual(rec["levels"][0]["complete"], True)
        self.assertEqual(rec["levels"][1]["complete"], False)
        self.assertEqual(rec["levels"][1]["vms_measured"], 1)
        self.assertTrue(any("exit 137" in e for e in rec["errors"]))
        self.assertIn("density_mem_host_mib_s_n1", metrics)
        self.assertNotIn("density_mem_host_mib_s_n2", metrics)

    def test_memory_density_cli_and_env_validation(self):
        self.assertEqual(density.workload_from_env({}), "cpu")
        self.assertEqual(density.workload_from_env({"BOATBENCH_DENSITY_WORKLOAD": "memory"}), "memory")
        self.assertEqual(density.mem_mib_from_env({}), density.DEFAULT_MEM_MIB)
        self.assertEqual(density.mem_mib_from_env({"BOATBENCH_DENSITY_MEM_MIB": "2048"}), 2048)
        with self.assertRaises(ValueError):
            density.workload_from_env({"BOATBENCH_DENSITY_WORKLOAD": "bogus"})
        with self.assertRaises(ValueError):
            density.mem_mib_from_env({"BOATBENCH_DENSITY_MEM_MIB": "0"})
        with self.assertRaises(SystemExit):
            boatbench.main(["run", "--driver", "host", "--only", "g", "--density-workload", "memory", "--density-mem-mib", "0"])

        with tempfile.TemporaryDirectory() as outdir, \
                mock.patch.dict(os.environ, {"BOATBENCH_DENSITY_LEVELS": "1"}), \
                mock.patch.object(boatbench, "client_info", return_value={"cpu_model": "t", "os": "t", "egress": "-"}):
            rc = boatbench.main(["run", "--driver", "host", "--only", "g",
                                 "--density-workload", "memory", "--density-mem-mib", "4",
                                 "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            files = [f for f in os.listdir(outdir) if f.endswith(".json")]
            with open(os.path.join(outdir, files[0])) as fh:
                data = json.load(fh)
            rep = data["repeats"][0]
            self.assertEqual(rep["density"]["workload"], "memory")
            self.assertEqual(rep["density"]["mem_mib"], 4)
            self.assertGreater(rep["metrics"]["density_mem_host_mib_s_n1"], 0)


class CapacityAdaptationAndCaveatTests(unittest.TestCase):
    def test_host_admitted_vms_reuses_admission_math(self):
        # 16 GiB runner (~15872 MiB MemTotal) with memOvercommit 1 admits 1 default VM.
        self.assertEqual(
            boatbench.host_admitted_vms(options='{"memOvercommit": 1, "cpuOvercommit": 12}',
                                        config_path="", memtotal_mib=15872, cpus=2),
            1,
        )
        # AX42 (~62000 MiB MemTotal, 8 cores SMT off) with memOvercommit 1 admits 7 default VMs.
        self.assertEqual(
            boatbench.host_admitted_vms(options='{"memOvercommit": 1, "cpuOvercommit": 4}',
                                        config_path="", memtotal_mib=62000, cpus=8),
            7,
        )
        # Without boatd config or options, returns None (do not artificially clamp).
        self.assertIsNone(boatbench.host_admitted_vms(options=None, config_path="", memtotal_mib=15872, cpus=2))

    def test_repeat_skips_snapshot_and_fork_when_host_admits_one_vm(self):
        d = FakeDriver()
        rec = boatbench.Repeat(d, "cap1", 0, set("b"), max_vms=1).run()
        self.assertEqual(rec["errors"], {})
        self.assertEqual(rec["skipped"], {
            "snapshot": "skipped: host admits 1 VMs",
            "fork": "skipped: host admits 1 VMs",
        })
        self.assertNotIn("snapshot_s", rec["metrics"])
        self.assertNotIn("from_snapshot_ready_s", rec["metrics"])
        self.assertNotIn("fork_ready_s", rec["metrics"])
        self.assertIn("stop_s", rec["metrics"])
        self.assertIn("resume_ready_s", rec["metrics"])
        self.assertEqual(d.live, set())

    def test_cmd_run_skips_or_clamps_concurrent_and_honors_override(self):
        # 1. Host admits 1 VM -> workload c is explicitly skipped, never reported as passed.
        d1 = FakeDriver()
        with tempfile.TemporaryDirectory() as outdir, \
                mock.patch.dict(drivers.DRIVERS, {"pandaos": lambda: d1}), \
                mock.patch.object(boatbench, "host_admitted_vms", return_value=1), \
                mock.patch.object(boatbench, "client_info", return_value={"cpu_model": "t", "os": "t", "egress": "-"}), \
                mock.patch.dict(os.environ, {"BOATBENCH_CONCURRENT_N": ""}):
            rc = boatbench.main(["run", "--driver", "pandaos", "--only", "bc", "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            with open(os.path.join(outdir, os.listdir(outdir)[0])) as fh:
                saved = json.load(fh)
        rep = saved["repeats"][0]
        self.assertEqual(saved["host"]["admitted_vms"], 1)
        self.assertEqual(rep["skipped"]["concurrent"], "skipped: host admits 1 VMs")
        self.assertEqual(rep["skipped"]["snapshot"], "skipped: host admits 1 VMs")
        self.assertEqual(rep["skipped"]["fork"], "skipped: host admits 1 VMs")
        self.assertFalse(any(k.startswith("concurrent") for k in rep["metrics"]))

        # Report surfaces skipped steps and run_true asymmetry caveat.
        md = report.render([saved])
        self.assertIn("skipped: host admits 1 VMs", md)
        self.assertIn("not passed", md)
        self.assertIn("--driver pandaos --compat", md)
        self.assertIn("bash -lc true", md)

        # 2. Host admits 7 VMs -> workload c clamps to 7 and report renders concurrent7_wall_s.
        d7 = FakeDriver()
        d7.ROUND_TRIPS = drivers.PandaOS.ROUND_TRIPS
        with tempfile.TemporaryDirectory() as outdir, \
                mock.patch.dict(drivers.DRIVERS, {"pandaos": lambda: d7}), \
                mock.patch.object(boatbench, "host_admitted_vms", return_value=7), \
                mock.patch.object(boatbench, "client_info", return_value={"cpu_model": "t", "os": "t", "egress": "-"}), \
                mock.patch.dict(os.environ, {"BOATBENCH_CONCURRENT_N": ""}):
            rc = boatbench.main(["run", "--driver", "pandaos", "--only", "c", "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            with open(os.path.join(outdir, os.listdir(outdir)[0])) as fh:
                saved7 = json.load(fh)
        rep7 = saved7["repeats"][0]
        self.assertEqual(rep7["concurrent_n"], 7)
        self.assertIn("concurrent7_wall_s", rep7["metrics"])
        self.assertIn("concurrent7_wall_s_net", rep7["metrics"])
        md7 = report.render([saved7])
        self.assertIn("(c) 7 concurrent creates, wall to all exec-ready", md7)

        # 3. Explicit BOATBENCH_CONCURRENT_N override wins even when host admits 7.
        d_ovr = FakeDriver()
        with tempfile.TemporaryDirectory() as outdir, \
                mock.patch.dict(drivers.DRIVERS, {"pandaos": lambda: d_ovr}), \
                mock.patch.object(boatbench, "host_admitted_vms", return_value=7), \
                mock.patch.object(boatbench, "client_info", return_value={"cpu_model": "t", "os": "t", "egress": "-"}), \
                mock.patch.dict(os.environ, {"BOATBENCH_CONCURRENT_N": "3"}):
            rc = boatbench.main(["run", "--driver", "pandaos", "--only", "c", "--repeats", "1", "--out", outdir])
            self.assertEqual(rc, 0)
            with open(os.path.join(outdir, os.listdir(outdir)[0])) as fh:
                saved3 = json.load(fh)
        self.assertEqual(saved3["repeats"][0]["concurrent_n"], 3)
        self.assertIn("concurrent3_wall_s", saved3["repeats"][0]["metrics"])


if __name__ == "__main__":
    unittest.main()
