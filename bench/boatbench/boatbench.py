#!/usr/bin/env python3
"""boatbench: the same workloads on PandaOS boat and on boat.dev.

  boatbench.py run --driver pandaos|boatdev [--repeats 5] [--only a,b,c,d,e,f,g] [--out DIR]
  boatbench.py run --driver host --only ad [--repeats 5] [--out DIR]   # same scripts, no VM
  boatbench.py report RESULTS.json... [--prices prices.json] [--server ax42|ax102|...] [--out report.md]
  boatbench.py presets RESULTS.json... [--out presets.md]   # PandaOS per boatd preset
  boatbench.py costs [--prices prices.json] [--out costs.md]   # price-only tables (no results)

pandaos needs BOAT_URL + BOAT_KEY; boatdev needs BOATDEV_API_KEY. Each repeat is a
fresh 4 vCPU / 8 GiB VM. Raw per-repeat data goes to DIR/results-<driver>-<ts>.json.
Optional env: BOATBENCH_HOST_DESC (hardware/location of the provider host, printed
in the report), BOATBENCH_RUN_URL, BOATBENCH_BOATD_OPTIONS and BOATBENCH_PRESET
(printed in the report), BOATBENCH_DENSITY_LEVELS (workload g, default 1,2,4,6),
BOATBENCH_HOST_CPUS (workload g: CPUs of the PandaOS host if the harness runs elsewhere),
BOATBENCH_CONCURRENT_N (workload c, default 10: VMs created at once), BB_NODE_LOADER (host
driver on NixOS: the ELF loader command that runs the pinned Node binary),
BOATBENCH_SOURCE_COMMIT (the boat / PandaOS commit under test; default GITHUB_SHA, then
`nixos-version --configuration-revision`, else "unknown"; recorded as source_commit).

Every repeat that runs b or c first records the API round-trip baseline: API_RTT_CALLS
cheap authenticated GETs of the VM (api_rtt_p50_ms). Lifecycle metrics then also get
a `<metric>_net` value: raw minus the driver's ROUND_TRIPS x that p50, so a localhost
API and one across the WAN are compared on equal footing.
"""
import argparse
import concurrent.futures
import datetime
import json
import os
import platform
import re
import secrets
import subprocess
import sys
import time
import traceback
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import admission  # noqa: E402
import cost_tables  # noqa: E402
import density  # noqa: E402
from cleanup import cleanup_vms  # noqa: E402
import drivers  # noqa: E402
import report  # noqa: E402
import workloads as W  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_BOATD_CONFIG = "/etc/boat/boatd.json"
READY_TIMEOUT_S = 600
RTT_CALLS = 50
API_RTT_CALLS = 25
CONCURRENT_N = 10
ALL_PARTS = "abcdefg"


def log(msg):
    print(f"[{datetime.datetime.now(datetime.timezone.utc):%H:%M:%S}] {msg}", file=sys.stderr, flush=True)


def _op_calls(d):
    tc = getattr(d, "thread_calls", None)
    if callable(tc):
        return tc()
    hc = getattr(d, "http_calls", None)
    return hc if isinstance(hc, int) else None


def _run_true_poll(d, vm):
    try:
        return d.run_true(vm, poll=True)
    except TypeError as exc:
        if "poll" not in str(exc):
            raise
        return d.run_true(vm)


def wait_exec_ready(d, vm, t0):
    """Seconds from t0 until `true` exits 0 in vm (the same rule for both providers)."""
    end = t0 + READY_TIMEOUT_S
    last = None
    attempt = 0
    while time.monotonic() < end:
        try:
            if _run_true_poll(d, vm) == 0:
                elapsed = time.monotonic() - t0
                calls = getattr(d, "_calls", None)
                note = getattr(calls, "note_serial_poll", None) or getattr(d, "note_serial_poll", None)
                if callable(note):
                    note(1)
                return elapsed
        except (drivers.ApiError, OSError) as e:  # not up yet (409 on boat.dev, 5xx/conn while booting)
            last = e
        time.sleep(drivers.poll_sleep(attempt))
        attempt += 1
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

    def __init__(self, d, run_id, index, parts, max_vms=None):
        self.d, self.idx, self.parts, self.max_vms = d, index, parts, max_vms
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

    def _record_calls(self, metric, before):
        after = _op_calls(self.d)
        if before is not None and after is not None:
            self.rec.setdefault("api_calls", {})[metric] = max(0, after - before)

    def new(self, start, metric=None):
        c0 = _op_calls(self.d)
        t0 = time.monotonic()
        vm = start()
        self.vms.append(vm)
        elapsed = wait_exec_ready(self.d, vm, t0)
        if metric:
            self._record_calls(metric, c0)
        return vm, elapsed

    def run(self):
        m = self.rec["metrics"]
        try:
            a, m["create_ready_s"] = self.new(lambda: self.d.create(self.prefix + "-a"), "create_ready_s")
        except Exception as e:
            self.rec["errors"]["create"] = f"{type(e).__name__}: {e}"
            self.cleanup()
            return self.rec
        self.step("probe", lambda: self.rec.update(probe=W.parse_bb_json(gexec(self.d, a, W.PROBE))))
        if "b" in self.parts or "c" in self.parts:
            self.step("api_rtt", lambda: self.api_rtt(a))
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
            if self.max_vms is not None and self.max_vms < 2:
                reason = f"skipped: host admits {self.max_vms} VMs"
                self.rec.setdefault("skipped", {})["snapshot"] = reason
                self.rec["skipped"]["fork"] = reason
                log(f"r{self.idx} snapshot {reason}")
                log(f"r{self.idx} fork {reason}")
            else:
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
        if v and _op_calls(self.d) is not None:
            self.rec.setdefault("api_calls", {})["exec_rtt_p50_ms"] = 1
        self.rec["metrics"].update(exec_rtt_p50_ms=report.percentile(v, 50), exec_rtt_p95_ms=report.percentile(v, 95))

    def api_rtt(self, vm):
        v = []
        for _ in range(API_RTT_CALLS):
            t = time.monotonic()
            self.d.ping(vm)
            v.append((time.monotonic() - t) * 1000)
        self.rec["api_rtt_ms"] = v
        self.rec["metrics"].update(api_rtt_p50_ms=report.percentile(v, 50), api_rtt_p95_ms=report.percentile(v, 95))

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
        c0 = _op_calls(self.d)
        t = time.monotonic()
        ref = self.d.snapshot(vm, self.prefix + "-snap", on_accepted=self.snaps.append)
        self.rec["metrics"]["snapshot_s"] = time.monotonic() - t
        self._record_calls("snapshot_s", c0)
        s_vm, self.rec["metrics"]["from_snapshot_ready_s"] = self.new(
            lambda: self.d.from_snapshot(ref, self.prefix + "-s"), "from_snapshot_ready_s")
        self.drop(s_vm)

    def fork(self, vm):
        f_vm, self.rec["metrics"]["fork_ready_s"] = self.new(
            lambda: self.d.fork(vm, self.prefix + "-f"), "fork_ready_s")
        self.drop(f_vm)

    def stop_resume(self, vm):
        gexec(self.d, vm, "sync")
        c0 = _op_calls(self.d)
        t = time.monotonic()
        self.d.stop(vm)
        self.rec["metrics"]["stop_s"] = time.monotonic() - t
        self._record_calls("stop_s", c0)
        c1 = _op_calls(self.d)
        t = time.monotonic()
        self.d.resume(vm)
        self.rec["metrics"]["resume_ready_s"] = wait_exec_ready(self.d, vm, t)
        self._record_calls("resume_ready_s", c1)

    def drop(self, vm):
        try:
            self.d.delete(vm)
            self.vms.remove(vm)
            self.rec["errors"].pop(f"cleanup_vm:{vm}", None)
        except Exception as e:
            self.rec["errors"][f"cleanup_vm:{vm}"] = f"{type(e).__name__}: {e}"
            log(f"delete {vm}: {e}")

    def cleanup(self):
        for vm in list(self.vms):
            self.drop(vm)
        for ref in list(self.snaps):
            try:
                self.d.delete_snapshot(ref)
                self.snaps.remove(ref)
                self.rec["errors"].pop(f"cleanup_snapshot:{ref}", None)
            except Exception as e:
                self.rec["errors"][f"cleanup_snapshot:{ref}"] = f"{type(e).__name__}: {e}"
                log(f"delete snapshot {ref}: {e}")


def concurrent_create(d, run_id, index, n=CONCURRENT_N, api_calls_out=None, cleanup_out=None):
    """Wall time from one shared start until all n VMs answer `true`."""
    t0 = time.monotonic()
    created = []

    def one(i):
        c0 = _op_calls(d)
        vm = d.create(f"bb-{run_id}-{index}-c{i}")
        created.append(vm)
        elapsed = wait_exec_ready(d, vm, t0)
        c1 = _op_calls(d)
        calls = max(0, c1 - c0) if (c0 is not None and c1 is not None) else None
        return vm, elapsed, calls

    out, errs = [], []
    with concurrent.futures.ThreadPoolExecutor(n) as ex:
        for f in [ex.submit(one, i) for i in range(n)]:
            try:
                out.append(f.result())
            except Exception as e:
                errs.append(f"{type(e).__name__}: {e}")
    wall = time.monotonic() - t0
    cleanup_errors = {}
    cleanup_vms(d, created, cleanup_errors, log)
    if cleanup_out is not None:
        cleanup_out.update(pending_vms=created, errors=cleanup_errors)
    problems = ([f"{len(errs)}/{n} creates failed: {errs[0]}"] if errs else [])
    if cleanup_errors:
        problems.append(f"unresolved VM cleanup: {cleanup_errors}")
    if problems:
        raise RuntimeError("; ".join(problems))
    if api_calls_out is not None and out:
        slowest = max(out, key=lambda x: x[1])
        if slowest[2] is not None:
            api_calls_out[f"concurrent{n}_wall_s"] = slowest[2]
    return wall, sorted(s for _, s, _ in out)


def add_net(metrics, round_trips, api_calls=None):
    """New dict: metrics plus `<k>_net` = raw minus serial api_calls[k] (else round_trips[k]) x API RTT p50, >= 0."""
    rtt = metrics.get("api_rtt_p50_ms")
    if rtt is None:
        return dict(metrics)
    out = dict(metrics)
    calls = api_calls or {}
    keys = list(dict.fromkeys([*round_trips.keys(), *calls.keys(), *(k for k in out if k.startswith("concurrent") and k.endswith("_wall_s"))]))
    for k in keys:
        n = calls.get(k, round_trips.get(k))
        if n is None and k.startswith("concurrent") and k.endswith("_wall_s"):
            n = round_trips.get("concurrent10_wall_s")
        if n is not None and out.get(k) is not None:
            if k.endswith("_ms"):
                val = max(0.0, round(out[k] - n * rtt, 3))
            else:
                val = max(0.0, round(out[k] - n * (rtt / 1000), 6))
            out[k + "_net"] = val
    if out.get("exec_rtt_p50_ms") is not None:
        n50 = calls.get("exec_rtt_p50_ms", round_trips.get("exec_rtt_p50_ms", 1))
        p50_net = max(0.0, round(out["exec_rtt_p50_ms"] - n50 * rtt, 3))
        out["exec_rtt_p50_ms_net"] = p50_net
        out["exec_rtt_p50_net_ms"] = p50_net
    if out.get("exec_rtt_p95_ms") is not None:
        n95 = calls.get("exec_rtt_p95_ms", round_trips.get("exec_rtt_p95_ms", 1))
        p95_net = max(0.0, round(out["exec_rtt_p95_ms"] - n95 * rtt, 3))
        out["exec_rtt_p95_ms_net"] = p95_net
        out["exec_rtt_p95_net_ms"] = p95_net
    return out


def read_boatd_config(config_path=None):
    """Read deployed /etc/boat/boatd.json (or BOATD_CONFIG / config_path) when present."""
    path = config_path if config_path is not None else (os.environ.get("BOATD_CONFIG") or DEFAULT_BOATD_CONFIG)
    if not path or not os.path.isfile(path):
        return None
    try:
        with open(path) as f:
            data = json.load(f)
        return data if isinstance(data, dict) else None
    except (OSError, ValueError):
        return None


def _cache_from_boatd_json(cfg):
    dc = str(cfg.get("diskCache") or "").strip()
    if dc:
        if dc.lower() == "writeback":
            return "Writeback"
        if dc.lower() == "unsafe":
            return "Unsafe"
        return dc
    if cfg.get("unsafeCache") is False:
        return "Writeback"
    return "Unsafe"


def host_memtotal_mib(meminfo_path="/proc/meminfo"):
    """Host MemTotal in MiB from /proc/meminfo, or None when unreadable (e.g. non-Linux)."""
    try:
        with open(meminfo_path) as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    parts = line.split()
                    if len(parts) >= 2:
                        return int(parts[1]) // 1024
    except (OSError, ValueError):
        pass
    return None


def _effective_boatd_config(options=None, config_path=None):
    cfg = read_boatd_config(config_path)
    opt = None
    if options and str(options).strip() not in ("", "-"):
        try:
            parsed = json.loads(options)
            if isinstance(parsed, dict):
                opt = parsed
        except ValueError:
            pass
    if cfg is None and opt is None:
        return None
    merged_cfg = dict(cfg or {})
    if opt:
        merged_cfg.update(opt)
    return merged_cfg


def host_admitted_vms(options=None, config_path=None, size="default", memtotal_mib=None, cpus=None):
    """Derive the number of active VMs of `size` boatd admits on this host via admission.py, or None if unknown."""
    cfg = _effective_boatd_config(options=options, config_path=config_path)
    if cfg is None:
        return None
    mt = host_memtotal_mib() if memtotal_mib is None else memtotal_mib
    if mt is None or mt <= 0:
        return None
    threads = density.host_cpus() if cpus is None else cpus
    if not threads or threads <= 0:
        return None
    d = dict(admission.boatd_defaults())
    if isinstance(cfg.get("hostReserveMiB"), (int, float)) and not isinstance(cfg.get("hostReserveMiB"), bool):
        d["host_reserve_mib"] = int(cfg["hostReserveMiB"])
    if isinstance(cfg.get("vmOverheadMiB"), (int, float)) and not isinstance(cfg.get("vmOverheadMiB"), bool):
        d["vm_overhead_mib"] = int(cfg["vmOverheadMiB"])
    builtin_sizes = {
        "small": (2, 4096),
        "default": (d["vm_vcpu"], d["vm_mem_mib"]),
        "large": (8, 16384),
        "xlarge": (16, 32768),
    }
    vcpu, mem_mib = builtin_sizes.get(size, (d["vm_vcpu"], d["vm_mem_mib"]))
    cfg_sizes = cfg.get("sizes")
    if isinstance(cfg_sizes, dict) and isinstance(cfg_sizes.get(size), dict):
        entry = cfg_sizes[size]
        if isinstance(entry.get("vcpu"), (int, float)) and not isinstance(entry.get("vcpu"), bool):
            vcpu = int(entry["vcpu"])
        if isinstance(entry.get("memMiB"), (int, float)) and not isinstance(entry.get("memMiB"), bool):
            mem_mib = int(entry["memMiB"])
    d["vm_vcpu"] = vcpu
    d["vm_mem_mib"] = mem_mib
    mem_oc = cfg.get("memOvercommit", d["mem_overcommit"])
    if isinstance(mem_oc, bool) or not isinstance(mem_oc, (int, float)) or mem_oc <= 0:
        mem_oc = d["mem_overcommit"]
    cpu_oc = cfg.get("cpuOvercommit", d["cpu_overcommit"])
    if isinstance(cpu_oc, bool) or not isinstance(cpu_oc, (int, float)) or cpu_oc <= 0:
        cpu_oc = d["cpu_overcommit"]
    admitted = min(
        admission.vms_by_ram(mt, mem_oc, d),
        admission.vms_by_cpu(threads, cpu_oc, d),
    )
    return max(0, int(admitted))


def host_metadata(desc=None, options=None, config_path=None):
    """Host metadata dict for results JSON, recording warm_pool (int) when boatd.json is readable."""
    host = {"desc": desc if desc is not None else os.environ.get("BOATBENCH_HOST_DESC", "-")}
    cfg = read_boatd_config(config_path)
    if cfg is not None:
        merged_cfg = dict(cfg)
        try:
            opt = json.loads(options or "{}")
            if isinstance(opt, dict):
                merged_cfg.update(opt)
        except ValueError:
            pass
        wp = merged_cfg.get("warmPool", 0)
        if isinstance(wp, bool):
            host["warm_pool"] = 1 if wp else 0
        elif isinstance(wp, (int, float)):
            host["warm_pool"] = max(0, int(wp))
        else:
            host["warm_pool"] = 0
    return host


def fc_cache_type(options=None, boatd_dir=os.path.join(HERE, "..", "..", "components", "boat", "boatd"),
                  config_path=None):
    """Firecracker cache_type of the root drive: boatd.json's diskCache/unsafeCache, else boatd's
    source default, else deployed /etc/boat/boatd.json (or BOATD_CONFIG), else Firecracker's default."""
    try:
        opt = json.loads(options or "{}")
    except ValueError:
        opt = {}
    if isinstance(opt, dict):
        if opt.get("diskCache"):
            dc = str(opt["diskCache"]).strip()
            norm = "Writeback" if dc.lower() == "writeback" else ("Unsafe" if dc.lower() == "unsafe" else dc)
            return f"{norm} (boatd.json diskCache)"
        if isinstance(opt.get("unsafeCache"), bool):
            return "Unsafe" if opt["unsafeCache"] else "Writeback"
    explicit_cfg = config_path if config_path is not None else os.environ.get("BOATD_CONFIG")
    if explicit_cfg:
        cfg = read_boatd_config(explicit_cfg)
        if cfg is not None:
            return _cache_from_boatd_json(cfg)
    try:
        with open(os.path.join(boatd_dir, "fc.go")) as f:
            fc = f.read()
        with open(os.path.join(boatd_dir, "config.go")) as f:
            cfg = f.read()
    except OSError:
        deployed = read_boatd_config(config_path)
        if deployed is not None:
            return _cache_from_boatd_json(deployed)
        return "unknown (boatd source not found)"
    if "cache_type" not in fc:
        return "Unsafe (Firecracker's default: boatd sets no cache_type)"
    m = re.search(r'^\s*DiskCache:\s*"(\w+)"', cfg, re.M)
    return f"{m.group(1)} (boatd default diskCache)" if m else "unknown"


def source_commit():
    """The boat / PandaOS source the measured stack was built from: BOATBENCH_SOURCE_COMMIT, CI's GITHUB_SHA,
    `nixos-version --configuration-revision` on a PandaOS host, else "unknown". boat's API has no version endpoint."""
    for v in (os.environ.get("BOATBENCH_SOURCE_COMMIT"), os.environ.get("GITHUB_SHA")):
        if v:
            return v
    try:
        r = subprocess.run(["nixos-version", "--configuration-revision"], capture_output=True, text=True, timeout=10)
        if r.returncode == 0 and r.stdout.strip():
            return r.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return "unknown"


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


def _build_driver(a):
    want_compat = (
        a.driver == "pandaos-compat"
        or bool(getattr(a, "compat", False))
        or getattr(a, "api_mode", None) == "compat"
    )
    with_env = bool(getattr(a, "with_env", False))
    no_env_flag = bool(getattr(a, "no_env", False))
    env_name = getattr(a, "environment", None) or os.environ.get("BOATBENCH_ENVIRONMENT") or None
    setup_script = getattr(a, "setup_script", None) or os.environ.get("BOATBENCH_SETUP_SCRIPT") or None
    if want_compat:
        cls = drivers.DRIVERS["pandaos-compat"]
        no_env = False if with_env else True
        return cls(no_env=no_env, environment=env_name, setup_script=setup_script), "compat"
    if a.driver == "pandaos" and (with_env or env_name or setup_script):
        cls = drivers.DRIVERS["pandaos"]
        return cls(no_env=False if with_env else True, environment=env_name, setup_script=setup_script), "native"
    return drivers.DRIVERS[a.driver](), "native"


def cmd_run(a):
    d, api_mode = _build_driver(a)
    try:
        run_id = os.environ.get("BOATBENCH_RUN_ID") or secrets.token_hex(3)  # names the VMs bb-<run_id>-*
        parts = set(a.only.replace(",", ""))
        opts_env = os.environ.get("BOATBENCH_BOATD_OPTIONS")
        is_pandaos = a.driver in ("pandaos", "pandaos-compat")
        admitted = host_admitted_vms(options=opts_env, size=d.size) if is_pandaos else None
        host_info = host_metadata(options=opts_env) if is_pandaos else {"desc": os.environ.get("BOATBENCH_HOST_DESC", "-")}
        if admitted is not None:
            host_info["admitted_vms"] = admitted
        res = {"driver": a.driver, "run_id": run_id, "size": d.size,
               "started": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
               "host": host_info,
               "boatd_options": opts_env or "-",
               "preset": os.environ.get("BOATBENCH_PRESET") or "-",
               "fc_cache_type": fc_cache_type(opts_env) if is_pandaos else "-", "run_url": os.environ.get("BOATBENCH_RUN_URL"),
               "source_commit": source_commit() if a.driver != "boatdev" else "n/a (boat.dev)",
               "client": client_info(), "parts": "".join(sorted(parts)), "repeats": []}
        if api_mode == "compat":
            res["api_mode"] = "compat"
        os.makedirs(a.out, exist_ok=True)
        path = os.path.join(a.out, f"results-{a.driver}-{res['started'].replace(':', '')}.json")
        for i in range(a.repeats):
            rec = Repeat(d, run_id, i, parts, max_vms=admitted).run()
            if "c" in parts:
                env_n = os.environ.get("BOATBENCH_CONCURRENT_N")
                if env_n:
                    n = int(env_n)
                elif admitted is not None:
                    if admitted < 2:
                        reason = f"skipped: host admits {admitted} VMs"
                        rec.setdefault("skipped", {})["concurrent"] = reason
                        log(f"r{i} concurrent {reason}")
                        n = None
                    else:
                        n = min(CONCURRENT_N, admitted)
                else:
                    n = CONCURRENT_N
                if n is not None:
                    log(f"r{i} concurrent (n={n})")
                    rec["concurrent_n"] = n
                    cleanup_out = {}
                    try:
                        calls_out = {}
                        rec["metrics"][f"concurrent{n}_wall_s"], rec["concurrent_each_s"] = concurrent_create(
                            d, run_id, i, n, api_calls_out=calls_out, cleanup_out=cleanup_out)
                        if calls_out:
                            rec.setdefault("api_calls", {}).update(calls_out)
                    except Exception as e:
                        rec["errors"]["concurrent"] = f"{type(e).__name__}: {e}"
                    rec["concurrent_cleanup"] = cleanup_out
            if "g" in parts:
                dw = getattr(a, "density_workload", None) or density.workload_from_env()
                dmem = getattr(a, "density_mem_mib", None)
                if dmem is None:
                    dmem = density.mem_mib_from_env()
                log(f"r{i} density {density.levels_from_env()} workload={dw}" + (f" mem_mib={dmem}" if dw == "memory" else ""))
                trial = density.Density(d, f"bb-{run_id}-{i}", density.levels_from_env(), log, gexec, ready,
                                        workload=dw, mem_mib=dmem)
                try:
                    rec["density"], dm = trial.run()
                    rec["metrics"].update(dm)
                except Exception as e:
                    rec["errors"]["density"] = f"{type(e).__name__}: {e}"
                    traceback.print_exc(file=sys.stderr)
                finally:
                    rec["density_cleanup"] = {"pending_vms": trial.cleanup_pending_vms, "errors": trial.cleanup_errors}
            rec["metrics"] = add_net(rec["metrics"], getattr(d, "ROUND_TRIPS", {}), rec.get("api_calls"))
            res["repeats"].append(rec)
            with open(path, "w") as f:  # rewrite after every repeat: a crash keeps what was measured
                json.dump(res, f, indent=1)
            log(f"r{i} done: {json.dumps(rec['metrics'])}")
        print(path)
        failed = all(r["errors"].get("create") for r in res["repeats"])
        unresolved_cleanup = any(
            any(str(key).startswith(("cleanup_vm:", "cleanup_snapshot:")) for key in r["errors"])
            or any((r.get(kind) or {}).get("pending_vms") for kind in ("concurrent_cleanup", "density_cleanup"))
            for r in res["repeats"])
        return 1 if failed or unresolved_cleanup else 0
    finally:
        if hasattr(d, "close"):
            d.close()


def cmd_report(a):
    results = []
    for p in a.results:
        with open(p) as f:
            results.append(json.load(f))
    md = report.render(results, report.load_prices(a.prices, a.server), report.load_published())
    if a.out:
        with open(a.out, "w") as f:
            f.write(md)
    print(md)
    return 0


def cmd_presets(a):
    results = []
    for p in a.results:
        with open(p) as f:
            results.append(json.load(f))
    md = report.render_presets(results)
    if a.out:
        with open(a.out, "w") as f:
            f.write(md)
    print(md)
    return 0


def cmd_costs(a):
    md = cost_tables.render(cost_tables.load(a.prices))
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
    r.add_argument("--compat", action="store_true", help="use PandaOS /api/v1 compat mode instead of native /v1/vms")
    r.add_argument("--api-mode", choices=("native", "compat"), default=None,
                   help="PandaOS API mode: native (/v1/vms) or compat (/api/v1/sandboxes)")
    r.add_argument("--with-env", action="store_true", help="run normal environment/setup path instead of no_env=true")
    r.add_argument("--no-env", action="store_true", help="send no_env/noEnv=true (default for native pandaos and compat)")
    r.add_argument("--environment", default=None, help="environment name or ID for create/fork/resume")
    r.add_argument("--setup-script", default=None, help="setupScript for /api/v1 compat sandbox creation")
    r.add_argument("--density-workload", choices=density.WORKLOADS, default=None,
                   help="workload (g) mode: cpu (default Node loop) or memory (RAM-pressure working set)")
    r.add_argument("--density-mem-mib", type=int, default=None,
                   help="per-VM working set in MiB for --density-workload memory (default 1024)")
    r.add_argument("--repeats", type=int, default=5)
    r.add_argument("--only", default=ALL_PARTS, help="workload letters, e.g. 'a,b' (default all: abcdefg)")
    r.add_argument("--out", default="boatbench-results")
    g = sub.add_parser("report")
    g.add_argument("results", nargs="+")
    g.add_argument("--prices", default=os.path.join(HERE, "prices.json"))
    g.add_argument("--server", help="PandaOS price: a prices.json servers[].key (ax42, ax102, ...); "
                   "default pandaos.default_server")
    g.add_argument("--out")
    q = sub.add_parser("presets", help="PandaOS medians side by side per boatd preset")
    q.add_argument("results", nargs="+")
    q.add_argument("--out")
    c = sub.add_parser("costs", help="price-only tables: per VM-hour, break-even, retention (nominal vs thin model)")
    c.add_argument("--prices", default=os.path.join(HERE, "prices.json"))
    c.add_argument("--out")
    a = p.parse_args(argv)
    if a.cmd == "run" and (a.repeats < 1 or not set(a.only.replace(",", "")) <= set(ALL_PARTS)):
        p.error("--repeats must be >= 1 and --only a subset of abcdefg")
    if a.cmd == "run" and a.driver == "host" and not set(a.only.replace(",", "")) <= drivers.Host.PARTS:
        p.error("--driver host only runs workloads a, d and g")
    if a.cmd == "run" and a.with_env and a.no_env:
        p.error("--with-env and --no-env cannot be combined")
    if a.cmd == "run" and a.compat and a.api_mode == "native":
        p.error("--compat and --api-mode native cannot be combined")
    if a.cmd == "run" and (a.compat or a.api_mode == "compat") and a.driver in ("boatdev", "host"):
        p.error("--compat / --api-mode compat only applies to --driver pandaos or pandaos-compat")
    if a.cmd == "run" and a.density_mem_mib is not None and a.density_mem_mib < 1:
        p.error("--density-mem-mib must be >= 1")
    return {"run": cmd_run, "report": cmd_report, "presets": cmd_presets, "costs": cmd_costs}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
