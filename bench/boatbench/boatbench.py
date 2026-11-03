#!/usr/bin/env python3
"""boatbench: the same workloads on PandaOS boat and on boat.dev.

  boatbench.py run --driver pandaos|boatdev [--repeats 5] [--only a,b,c,d,e,f] [--out DIR]
  boatbench.py run --driver host --only ad [--repeats 5] [--out DIR]   # same scripts, no VM
  boatbench.py report RESULTS.json... [--prices prices.json] [--out report.md]

pandaos needs BOAT_URL + BOAT_KEY; boatdev needs BOATDEV_API_KEY. Each repeat is a
fresh 4 vCPU / 8 GiB VM. Raw per-repeat data goes to DIR/results-<driver>-<ts>.json.
Optional env: BOATBENCH_HOST_DESC (hardware/location of the provider host, printed
in the report), BOATBENCH_RUN_URL, BOATBENCH_BOATD_OPTIONS (printed in the report).
"""
import argparse
import concurrent.futures
import datetime
import json
import os
import platform
import secrets
import subprocess
import sys
import time
import traceback
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import drivers  # noqa: E402
import report  # noqa: E402
import workloads as W  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
READY_TIMEOUT_S = 600
RTT_CALLS = 50
CONCURRENT_N = 10
ALL_PARTS = "abcdef"


def log(msg):
    print(f"[{datetime.datetime.now(datetime.timezone.utc):%H:%M:%S}] {msg}", file=sys.stderr, flush=True)


def wait_exec_ready(d, vm, t0):
    """Seconds from t0 until `true` exits 0 in vm (the same rule for both providers)."""
    end = t0 + READY_TIMEOUT_S
    last = None
    while time.monotonic() < end:
        try:
            if d.run_true(vm) == 0:
                return time.monotonic() - t0
        except (drivers.ApiError, OSError) as e:  # not up yet (409 on boat.dev, 5xx/conn while booting)
            last = e
        time.sleep(0.1)
    raise TimeoutError(f"{vm} not exec-ready after {READY_TIMEOUT_S}s (last error: {last})")


def ready(d, start):
    t0 = time.monotonic()
    vm = start()
    return vm, wait_exec_ready(d, vm, t0)


def gexec(d, vm, script, timeout=600):
    rc, out, err = d.exec(vm, script, timeout)
    if rc != 0:
        raise RuntimeError(f"exit {rc}: {(out + err)[-600:]}")
    return out


class Repeat:
    """One fresh VM and everything measured on it; cleans up what it made."""

    def __init__(self, d, run_id, index, parts):
        self.d, self.idx, self.parts = d, index, parts
        self.prefix = f"bb-{run_id}-{index}"
        self.rec = {"index": index, "metrics": {}, "errors": {}}
        self.vms, self.snaps = [], []

    def step(self, name, fn):
        log(f"r{self.idx} {name}")
        try:
            fn()
        except Exception as e:  # record and go on: one failed step must not hide the others
            self.rec["errors"][name] = f"{type(e).__name__}: {e}"
            log(f"r{self.idx} {name} FAILED: {e}")
            traceback.print_exc(file=sys.stderr)

    def new(self, start):
        vm, s = ready(self.d, start)
        self.vms.append(vm)
        return vm, s

    def run(self):
        m = self.rec["metrics"]
        try:
            a, m["create_ready_s"] = self.new(lambda: self.d.create(self.prefix + "-a"))
        except Exception as e:
            self.rec["errors"]["create"] = f"{type(e).__name__}: {e}"
            self.cleanup()
            return self.rec
        self.step("probe", lambda: self.rec.update(probe=W.parse_bb_json(gexec(self.d, a, W.PROBE))))
        if "b" in self.parts:
            self.step("exec_rtt", lambda: self.rtt(a))
        if "a" in self.parts or "f" in self.parts:
            self.step("node_setup", lambda: self.rec.update(node=W.parse_bb_json(gexec(self.d, a, W.NODE_SETUP))["node"]))
        if "a" in self.parts:
            self.step("node_loop", lambda: m.update(node_runs_s=[
                W.parse_geomean(gexec(self.d, a, W.NODE_RUN)) for _ in range(W.PTS_PASSES)]))
        if "d" in self.parts:
            self.step("disk", lambda: self.disk(a))
        if "e" in self.parts:
            self.step("net", lambda: self.net(a))
        if "f" in self.parts:
            self.step("npm_cold", lambda: self.npm(a))
        if "b" in self.parts:
            self.step("snapshot", lambda: self.snapshot(a))
            self.step("fork", lambda: self.fork(a))
            self.step("stop_resume", lambda: self.stop_resume(a))
        self.cleanup()
        return self.rec

    def rtt(self, vm):
        v, throttled = [], 0
        for _ in range(RTT_CALLS * 3):
            if len(v) == RTT_CALLS:
                break
            t = time.monotonic()
            try:
                rc = self.d.run_true(vm)
            except drivers.ApiError as e:
                if e.status != 429:
                    raise
                throttled += 1  # API rate limit: not an exec sample; wait and retry
                time.sleep(1)
                continue
            if rc == 0:
                v.append((time.monotonic() - t) * 1000)
        self.rec["exec_rtt_throttled"] = throttled
        self.rec["exec_rtt_ms"] = v
        self.rec["metrics"].update(exec_rtt_p50_ms=report.percentile(v, 50), exec_rtt_p95_ms=report.percentile(v, 95))

    def disk(self, vm):
        r = W.parse_bb_json(gexec(self.d, vm, W.DISK, 900))
        self.rec["disk_tool"] = r.pop("tool")
        self.rec["disk_errors"] = r.pop("errors", None)
        self.rec["metrics"].update(r)

    def net(self, vm):
        r = W.parse_bb_json(gexec(self.d, vm, W.NET))
        self.rec["net_errors"] = r.pop("errors", None)
        self.rec["metrics"].update(r)

    def npm(self, vm):
        for f in ("package.json", "package-lock.json"):
            with open(os.path.join(HERE, "npm-project", f), "rb") as fh:
                self.d.put_file(vm, f"{W.NPM_SRC}/{f}", fh.read())
        r = W.parse_bb_json(gexec(self.d, vm, W.NPM_COLD))
        self.rec["npm_packages"] = r["packages"]
        self.rec["metrics"]["npm_ci_s"] = r["npm_ci_s"]

    def snapshot(self, vm):
        t = time.monotonic()
        ref = self.d.snapshot(vm, self.prefix + "-snap")
        self.rec["metrics"]["snapshot_s"] = time.monotonic() - t
        self.snaps.append(ref)
        s_vm, self.rec["metrics"]["from_snapshot_ready_s"] = self.new(lambda: self.d.from_snapshot(ref, self.prefix + "-s"))
        self.drop(s_vm)

    def fork(self, vm):
        f_vm, self.rec["metrics"]["fork_ready_s"] = self.new(lambda: self.d.fork(vm, self.prefix + "-f"))
        self.drop(f_vm)

    def stop_resume(self, vm):
        gexec(self.d, vm, "sync")
        t = time.monotonic()
        self.d.stop(vm)
        self.rec["metrics"]["stop_s"] = time.monotonic() - t
        t = time.monotonic()
        self.d.resume(vm)
        self.rec["metrics"]["resume_ready_s"] = wait_exec_ready(self.d, vm, t)

    def drop(self, vm):
        try:
            self.d.delete(vm)
            self.vms.remove(vm)
        except Exception as e:
            log(f"delete {vm}: {e}")

    def cleanup(self):
        for vm in list(self.vms):
            self.drop(vm)
        for ref in self.snaps:
            try:
                self.d.delete_snapshot(ref)
            except Exception as e:
                log(f"delete snapshot {ref}: {e}")


def concurrent_create(d, run_id, index, n=CONCURRENT_N):
    """Wall time from one shared start until all n VMs answer `true`."""
    t0 = time.monotonic()

    def one(i):
        vm = d.create(f"bb-{run_id}-{index}-c{i}")
        return vm, wait_exec_ready(d, vm, t0)

    out, errs = [], []
    with concurrent.futures.ThreadPoolExecutor(n) as ex:
        for f in [ex.submit(one, i) for i in range(n)]:
            try:
                out.append(f.result())
            except Exception as e:
                errs.append(f"{type(e).__name__}: {e}")
    wall = time.monotonic() - t0
    for vm, _ in out:
        try:
            d.delete(vm)
        except Exception as e:
            log(f"delete {vm}: {e}")
    if errs:
        raise RuntimeError(f"{len(errs)}/{n} creates failed: {errs[0]}")
    return wall, sorted(s for _, s in out)


def client_info():
    model = ""
    try:
        model = next((ln.split(":", 1)[1].strip() for ln in open("/proc/cpuinfo") if ln.startswith("model name")), "")
    except OSError:
        model = subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"], capture_output=True, text=True).stdout.strip()
    egress = "-"
    try:
        with urllib.request.urlopen("https://www.cloudflare.com/cdn-cgi/trace", timeout=10) as r:
            t = dict(ln.split("=", 1) for ln in r.read().decode().splitlines() if "=" in ln)
            egress = f"{t.get('colo')} {t.get('loc')}"
    except OSError:
        pass
    return {"cpu_model": f"{model} x{os.cpu_count()}", "os": platform.platform(), "egress": egress}


def cmd_run(a):
    d = drivers.DRIVERS[a.driver]()
    run_id = secrets.token_hex(3)
    parts = set(a.only.replace(",", ""))
    res = {"driver": a.driver, "run_id": run_id, "size": d.size,
           "started": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
           "host": {"desc": os.environ.get("BOATBENCH_HOST_DESC", "-")},
           "boatd_options": os.environ.get("BOATBENCH_BOATD_OPTIONS") or "-", "run_url": os.environ.get("BOATBENCH_RUN_URL"),
           "client": client_info(), "parts": "".join(sorted(parts)), "repeats": []}
    os.makedirs(a.out, exist_ok=True)
    path = os.path.join(a.out, f"results-{a.driver}-{res['started'].replace(':', '')}.json")
    for i in range(a.repeats):
        rec = Repeat(d, run_id, i, parts).run()
        if "c" in parts:
            log(f"r{i} concurrent {CONCURRENT_N}")
            try:
                rec["metrics"]["concurrent10_wall_s"], rec["concurrent_each_s"] = concurrent_create(d, run_id, i)
            except Exception as e:
                rec["errors"]["concurrent"] = f"{type(e).__name__}: {e}"
        res["repeats"].append(rec)
        with open(path, "w") as f:  # rewrite after every repeat: a crash keeps what was measured
            json.dump(res, f, indent=1)
        log(f"r{i} done: {json.dumps(rec['metrics'])}")
    print(path)
    failed = all(r["errors"].get("create") for r in res["repeats"])
    return 1 if failed else 0


def cmd_report(a):
    results = []
    for p in a.results:
        with open(p) as f:
            results.append(json.load(f))
    md = report.render(results, report.load_prices(a.prices), report.load_published())
    if a.out:
        with open(a.out, "w") as f:
            f.write(md)
    print(md)
    return 0


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--driver", choices=sorted(drivers.DRIVERS), required=True)
    r.add_argument("--repeats", type=int, default=5)
    r.add_argument("--only", default=ALL_PARTS, help="workload letters, e.g. 'a,b' (default all: abcdef)")
    r.add_argument("--out", default="boatbench-results")
    g = sub.add_parser("report")
    g.add_argument("results", nargs="+")
    g.add_argument("--prices", default=os.path.join(HERE, "prices.json"))
    g.add_argument("--out")
    a = p.parse_args(argv)
    if a.cmd == "run" and (a.repeats < 1 or not set(a.only.replace(",", "")) <= set(ALL_PARTS)):
        p.error("--repeats must be >= 1 and --only a subset of abcdef")
    if a.cmd == "run" and a.driver == "host" and not set(a.only.replace(",", "")) <= drivers.Host.PARTS:
        p.error("--driver host only runs workloads a and d")
    return cmd_run(a) if a.cmd == "run" else cmd_report(a)


if __name__ == "__main__":
    sys.exit(main())
