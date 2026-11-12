"""Workload (g): density under load. N VMs run the Node loop (a) at the same time.

The largest level's VMs are created at once, each a fresh VM set up on its own
(NODE_SETUP); the ones the provider refuses are left out. Not forks: on PandaOS a
long exec on a fork's SOURCE VM never returned (502 after timeout_s, run
42818758694), and forks would share page cache. For each level n, n of those VMs start DENSITY_PASSES passes of the
Node loop together, from one barrier. Per level the result holds:

  per_vm_runs_s   median of each VM's passes (geomean runs/s, as in (a))
  host_runs_s     their sum: the throughput of the whole host (PandaOS: all
                  VMs are on one host; boat.dev: on its fleet, so only the
                  per-VM number means anything there)
  cpu_per_loop    (user + sys) / real of `node` inside the guest, median
  efficiency      per_vm_runs_s(n) / per_vm_runs_s(1)
  loops_per_cpu   n x cpu_per_loop(1) / host CPUs (PandaOS, host CPUs = the
                  CPUs online where the harness runs: true in CI and in
                  run-on-server.sh run on the server; else set BOATBENCH_HOST_CPUS)

A host's cost per run is its price per hour over host_runs_s, not over one
uncontended VM's speed.
"""
import concurrent.futures
import os
import re
import statistics
import threading
import time

import workloads as W

DEFAULT_LEVELS = (1, 2, 4, 6)
DENSITY_PASSES = 2
_CPU = re.compile(r"BB_CPU ([\d.]+) ([\d.]+) ([\d.]+)")
RUN = ("TIMEFORMAT='BB_CPU %R %U %S'; { time ( " + W.NODE_RUN + " ) ; } 2>&1")
PASS_TIMEOUT_S = 900  # one solo pass takes about 1 min on the runner; 6 on 2 CPUs about 8


def levels_from_env(env=None):
    raw = (env if env is not None else os.environ).get("BOATBENCH_DENSITY_LEVELS", "")
    if not raw.strip():
        return DEFAULT_LEVELS
    lv = sorted({int(x) for x in raw.replace(",", " ").split()})
    if not lv or lv[0] < 1:
        raise ValueError(f"BOATBENCH_DENSITY_LEVELS must be positive integers: {raw!r}")
    return tuple(lv)


def host_cpus(env=None):
    raw = (env if env is not None else os.environ).get("BOATBENCH_HOST_CPUS", "")
    if raw.strip():
        return int(raw)
    try:
        return len(os.sched_getaffinity(0))
    except AttributeError:  # macOS
        return os.cpu_count()


def parse_cpu(out):
    """(user + sys) / real of the timed node run, or None."""
    m = _CPU.search(out)
    if not m:
        return None
    real, user, sys_ = (float(x) for x in m.groups())
    return (user + sys_) / real if real > 0 else None


def summarize_level(n, passes, cpus):
    """passes: one list per VM of (runs_s, cpu_per_loop) tuples."""
    per_vm = [statistics.median(r for r, _ in vm) for vm in passes if vm]
    cpu = [c for vm in passes for _, c in vm if c is not None]
    return {"n": n, "vms_measured": len(per_vm), "per_vm_runs_s": statistics.median(per_vm) if per_vm else None,
            "per_vm_each": per_vm, "host_runs_s": sum(per_vm) if per_vm else None,
            "cpu_per_loop": statistics.median(cpu) if cpu else None, "host_cpus": cpus}


def finish(levels):
    """Add efficiency and loops_per_cpu relative to the n=1 level."""
    solo = next((lv for lv in levels if lv["n"] == 1 and lv.get("per_vm_runs_s")), None)
    out = []
    for lv in levels:
        e = dict(lv)
        if solo and lv.get("per_vm_runs_s"):
            e["efficiency"] = lv["per_vm_runs_s"] / solo["per_vm_runs_s"]
        if solo and solo.get("cpu_per_loop") and lv.get("host_cpus"):
            e["loops_per_cpu"] = lv["n"] * solo["cpu_per_loop"] / lv["host_cpus"]
        out.append(e)
    return out


def metrics_of(levels):
    m = {}
    for lv in levels:
        n = lv["n"]
        for k in ("host_runs_s", "per_vm_runs_s", "efficiency", "cpu_per_loop"):
            if lv.get(k) is not None:
                m[f"density_{k}_n{n}"] = lv[k]
    return m


class Density:
    def __init__(self, d, prefix, levels, log, gexec, ready):
        self.d, self.prefix, self.levels, self.log = d, prefix, levels, log
        self.gexec, self.ready = gexec, ready
        self.vms, self.broken, self.errors = [], [], []

    def one_vm(self, i):
        vm = None
        try:
            vm, _ = self.ready(self.d, lambda: self.d.create(f"{self.prefix}-g{i}"))
            self.gexec(self.d, vm, W.NODE_SETUP)
            return vm
        except Exception as e:  # refused (capacity) or broken: measure without it
            self.errors.append(f"vm {i}: {type(e).__name__}: {e}")
            return ("failed", vm)

    def grow(self, want):
        """`want` fresh VMs at once, each set up; the provider's refusals are left out."""
        if self.d.name == "host":  # no VMs: set Node up once, then N loops run side by side on the host
            self.gexec(self.d, "host", W.NODE_SETUP)
            self.vms = ["host"] * want
            return
        with concurrent.futures.ThreadPoolExecutor(want) as ex:
            got = list(ex.map(self.one_vm, range(want)))
        self.vms = [v for v in got if isinstance(v, str)]
        self.broken = [v[1] for v in got if isinstance(v, tuple) and v[1]]
        self.log(f"density: {len(self.vms)} of {want} VMs set up")

    def level(self, n, cpus):
        vms = self.vms[:n]
        barrier = threading.Barrier(n)

        def one(vm):
            barrier.wait(timeout=600)
            out = []
            for _ in range(DENSITY_PASSES):
                o = self.gexec(self.d, vm, RUN, PASS_TIMEOUT_S)
                out.append((W.parse_geomean(o), parse_cpu(o)))
            return out

        t = time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(n) as ex:
            futs = [ex.submit(one, vm) for vm in vms]
            passes = []
            for f in futs:
                try:
                    passes.append(f.result())
                except Exception as e:
                    self.errors.append(f"n={n}: {type(e).__name__}: {e}")
                    passes.append([])
        lv = summarize_level(n, passes, cpus)
        lv["wall_s"] = time.monotonic() - t
        return lv

    def run(self):
        cpus = host_cpus() if self.d.name in ("pandaos", "host") else None
        try:
            self.grow(max(self.levels))
            done = [self.level(n, cpus) for n in self.levels if n <= len(self.vms)]
        finally:
            self.cleanup()
        levels = finish(done)
        return {"levels": levels, "max_vms": len(self.vms_made), "requested": list(self.levels),
                "passes": DENSITY_PASSES, "errors": self.errors}, metrics_of(levels)

    def cleanup(self):
        self.vms_made = list(self.vms)
        for vm in [v for v in self.vms if v != "host"] + self.broken:
            try:
                self.d.delete(vm)
            except Exception as e:
                self.log(f"delete {vm}: {e}")
        self.vms, self.broken = [], []
