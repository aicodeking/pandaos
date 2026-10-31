#!/usr/bin/env python3
"""cubench: the same computer-use tasks, checkers, model and tools on every OS.

  cubench.py prepare --os NAME --key KEY [--cache images] [--disks disks]
      Boot the pinned cloud image, install the desktop over ssh
      (os/NAME.prepare.sh), power off, and save disks/NAME.qcow2.
  cubench.py run --os A,B --key KEY --out DIR [--tools generic|panda]
                 [--provider anthropic|openai] [--model M] [--repeats 3]
                 [--tasks GLOB] [--seed 0] [--disk NAME=PATH] [--dry-run]
      Boot each prepared disk (UEFI, 4 vCPU, 4 GB, virtio-vga at 1280x800,
      usb-tablet, QMP socket), run every task N times, write
      DIR/results-<os>-<tools>.json and per-attempt transcripts.
  cubench.py run --smoke --os A --key KEY --out DIR [--tasks a,b]
      VM plumbing check, no API key: the scripted agent runs smoke/*.json
      (QMP clicks, wheel and keys must reach the guest 1:1) plus the --tasks
      given (their setup and checker must run; the scripted agent fails
      them). Exits non-zero unless the display is 1280x800, no setup
      crashed and every smoke task passed; --tools panda also requires
      `panda-cu call` to work in the session.
  cubench.py report FILES... --out results/DATE
      Merge result files into DATE.json and DATE.md (pass rates, 95% Wilson
      intervals, medians, fail reasons, per-task table).

Setup and verification run over ssh inside the graphical session; the agent
only gets the screen and the tools. Uses bench/compare/run.py for image
download/verification and the base QEMU VM.
"""
import argparse
import base64
import glob
import io
import json
import os
import random
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(HERE, "..", "compare"))
import agent  # noqa: E402
import qmp as qmplib  # noqa: E402
import score  # noqa: E402

CPUS, MEM_MB = 4, 4096
DISPLAY = (1280, 800)
SESSION_TIMEOUT, SETTLE_S, SETUP_SETTLE_S = 420, 20, 2
PREPARE_TIMEOUT = 3600
TASK_KEYS = {"id": str, "title": str, "instruction": str, "setup": list, "verify": str,
             "timeout_s": int, "max_steps": int}
OS_KEYS = {"name": str, "user": str, "session": str, "apps": dict, "hints": list, "reset": list}


def log(*a):
    print("[cubench]", *a, file=sys.stderr, flush=True)


# -- tasks and OS profiles ----------------------------------------------------------
def load_tasks(task_dir, pattern="*"):
    """Tasks in task_dir matching pattern (comma-separated globs over ids)."""
    tasks, seen = [], set()
    paths = sorted({p for g in pattern.split(",") if g for p in glob.glob(os.path.join(task_dir, g + ".json"))})
    for path in paths:
        with open(path) as f:
            t = json.load(f)
        for k, typ in TASK_KEYS.items():
            if not isinstance(t.get(k), typ):
                raise ValueError(f"{path}: '{k}' must be a {typ.__name__}")
        if t["id"] != os.path.basename(path)[:-5]:
            raise ValueError(f"{path}: id {t['id']!r} must match the file name")
        if t["id"] in seen:
            raise ValueError(f"{path}: duplicate id")
        if not all(isinstance(s, str) for s in t["setup"]):
            raise ValueError(f"{path}: setup must be a list of shell strings")
        if t["timeout_s"] <= 0 or t["max_steps"] <= 0:
            raise ValueError(f"{path}: timeout_s and max_steps must be positive")
        bad = set(t.get("tools_exclude", [])) - agent.MOUSE_TOOLS
        if bad:
            raise ValueError(f"{path}: only mouse tools can be excluded, not {sorted(bad)}")
        seen.add(t["id"])
        tasks.append(t)
    if not tasks:
        raise ValueError(f"no tasks match {pattern!r} in {task_dir}")
    return tasks


def load_os(name):
    path = os.path.join(HERE, "os", name + ".json")
    with open(path) as f:
        p = json.load(f)
    for k, typ in OS_KEYS.items():
        if not isinstance(p.get(k), typ):
            raise ValueError(f"{path}: '{k}' must be a {typ.__name__}")
    if not set(p["apps"]) >= {"editor", "files", "terminal", "browser"}:
        raise ValueError(f"{path}: apps needs editor, files, terminal, browser")
    if not re.fullmatch(r"(systemd|proc:[\w.+-]+)", p["session"]):
        raise ValueError(f"{path}: session must be 'systemd' or 'proc:NAME'")
    return p


def prelude(p):
    """Shell header for setup/verify: session env, $B, $A, launch, serve_form."""
    if p["session"] == "systemd":
        src = "systemctl --user show-environment 2>/dev/null"
    else:
        src = (f"p=$(pgrep -u \"$(id -u)\" -x {p['session'][5:]} | head -n1); "
               "[ -n \"$p\" ] && tr '\\0' '\\n' < /proc/$p/environ")
    apps = "".join(f"APP_{role}={json.dumps(cmd)}\n" for role, cmd in sorted(p["apps"].items()))
    return f"""set -u
export B="$HOME/bench" A="$HOME/.cubench"
while IFS= read -r kv; do export "$kv"; done < <({src} | grep -E '^(DISPLAY|WAYLAND_DISPLAY|XAUTHORITY|XDG_RUNTIME_DIR|DBUS_SESSION_BUS_ADDRESS|XDG_CURRENT_DESKTOP|XDG_SESSION_TYPE)=')
export XDG_RUNTIME_DIR="${{XDG_RUNTIME_DIR:-/run/user/$(id -u)}}"
export DBUS_SESSION_BUS_ADDRESS="${{DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}}"
if [ -z "${{WAYLAND_DISPLAY:-}}${{DISPLAY:-}}" ] && [ -S "$XDG_RUNTIME_DIR/wayland-0" ]; then export WAYLAND_DISPLAY=wayland-0; fi
{apps}launch() {{ local r=$1 c; shift; eval "c=\\$APP_$r"; setsid -f $c "$@" >/dev/null 2>&1 </dev/null; sleep "${{LAUNCH_WAIT:-5}}"; }}
serve_form() {{
  setsid -f python3 "$A/formserver.py" "$A/www" "$B/form.log" >/dev/null 2>&1 </dev/null
  for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/8000) 2>/dev/null && return 0; sleep 0.2; done; return 1
}}
"""


def assets_tarball_b64():
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        tar.add(os.path.join(HERE, "tasks", "assets"), arcname=".")
    return base64.b64encode(buf.getvalue()).decode()


# -- VMs ------------------------------------------------------------------------
def DesktopVM(base, work, port, key, user, ovmf_code, ovmf_vars, seed):
    """bench/compare's VM plus a 1280x800 virtio-vga, a usb-tablet and a QMP socket."""
    import run as compare
    vm = compare.VM(base, work, port, key, user, ovmf_code, ovmf_vars, seed)
    vm.cmd[vm.cmd.index("-m") + 1] = str(MEM_MB)
    vm.cmd[vm.cmd.index("-smp") + 1] = str(CPUS)
    vm.qmp_path = os.path.join(tempfile.mkdtemp(prefix="cuqmp"), "qmp.sock")  # sun_path is short
    vm.cmd += ["-vga", "none", "-device", f"virtio-vga,xres={DISPLAY[0]},yres={DISPLAY[1]}",
               "-device", "qemu-xhci", "-device", "usb-tablet",
               "-qmp", f"unix:{vm.qmp_path},server=on,wait=off"]
    return vm


def make_seed(key, out):
    """NoCloud seed with only the ssh key and a fresh instance-id (so a cached
    prepared disk takes this run's key)."""
    with open(key + ".pub") as f:
        pub = f.read().strip()
    d = os.path.join(out, "seed")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "user-data"), "w") as f:
        f.write(f"#cloud-config\nssh_authorized_keys:\n  - {pub}\n")
    with open(os.path.join(d, "meta-data"), "w") as f:
        f.write(f"instance-id: cubench-{int(time.time())}\nlocal-hostname: cubench\n")
    seed = os.path.join(d, "seed.img")
    subprocess.run(["cloud-localds", seed, os.path.join(d, "user-data"), os.path.join(d, "meta-data")], check=True)
    return seed


def shell_for(vm, p):
    pre = prelude(p)

    def sh(script, timeout=120):
        try:
            r = vm.ssh("bash -s", timeout=timeout, stdin=pre + script + "\n")
            return r.returncode, r.stdout, r.stderr
        except subprocess.TimeoutExpired:
            return 124, "", f"timed out after {timeout} s"
    return sh


def wait_cloud_init(vm):
    vm.ssh("if command -v cloud-init >/dev/null; then cloud-init status --wait >/dev/null 2>&1; fi; true",
           timeout=900)


def prepare(args):
    p = load_os(args.os)
    out = os.path.join(args.disks, p["name"] + ".qcow2")
    if os.path.exists(out):
        log("prepared disk exists:", out)
        return 0
    if "image" not in p:
        raise SystemExit(f"{p['name']} has no cloud image; build it (see README) and pass --disk")
    import run as compare
    base = compare.fetch(p["image"], args.cache)
    work = tempfile.mkdtemp(prefix=f"cuprep-{p['name']}-", dir=args.work)
    vm = DesktopVM(base, work, 2299, args.key, p["user"], args.ovmf_code, args.ovmf_vars,
                   make_seed(args.key, work))
    try:
        log("booting", p["name"], "for preparation")
        vm.boot()
        wait_cloud_init(vm)
        script = ""
        for part in ("common.sh", p["name"] + ".prepare.sh"):
            with open(os.path.join(HERE, "os", part)) as f:
                script += f.read() + "\n"
        t0 = time.monotonic()
        r = vm.ssh(f"sudo CUBENCH_USER={p['user']} bash -s", timeout=PREPARE_TIMEOUT, stdin=script)
        log(f"prepare script: exit {r.returncode} after {time.monotonic() - t0:.0f} s")
        if r.returncode != 0:
            print(r.stdout[-6000:], r.stderr[-6000:], sep="\n", file=sys.stderr)
            return 1
        vm.ssh("sudo systemctl poweroff", timeout=30)
        vm.proc.wait(300)
    except Exception as e:
        log("prepare FAILED:", e, "\n" + vm.console_tail())
        return 1
    finally:
        vm.stop()
    os.makedirs(args.disks, exist_ok=True)
    subprocess.run(["qemu-img", "convert", "-p", "-O", "qcow2", "-c", "-o", "compression_type=zstd",
                    os.path.join(work, "disk.qcow2"), out + ".part"], check=True)
    os.replace(out + ".part", out)
    shutil.rmtree(work, ignore_errors=True)
    log("saved", out, f"{os.path.getsize(out) >> 20} MB")
    return 0


# -- trials ------------------------------------------------------------------------
def schedule(tasks, repeats, seed):
    """(task, repeat) pairs, shuffled with the seed so order effects average out."""
    order = [(t, r) for r in range(1, repeats + 1) for t in tasks]
    random.Random(seed).shuffle(order)
    return order


def run_trial(sh, q, p, task, rep, args, tdir):
    os.makedirs(tdir, exist_ok=True)
    trial = {"os": p["name"], "tools": args.tools, "task": task["id"], "repeat": rep,
             "passed": False, "steps": 0, "seconds": 0.0}
    setup = "\n".join(p["reset"]) + "\nrm -rf \"$B\"; mkdir -p \"$B\"\nset -e\n" + "\n".join(task["setup"])
    rc, out, err = sh(setup, timeout=180)
    if rc != 0:
        trial.update(reason="crash", error=f"setup exited {rc}: {(err or out)[-1500:]}")
        return trial
    time.sleep(SETUP_SETTLE_S)
    model = agent.make_model(args.provider, args.model, agent.tool_specs(args.tools, task.get("tools_exclude", ())),
                             seed=args.seed, effort=args.effort, script=task.get("script"))
    env = agent.Env(q, sh, tdir)
    rec = agent.run_episode(model, env, task, p, args.tools, os.path.join(tdir, "transcript.jsonl"))
    rc, out, err = sh(task["verify"], timeout=60)
    passed = rc == 0 and not rec["error"]
    usage = rec.pop("usage")
    trial.update(rec, passed=passed, reason=score.fail_reason(rec, passed), usage=usage,
                 cost_usd=agent.cost_usd(getattr(model, "model", args.model), usage, args.price),
                 verifyOutput=(out + err)[-500:])
    return trial


def boot_desktop(p, base, args, idx):
    work = tempfile.mkdtemp(prefix=f"cuvm-{p['name']}-", dir=args.work)
    vm = DesktopVM(base, work, 2222 + idx, args.key, p["user"], args.ovmf_code, args.ovmf_vars,
                   make_seed(args.key, work))
    vm.boot()
    wait_cloud_init(vm)
    sh = shell_for(vm, p)
    t0 = time.monotonic()
    while time.monotonic() - t0 < SESSION_TIMEOUT:
        if sh('[ -n "${WAYLAND_DISPLAY:-}${DISPLAY:-}" ]', 30)[0] == 0:
            break
        time.sleep(5)
    else:
        raise RuntimeError(f"no graphical session within {SESSION_TIMEOUT} s")
    rc, _, err = sh(f'mkdir -p "$A" && echo {assets_tarball_b64()} | base64 -d | tar -xzf - -C "$A"')
    if rc:
        raise RuntimeError(f"asset upload failed: {err}")
    for line in p.get("session_setup", []):
        sh(line)
    time.sleep(SETTLE_S)
    # WAYLAND_DISPLAY first: greetd leaves XDG_SESSION_TYPE=tty in labwc's env.
    vm.session_type = sh('echo "${WAYLAND_DISPLAY:+wayland}"')[1].strip() or (
        "x11" if sh('[ -n "${DISPLAY:-}" ]')[0] == 0 else "unknown")
    return vm, sh


def run_os(name, args, idx, tasks):
    p = load_os(name)
    label = f"{p['name']}-{args.tools}"
    tdir = os.path.join(args.out, "transcripts", label)
    info = {"os": p["name"], "tools": args.tools}
    trials, vm, fake = [], None, None
    try:
        if args.dry_run:
            sock = os.path.join(tempfile.mkdtemp(prefix="cufake"), "qmp.sock")
            fake = qmplib.FakeQMPServer(sock)
            sh = lambda script, timeout=120: (0, "", "")  # noqa: E731
        else:
            base = args.disk.get(p["name"]) or os.path.join(args.disks, p["name"] + ".qcow2")
            if not os.path.exists(base):
                raise RuntimeError(f"no disk {base}: run `prepare` (or pass --disk {p['name']}=PATH)")
            vm, sh = boot_desktop(p, base, args, idx)
            sock = vm.qmp_path
            info["sessionType"] = vm.session_type
        q = qmplib.QMP(sock)
        q.screendump(os.path.join(args.out, f"{label}-desktop.png"))
        info["display"] = "x".join(map(str, q.size))
        if q.size != DISPLAY:
            log(f"WARNING {p['name']}: display is {info['display']}, not {DISPLAY}")
        if args.smoke and args.tools == "panda" and not args.dry_run:
            info["pandaCu"] = panda_cu_probe(sh)
        for task, rep in schedule(tasks, args.repeats, args.seed):
            if args.repeat_index and rep != args.repeat_index:
                continue
            log(f"{label}: {task['id']} #{rep}")
            if vm and vm.proc.poll() is not None:
                trials.append({"os": p["name"], "tools": args.tools, "task": task["id"], "repeat": rep,
                               "passed": False, "reason": "crash", "steps": 0, "seconds": 0.0,
                               "error": "VM is not running"})
                continue
            t = run_trial(sh, q, p, task, rep, args, os.path.join(tdir, f"{task['id']}-r{rep}"))
            log(f"  -> {'PASS' if t['passed'] else 'FAIL ' + str(t.get('reason'))} "
                f"steps={t['steps']} {t['seconds']}s")
            trials.append(t)
        q.close()
    except Exception as e:
        info["error"] = f"{type(e).__name__}: {e}"
        if vm:
            info["consoleTail"] = vm.console_tail()
        log(p["name"], "FAILED:", e)
    finally:
        if vm:
            vm.stop()
        if fake:
            fake.close()
    return info, trials


def panda_cu_probe(sh):
    """`panda-cu call` inside the session: the windows and a screenshot's size."""
    rc, out, err = sh("panda-cu call cu_windows && panda-cu call cu_screenshot | python3 -c "
                      "'import base64,json,struct,sys; d=base64.b64decode(json.load(sys.stdin)[\"png_base64\"]);"
                      " print(\"%dx%d\" % struct.unpack(\">II\", d[16:24]))'", 120)
    lines = out.strip().splitlines()
    if rc != 0 or not lines:
        return {"ok": False, "error": f"exit {rc}: {(err or out)[-800:]}"}
    return {"ok": True, "windows": lines[0][:800], "screenshot": lines[-1]}


def smoke_failures(info, trials, smoke_ids):
    """Why a --smoke run failed (empty: the VM plumbing works)."""
    bad = [f"run error: {info['error']}"] if "error" in info else []
    if info.get("display") != "x".join(map(str, DISPLAY)):
        bad.append(f"display {info.get('display')}, want {DISPLAY[0]}x{DISPLAY[1]}")
    if "pandaCu" in info and not info["pandaCu"]["ok"]:
        bad.append(f"panda-cu call failed: {info['pandaCu']['error']}")
    for t in trials:
        if t.get("reason") == "crash":
            bad.append(f"{t['task']}: crash: {t.get('error')}")
        elif t["task"] in smoke_ids and not t["passed"]:
            bad.append(f"{t['task']}: failed: {t.get('verifyOutput', '').strip()}")
    ran = {t["task"] for t in trials}
    bad += [f"{i}: did not run" for i in sorted(smoke_ids - ran)]
    return bad


def cmd_run(args):
    os.makedirs(args.out, exist_ok=True)
    args.work = args.work or args.out
    if args.dry_run:
        global SETUP_SETTLE_S
        args.provider = "scripted"
        agent.SETTLE_S = SETUP_SETTLE_S = 0
    smoke_ids = set()
    if args.smoke:
        args.provider = "scripted"
        smoke = load_tasks(os.path.join(HERE, "smoke"))
        smoke_ids = {t["id"] for t in smoke}
        extra = load_tasks(os.path.join(HERE, "tasks"), args.tasks) if args.tasks else []
        if smoke_ids & {t["id"] for t in extra}:
            raise ValueError("a smoke task id is also a benchmark task id")
        tasks = smoke + extra
    else:
        tasks = load_tasks(os.path.join(HERE, "tasks"), args.tasks or "*")
    rc = 0
    for idx, name in enumerate(n for n in args.os.split(",") if n):
        info, trials = run_os(name, args, idx, tasks)
        meta = {"date": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "provider": args.provider,
                "model": args.model or ("claude-sonnet-5-5" if args.provider == "anthropic" else
                                        os.environ.get("OPENAI_MODEL", args.provider)),
                "repeats": args.repeats, "seed": args.seed, "dryRun": args.dry_run, "smoke": args.smoke,
                "vm": {"vcpus": CPUS, "memMB": MEM_MB, "display": "x".join(map(str, DISPLAY)),
                       "firmware": "OVMF (UEFI)", "pointer": "usb-tablet", "input": "QMP"},
                "commit": os.environ.get("GITHUB_SHA"), **info}
        shard = f"-r{args.repeat_index}" if args.repeat_index else ""
        if args.smoke:
            meta["smokeFailures"] = smoke_failures(info, trials, smoke_ids)
            for msg in meta["smokeFailures"]:
                log(f"SMOKE FAIL {name}: {msg}")
            if meta["smokeFailures"]:
                rc = 1
            else:
                log(f"SMOKE OK {name}: display {info.get('display')}, {info.get('sessionType')} session")
        path = os.path.join(args.out, f"results-{name}-{args.tools}{shard}.json")
        with open(path, "w") as f:
            json.dump({"meta": meta, "trials": trials}, f, indent=2)
        log("wrote", path)
        if "error" in info:
            rc = 1
    return rc


def cmd_report(args):
    metas, trials = [], []
    for path in args.files:
        with open(path) as f:
            d = json.load(f)
        metas.append(d["meta"])
        trials += d["trials"]
    rows = score.summarize(trials)
    md = score.markdown(rows, trials)
    head = metas[0] if metas else {}
    notes = [f"- {m['os']} ({m['tools']}): run failed: {m['error']}" for m in metas if m.get("error")]
    notes += [f"- {m['os']} ({m['tools']}): smoke " + ("; ".join(m["smokeFailures"]) or
                                                       f"OK, display {m.get('display')}, {m.get('sessionType')} session")
              for m in metas if m.get("smoke")]
    title = (f"# cubench {head.get('date', '')[:10]}\n\nModel: {head.get('provider')}/{head.get('model')}, "
             f"repeats: {head.get('repeats')}, seed: {head.get('seed')}"
             f"{' (DRY RUN: scripted model, fake VM)' if head.get('dryRun') else ''}"
             f"{' (SMOKE: scripted agent, VM plumbing only; pass rates mean nothing)' if head.get('smoke') else ''}\n\n")
    md = title + md + ("\n" + "\n".join(notes) + "\n" if notes else "")
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out + ".json", "w") as f:
        json.dump({"runs": metas, "summary": rows, "trials": trials}, f, indent=2)
    with open(args.out + ".md", "w") as f:
        f.write(md)
    print(md)
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("prepare", "run"):
        s = sub.add_parser(name)
        s.add_argument("--os", required=True)
        s.add_argument("--key", help="ssh private key; KEY.pub goes into the seed")
        s.add_argument("--cache", default="images")
        s.add_argument("--disks", default="disks")
        s.add_argument("--work", help="scratch dir for VM overlays (default: --out)")
        s.add_argument("--ovmf-code", default="/usr/share/OVMF/OVMF_CODE_4M.fd")
        s.add_argument("--ovmf-vars", default="/usr/share/OVMF/OVMF_VARS_4M.fd")
    r = sub.choices["run"]
    r.add_argument("--out", required=True)
    r.add_argument("--tools", choices=("generic", "panda"), default="generic")
    r.add_argument("--provider", choices=("anthropic", "openai", "scripted"), default="anthropic")
    r.add_argument("--model")
    r.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"))
    r.add_argument("--price", type=lambda s: tuple(float(x) for x in s.split(",")),
                   help="USD per Mtok 'in,out[,cache_write,cache_read]' for models not in agent.PRICES")
    r.add_argument("--repeats", type=int, default=3)
    r.add_argument("--repeat-index", type=int, default=0,
                   help="run only this repeat (1..N), so CI can shard repeats over jobs")
    r.add_argument("--tasks", help="comma-separated globs over task ids (default: all; with --smoke: none)")
    r.add_argument("--seed", type=int, default=0)
    r.add_argument("--disk", action="append", default=[], metavar="NAME=PATH")
    r.add_argument("--dry-run", action="store_true", help="scripted model + fake QMP, no VM, no API")
    r.add_argument("--smoke", action="store_true",
                   help="VM plumbing check: scripted model, smoke/*.json plus --tasks, strict exit code")
    rep = sub.add_parser("report")
    rep.add_argument("files", nargs="+")
    rep.add_argument("--out", required=True, help="output path without extension")
    args = ap.parse_args(argv)
    if args.cmd == "report":
        return cmd_report(args)
    if not getattr(args, "dry_run", False) and not args.key:
        ap.error("--key is required")
    if args.cmd == "prepare":
        args.work = args.work or tempfile.gettempdir()
        return prepare(args)
    args.disk = dict(d.split("=", 1) for d in args.disk)
    return cmd_run(args)


if __name__ == "__main__":
    sys.exit(main())
