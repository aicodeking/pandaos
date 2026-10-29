#!/usr/bin/env python3
"""Boot each OS image under QEMU+KVM with identical resources and measure it.

usage: run.py --images images.json --key KEY --out DIR [--cache DIR]
              [--pandaos IMAGE.qcow2] [--ovmf-code F --ovmf-vars F]

Every VM: 2 vCPU, 2048 MB RAM, a 20 GB virtio disk (qcow2 overlay on the
pristine image), user-mode net with ssh forwarded to 127.0.0.1, UEFI (OVMF),
and the same cloud-init NoCloud seed (only sets the ssh key). Writes
results.json and results.md to --out. Needs qemu-system-x86_64, qemu-img,
cloud-localds and ssh on the host.
"""
import argparse
import hashlib
import json
import os
import shutil
import statistics
import subprocess
import sys
import time
import urllib.request

CPUS, MEM_MB, DISK = 2, 2048, "20G"
BOOT_TIMEOUT, SETUP_TIMEOUT = 600, 600
SETTLE, SAMPLES, GAP = 60, 3, 10

# POSIX sh, run the same way on every guest (busybox ash on Alpine).
PROBE = r"""
. /etc/os-release; echo "os=$PRETTY_NAME"
echo "kernel=$(uname -r)"
n=0; u=0
for p in /proc/[0-9]*; do
  [ -d "$p" ] || continue
  n=$((n+1))
  [ -n "$(tr -d '\000' < "$p/cmdline" 2>/dev/null | head -c1)" ] && u=$((u+1))
done
echo "procs=$n"; echo "userProcs=$u"
echo "rootUsedKB=$(df -Pk / | awk 'NR==2{print $3}')"
echo "rootFs=$(awk '$2=="/"{t=$3} END{print t}' /proc/mounts)"
if ls /lib/ld-musl* >/dev/null 2>&1; then echo libc=musl; else echo libc=glibc; fi
if command -v systemctl >/dev/null; then
  echo init=systemd
  echo "services=$(systemctl list-units --type=service --state=running --no-legend --plain | wc -l)"
  echo "failedUnits=$(systemctl list-units --state=failed --no-legend --plain | wc -l)"
  echo "failedNames=$(systemctl list-units --state=failed --no-legend --plain | awk '{print $1}' | tr '\n' ' ')"
else
  echo init=openrc
  echo "services=$(rc-status -a 2>/dev/null | grep -c started)"
fi
"""


def log(*a):
    print("[compare]", *a, file=sys.stderr, flush=True)


def meminfo_mb(text):
    """(free used+shmem, MemTotal-MemAvailable) in MB from /proc/meminfo."""
    mi = {}
    for line in text.splitlines():
        k, _, v = line.partition(":")
        if v.strip():
            mi[k] = int(v.split()[0])
    used = (mi["MemTotal"] - mi["MemFree"] - mi["Buffers"] - mi["Cached"]
            - mi["SReclaimable"] + mi["Shmem"])
    return used // 1024, (mi["MemTotal"] - mi["MemAvailable"]) // 1024


def parse_kv(text):
    out = {}
    for line in text.splitlines():
        k, sep, v = line.partition("=")
        if sep:
            out[k.strip()] = v.strip()
    return out


def fetch(img, cache):
    os.makedirs(cache, exist_ok=True)
    path = os.path.join(cache, os.path.basename(img["url"]))
    algo = "sha512" if "sha512" in img else "sha256"
    if not os.path.exists(path):
        log("downloading", img["url"])
        tmp = path + ".part"
        with urllib.request.urlopen(img["url"], timeout=60) as r, open(tmp, "wb") as f:
            shutil.copyfileobj(r, f, 1 << 20)
        os.replace(tmp, path)
    h = hashlib.new(algo)
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    if h.hexdigest() != img[algo]:
        os.remove(path)
        raise RuntimeError(f"{algo} mismatch for {path}")
    return path


class VM:
    def __init__(self, base, work, port, key, user, ovmf_code, ovmf_vars, seed):
        self.port, self.key, self.user, self.work = port, key, user, work
        disk = os.path.join(work, "disk.qcow2")
        vars_copy = os.path.join(work, "vars.fd")
        shutil.copy(ovmf_vars, vars_copy)
        subprocess.run(["qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2",
                        "-b", os.path.abspath(base), disk, DISK], check=True)
        self.cmd = [
            "qemu-system-x86_64", "-enable-kvm", "-machine", "q35", "-cpu", "host",
            "-smp", str(CPUS), "-m", str(MEM_MB),
            "-drive", f"if=pflash,format=raw,readonly=on,file={ovmf_code}",
            "-drive", f"if=pflash,format=raw,file={vars_copy}",
            "-drive", f"file={disk},if=virtio,format=qcow2",
            "-drive", f"file={seed},if=virtio,format=raw,readonly=on",
            "-netdev", f"user,id=n0,hostfwd=tcp:127.0.0.1:{port}-:22",
            "-device", "virtio-net-pci,netdev=n0",
            "-display", "none", "-serial", f"file:{os.path.join(work, 'console.log')}",
        ]

    def ssh(self, command, timeout=60, stdin=None):
        return subprocess.run(
            ["ssh", "-i", self.key, "-p", str(self.port), "-o", "BatchMode=yes",
             "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
             "-o", "ConnectTimeout=3", "-o", "LogLevel=ERROR",
             f"{self.user}@127.0.0.1", command],
            input=stdin, capture_output=True, text=True, timeout=timeout)

    def boot(self):
        self.t0 = time.monotonic()
        self.proc = subprocess.Popen(self.cmd)
        while time.monotonic() - self.t0 < BOOT_TIMEOUT:
            if self.proc.poll() is not None:
                raise RuntimeError(f"qemu exited with {self.proc.returncode}")
            try:
                if self.ssh("true", timeout=10).returncode == 0:
                    return time.monotonic() - self.t0
            except subprocess.TimeoutExpired:
                pass
            time.sleep(1)
        raise RuntimeError(f"no ssh within {BOOT_TIMEOUT} s")

    def stop(self):
        if getattr(self, "proc", None) and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(30)
            except subprocess.TimeoutExpired:
                self.proc.kill()

    def console_tail(self, n=40):
        try:
            with open(os.path.join(self.work, "console.log"), errors="replace") as f:
                return "".join(f.readlines()[-n:])
        except OSError:
            return ""


def measure(vm):
    r = {"bootToSshS": round(vm.boot(), 1)}
    # First-boot setup: cloud-init on the cloud images (absent on PandaOS),
    # then systemd's startup job queue. Exit codes are ignored (degraded is fine).
    vm.ssh("if command -v cloud-init >/dev/null; then cloud-init status --wait >/dev/null 2>&1; fi; "
           "command -v systemctl >/dev/null && systemctl is-system-running --wait >/dev/null 2>&1; true",
           timeout=SETUP_TIMEOUT)
    r["setupDoneS"] = round(time.monotonic() - vm.t0, 1)
    time.sleep(SETTLE)
    used, avail = [], []
    for i in range(SAMPLES):
        if i:
            time.sleep(GAP)
        u, a = meminfo_mb(vm.ssh("cat /proc/meminfo").stdout)
        used.append(u)
        avail.append(a)
    r["ramUsedShmemMB"] = int(statistics.median(used))
    r["ramTotalMinusAvailMB"] = int(statistics.median(avail))
    r["ramSamples"] = {"usedShmem": used, "totalMinusAvail": avail}
    probe = vm.ssh("sh -s", stdin=PROBE)
    kv = parse_kv(probe.stdout)
    for k in ("procs", "userProcs", "rootUsedKB", "services", "failedUnits"):
        if kv.get(k, "").isdigit():
            r[k] = int(kv[k])
    for k in ("os", "kernel", "rootFs", "libc", "init", "failedNames"):
        if k in kv:
            r[k] = kv[k]
    if "rootUsedKB" in r:
        r["rootUsedMB"] = r.pop("rootUsedKB") // 1024
    return r


def run_one(img, base, idx, args, seed):
    work = os.path.join(args.out, "vm", str(idx))
    os.makedirs(work, exist_ok=True)
    res = {"name": img["name"], "imageMB": round(os.path.getsize(base) / 2**20)}
    if "url" in img:
        res["url"] = img["url"]
    vm = VM(base, work, 2222 + idx, args.key, img["user"], args.ovmf_code, args.ovmf_vars, seed)
    try:
        res.update(measure(vm))
        res["ok"] = True
    except Exception as e:  # report it, keep going with the next OS
        res.update(ok=False, error=str(e), consoleTail=vm.console_tail())
        log(img["name"], "FAILED:", e)
    finally:
        vm.stop()
    return res


COLS = [
    ("Idle RAM, used+shmem (MB)", "ramUsedShmemMB"),
    ("Idle RAM, Total-Available (MB)", "ramTotalMinusAvailMB"),
    ("Processes (userland)", None),
    ("Running services", "services"),
    ("Disk used on / (MB)", "rootUsedMB"),
    ("Image download (MB)", "imageMB"),
    ("Boot to ssh (s)", "bootToSshS"),
    ("First-boot setup done (s)", "setupDoneS"),
]


def table(results):
    lines = ["| OS | " + " | ".join(c for c, _ in COLS) + " |",
             "|---|" + "---|" * len(COLS)]
    for r in results:
        if not r.get("ok"):
            lines.append(f"| {r['name']} | failed: {r.get('error', '?')} |" + " |" * (len(COLS) - 1))
            continue
        cells = []
        for _, k in COLS:
            if k is None:
                cells.append(f"{r.get('procs', '?')} ({r.get('userProcs', '?')})")
            else:
                cells.append(str(r.get(k, "?")))
        lines.append(f"| {r['name']} | " + " | ".join(cells) + " |")
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--images", required=True)
    ap.add_argument("--key", required=True, help="ssh private key; KEY.pub goes into the seed")
    ap.add_argument("--out", required=True)
    ap.add_argument("--cache", default="images")
    ap.add_argument("--pandaos", help="PandaOS qcow2 (bench/compare/pandaos-image.nix)")
    ap.add_argument("--ovmf-code", default="/usr/share/OVMF/OVMF_CODE_4M.fd")
    ap.add_argument("--ovmf-vars", default="/usr/share/OVMF/OVMF_VARS_4M.fd")
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)

    with open(args.key + ".pub") as f:
        pub = f.read().strip()
    seed_dir = os.path.join(args.out, "seed")
    os.makedirs(seed_dir, exist_ok=True)
    with open(os.path.join(seed_dir, "user-data"), "w") as f:
        f.write(f"#cloud-config\nssh_authorized_keys:\n  - {pub}\n")
    with open(os.path.join(seed_dir, "meta-data"), "w") as f:
        f.write("instance-id: bench-compare\nlocal-hostname: bench\n")
    seed = os.path.join(seed_dir, "seed.img")
    subprocess.run(["cloud-localds", seed, os.path.join(seed_dir, "user-data"),
                    os.path.join(seed_dir, "meta-data")], check=True)

    with open(args.images) as f:
        images = json.load(f)
    jobs = []
    if args.pandaos:
        jobs.append(({"name": "PandaOS headless", "user": "panda"}, args.pandaos))
    for img in images:
        jobs.append((img, None))

    results = []
    for idx, (img, base) in enumerate(jobs):
        log("==>", img["name"])
        try:
            base = base or fetch(img, args.cache)
        except Exception as e:
            results.append({"name": img["name"], "ok": False, "error": f"download: {e}"})
            continue
        results.append(run_one(img, base, idx, args, seed))
        log(json.dumps(results[-1]))

    meta = {"date": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "vm": {"vcpus": CPUS, "memMB": MEM_MB, "disk": DISK, "firmware": "OVMF (UEFI)",
                   "net": "qemu user-mode", "settleS": SETTLE, "samples": SAMPLES, "gapS": GAP},
            "host": os.uname().release}
    with open(os.path.join(args.out, "results.json"), "w") as f:
        json.dump({"meta": meta, "results": results}, f, indent=2)
    md = table(results)
    with open(os.path.join(args.out, "results.md"), "w") as f:
        f.write(md)
    print(md)
    return 0 if all(r.get("ok") for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
