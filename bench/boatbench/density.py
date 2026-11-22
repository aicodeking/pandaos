"""Workload (g): density under load. N VMs run the Node loop (a) at the same time.

The largest level's VMs are created at once, each a fresh VM set up on its own
(NODE_SETUP); the ones the provider refuses are left out. Not forks: on PandaOS a
long exec on a fork's SOURCE VM never returned (502 after timeout_s, run
42818758694), and forks would share page cache. For each level n, n of those VMs start DENSITY_PASSES passes of the
Node loop together, from one barrier. Per level the result holds:

  per_vm_runs_s   median of each VM's passes (geometric-mean index units, as in (a))
  host_runs_s     their sum: aggregate geometric-mean index for the host (PandaOS: all
                  VMs are on one host; boat.dev: on its fleet, so only the
                  per-VM number means anything there)
  cpu_per_loop    (user + sys) / real of `node` inside the guest, median
  efficiency      per_vm_runs_s(n) / per_vm_runs_s(1)
  loops_per_cpu   n x cpu_per_loop(1) / host CPUs (PandaOS, host CPUs = the
                  CPUs online where the harness runs: true in CI and in
                  run-on-server.sh run on the server; else set BOATBENCH_HOST_CPUS)

host_runs_s is a sum of geometric-mean indices, not completed jobs/s.
Completed-suite throughput requires successful pass counts / level wall_s.
"""
import concurrent.futures
import json
import math
import os
import re
import statistics
import threading
import time

from cleanup import cleanup_vms
import workloads as W

DEFAULT_LEVELS = (1, 2, 4, 6)
DENSITY_PASSES = 2
DEFAULT_WORKLOAD = "cpu"
WORKLOADS = ("cpu", "memory")
DEFAULT_MEM_MIB = 1024
DEFAULT_MEM_SWEEPS = 3
_CPU = re.compile(r"BB_CPU ([\d.]+) ([\d.]+) ([\d.]+)")
_MEM = re.compile(r"^BB_MEM\s+(\{.*\})\s*$", re.M)
RUN = ("TIMEFORMAT='BB_CPU %R %U %S'; { time ( " + W.NODE_RUN + " ) ; } 2>&1")
PASS_TIMEOUT_S = 900  # one solo pass takes about 1 min on the runner; 6 on 2 CPUs about 8

_MEM_PY = "\n".join([
    "import json, sys, time",
    "mib, sweeps = int(sys.argv[1]), int(sys.argv[2])",
    "n = mib * 1024 * 1024",
    "pages = (n + 4095) // 4096",
    "t0 = time.monotonic()",
    "buf = bytearray(n)",
    "for s in range(sweeps):",
    "    b = (s + 1) & 255",
    "    buf[0:n:4096] = bytes([b]) * pages",
    "dt = time.monotonic() - t0",
    "rss = None",
    "try:",
    "    for ln in open('/proc/self/status'):",
    "        if ln.startswith('VmRSS:'):",
    "            rss = round(int(ln.split()[1]) / 1024.0, 2)",
    "            break",
    "except OSError:",
    "    pass",
    "rate = round((mib * sweeps) / dt, 3) if dt > 0 else 0.0",
    "print('BB_MEM ' + json.dumps({'mem_mib': mib, 'sweeps': sweeps, 'elapsed_s': round(dt, 6), 'mib_per_s': rate, 'rss_mib': rss}))",
])


def workload_from_env(env=None):
    raw = (env if env is not None else os.environ).get("BOATBENCH_DENSITY_WORKLOAD", "").strip().lower()
    if not raw:
        return DEFAULT_WORKLOAD
    if raw not in WORKLOADS:
        raise ValueError(f"BOATBENCH_DENSITY_WORKLOAD must be one of {WORKLOADS}: {raw!r}")
    return raw


def mem_mib_from_env(env=None):
    raw = (env if env is not None else os.environ).get("BOATBENCH_DENSITY_MEM_MIB", "").strip()
    if not raw:
        return DEFAULT_MEM_MIB
    val = int(raw)
    if val < 1:
        raise ValueError(f"BOATBENCH_DENSITY_MEM_MIB must be a positive integer: {raw!r}")
    return val


def mem_run_script(mem_mib, sweeps=DEFAULT_MEM_SWEEPS):
    """Allocate and dirty every 4 KiB page of a mem_mib anonymous working set across sweeps."""
    if not isinstance(mem_mib, int) or isinstance(mem_mib, bool) or mem_mib < 1:
        raise ValueError(f"mem_mib must be a positive integer: {mem_mib!r}")
    if not isinstance(sweeps, int) or isinstance(sweeps, bool) or sweeps < 1:
        raise ValueError(f"sweeps must be a positive integer: {sweeps!r}")
    py_quoted = "'" + _MEM_PY.replace("'", "'\"'\"'") + "'"
    cmd = f"python3 -c {py_quoted} {mem_mib} {sweeps}"
    return "TIMEFORMAT='BB_CPU %R %U %S'; { time ( " + cmd + " ) ; } 2>&1"


def parse_mem_info(out):
    m = _MEM.search(out or "")
    if not m:
        return None
    try:
        data = json.loads(m.group(1))
    except ValueError:
        return None
    return data if isinstance(data, dict) else None


def parse_mem(out):
    """Memory touch throughput (MiB/s) of a mem_run_script pass, or None."""
    data = parse_mem_info(out)
    if not data:
        return None
    rate = data.get("mib_per_s")
    return float(rate) if positive_finite(rate) else None


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


def positive_finite(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value > 0


def level_complete(lv):
    """Admit complete trials, including historical summaries without pass counts."""
    n = lv.get("n")
    each = lv.get("per_vm_each", [])
    return (isinstance(n, int) and not isinstance(n, bool) and n > 0
            and lv.get("complete", True) is True and lv.get("vms_measured") == n
            and len(each) == n and all(positive_finite(x) for x in each)
            and all(positive_finite(lv.get(k)) for k in ("per_vm_runs_s", "host_runs_s", "cpu_per_loop"))
            and ("pass_counts" not in lv or lv["pass_counts"] == [lv.get("passes_required")] * n
                 and positive_finite(lv.get("passes_required"))))


def summarize_level(n, passes, cpus):
    """Only full sets of valid passes may supply an N-worker measurement."""
    valid = [vm for vm in passes if len(vm) == DENSITY_PASSES
             and all(positive_finite(r) and positive_finite(c) for r, c in vm)]
    per_vm = [statistics.median(r for r, _ in vm) for vm in valid]
    cpu = [c for vm in valid for _, c in vm]
    complete = len(passes) == n and len(valid) == n
    return {"n": n, "vms_measured": len(valid), "complete": complete,
            "pass_counts": [len(vm) for vm in passes], "passes_required": DENSITY_PASSES,
            "per_vm_runs_s": statistics.median(per_vm) if complete else None,
            "per_vm_each": per_vm, "host_runs_s": sum(per_vm) if complete else None,
            "cpu_per_loop": statistics.median(cpu) if complete else None, "host_cpus": cpus}


def finish(levels):
    """Add efficiency and loops_per_cpu relative to the n=1 level."""
    solo = next((lv for lv in levels if lv["n"] == 1 and level_complete(lv)), None)
    out = []
    for lv in levels:
        e = dict(lv)
        if solo and level_complete(lv):
            e["efficiency"] = lv["per_vm_runs_s"] / solo["per_vm_runs_s"]
        if solo and level_complete(lv) and positive_finite(lv.get("host_cpus")):
            e["loops_per_cpu"] = lv["n"] * solo["cpu_per_loop"] / lv["host_cpus"]
        out.append(e)
    return out


def metrics_of(levels, workload=DEFAULT_WORKLOAD):
    m = {}
    for lv in levels:
        if not level_complete(lv):
            continue
        n = lv["n"]
        if workload == "memory":
            if lv.get("host_mib_s") is not None:
                m[f"density_mem_host_mib_s_n{n}"] = lv["host_mib_s"]
            if lv.get("per_vm_mib_s") is not None:
                m[f"density_mem_per_vm_mib_s_n{n}"] = lv["per_vm_mib_s"]
            for k in ("efficiency", "cpu_per_loop"):
                if lv.get(k) is not None:
                    m[f"density_mem_{k}_n{n}"] = lv[k]
        else:
            for k in ("host_runs_s", "per_vm_runs_s", "efficiency", "cpu_per_loop"):
                if lv.get(k) is not None:
                    m[f"density_{k}_n{n}"] = lv[k]
    return m


class Density:
    def __init__(self, d, prefix, levels, log, gexec, ready,
                 workload=DEFAULT_WORKLOAD, mem_mib=DEFAULT_MEM_MIB, mem_sweeps=DEFAULT_MEM_SWEEPS):
        if workload not in WORKLOADS:
            raise ValueError(f"unsupported density workload: {workload!r} (expected one of {WORKLOADS})")
        if not isinstance(mem_mib, int) or isinstance(mem_mib, bool) or mem_mib < 1:
            raise ValueError(f"mem_mib must be a positive integer: {mem_mib!r}")
        if not isinstance(mem_sweeps, int) or isinstance(mem_sweeps, bool) or mem_sweeps < 1:
            raise ValueError(f"mem_sweeps must be a positive integer: {mem_sweeps!r}")
        self.d, self.prefix, self.levels, self.log = d, prefix, levels, log
        self.gexec, self.ready = gexec, ready
        self.workload = workload
        self.mem_mib = mem_mib
        self.mem_sweeps = mem_sweeps
        self.vms, self.broken, self.errors = [], [], []
        self.vms_made, self.cleanup_pending_vms, self.cleanup_errors = [], [], {}

    def one_vm(self, i):
        vm = None

        def start():
            nonlocal vm
            vm = self.d.create(f"{self.prefix}-g{i}")
            return vm

        try:
            vm, _ = self.ready(self.d, start)
            if self.workload == "cpu":
                self.gexec(self.d, vm, W.NODE_SETUP)
            return vm
        except Exception as e:  # refused (capacity) or broken: measure without it
            self.errors.append(f"vm {i}: {type(e).__name__}: {e}")
            return ("failed", vm)

    def grow(self, want):
        """`want` fresh VMs at once, each set up; the provider's refusals are left out."""
        if self.d.name == "host":  # no VMs: set Node up once (for cpu), then N loops run side by side on the host
            if self.workload == "cpu":
                self.gexec(self.d, "host", W.NODE_SETUP)
            self.vms = ["host"] * want
            self.vms_made = list(self.vms)
            return
        with concurrent.futures.ThreadPoolExecutor(want) as ex:
            got = list(ex.map(self.one_vm, range(want)))
        self.vms = [v for v in got if isinstance(v, str)]
        self.broken = [v[1] for v in got if isinstance(v, tuple) and v[1]]
        self.vms_made = list(self.vms)
        self.cleanup_pending_vms.extend(dict.fromkeys(self.vms + self.broken))
        self.log(f"density: {len(self.vms)} of {want} VMs set up")

    def level(self, n, cpus):
        vms = self.vms[:n]
        barrier = threading.Barrier(n)
        script = RUN if self.workload == "cpu" else mem_run_script(self.mem_mib, self.mem_sweeps)
        parse_rate = W.parse_geomean if self.workload == "cpu" else parse_mem

        def one(vm):
            barrier.wait(timeout=600)
            out = []
            for _ in range(DENSITY_PASSES):
                o = self.gexec(self.d, vm, script, PASS_TIMEOUT_S)
                sample = (parse_rate(o), parse_cpu(o))
                if not all(positive_finite(x) for x in sample):
                    raise ValueError("invalid density throughput or CPU measurement")
                out.append(sample)
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
        if self.workload == "memory":
            lv["workload"] = "memory"
            lv["mem_mib"] = self.mem_mib
            lv["working_set_mib"] = n * self.mem_mib
            lv["per_vm_mib_s"] = lv["per_vm_runs_s"]
            lv["host_mib_s"] = lv["host_runs_s"]
        return lv

    def run(self):
        cpus = host_cpus() if self.d.name in ("pandaos", "pandaos-compat", "host") else None
        try:
            self.grow(max(self.levels))
            done = [self.level(n, cpus) for n in self.levels if n <= len(self.vms)]
        finally:
            self.cleanup()
        levels = finish(done)
        rec = {"levels": levels, "max_vms": len(self.vms_made), "requested": list(self.levels),
               "passes": DENSITY_PASSES, "errors": self.errors,
               "cleanup_pending_vms": self.cleanup_pending_vms,
               "cleanup_errors": self.cleanup_errors}
        if self.workload != DEFAULT_WORKLOAD:
            rec["workload"] = self.workload
            rec["mem_mib"] = self.mem_mib
        return rec, metrics_of(levels, workload=self.workload)

    def cleanup(self):
        cleanup_vms(self.d, self.cleanup_pending_vms, self.cleanup_errors, self.log)
        self.vms = [vm for vm in self.vms if vm in self.cleanup_pending_vms]
        self.broken = [vm for vm in self.broken if vm in self.cleanup_pending_vms]
