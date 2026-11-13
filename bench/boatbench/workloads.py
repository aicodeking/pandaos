"""Guest-side workloads, identical bytes on both providers.

Every script runs through the driver's exec (bash -lc) as the provider's
default user and ends with one `BB_JSON {...}` line. Pins:

  (a) PTS pts/node-web-tooling-1.0.1 = V8 Web Tooling Benchmark v0.5.3, built
      by the PTS profile's own install.sh (test-profiles@1b05a0e, sha256
      checked), run with a pinned Node.js so both sides run the same JIT.
  (d) fio (installed with apt if absent) or dd fallback, on $HOME (disk, not tmpfs).
      Two parameter sets: ours (iodepth 32, 15 s), and the job file of pts/fio-2.1.0,
      which boat.dev's published numbers used (pts_* metrics). PTS reports MiB/s as "MB/s".
      Two flushing jobs (fsync after every 4k write; 1M writes + fsync at the end), the
      only ones where a durable cache (Firecracker Writeback) differs from Unsafe.
  (e) fixed downloads from two CDNs + TCP connect latency.
  (f) npm ci of npm-project/ (lockfile committed), cold cache, no scripts.
"""
import json
import re

NODE_VERSION = "22.22.3"
NODE_SHA256 = "2e5d13569282d016861fae7c8f935e741693c269101a5bebcf761a5376d1f99f"
WTB_SHA256 = "5d09d6f3eb19c02d9e858079efb0bd1b7303a5878f83462e6ddb93f21a1635c0"
PTS_COMMIT = "1b05a0e47b9aec76cd302f972ed209454431e855"
PTS_INSTALL_SHA256 = "ff36cde192b2dadc111dc53ac89073ba54bde935e7599feba473546f710f87f1"
PTS_PASSES = 3  # pts/node-web-tooling TimesToRun
NPM_SRC = "/tmp/bb-npm-src"  # writable on both (boat.dev files API: /home/user or /tmp)
DOWNLOADS = {
    "go_tarball_googlecdn": "https://dl.google.com/go/go1.25.0.linux-amd64.tar.gz",  # 59.7 MB
    "nodejs_tarball": f"https://nodejs.org/dist/v{NODE_VERSION}/node-v{NODE_VERSION}-linux-x64.tar.xz",
}
LATENCY_TARGETS = {"cloudflare_1.1.1.1": "https://1.1.1.1/", "github.com": "https://github.com/"}

PROBE = r"""
python3 - <<'PY'
import json, os, shutil, subprocess
def sh(c):
    try:
        return subprocess.run(c, shell=True, capture_output=True, text=True, timeout=30).stdout.strip()
    except Exception:
        return ""
mem = next((int(l.split()[1]) // 1024 for l in open("/proc/meminfo") if l.startswith("MemTotal:")), None)
model = next((l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")), "")
osr = dict(l.rstrip().split("=", 1) for l in open("/etc/os-release") if "=" in l)
trace = dict(l.split("=", 1) for l in sh("curl -fsS --max-time 10 https://www.cloudflare.com/cdn-cgi/trace").splitlines() if "=" in l)
print("BB_JSON " + json.dumps({
    "nproc": int(sh("nproc") or 0), "cpu_count": os.cpu_count(), "mem_mib": mem,
    "disk_gib": round(shutil.disk_usage(os.path.expanduser("~")).total / 2**30, 1),
    "cpu_model": model, "kernel": os.uname().release, "os": osr.get("PRETTY_NAME", "").strip('"'),
    "virt": sh("systemd-detect-virt"), "system_node": sh("node --version"), "user": sh("id -un"),
    "home_fs": sh("stat -f -c %T ~"), "egress_colo": trace.get("colo"), "egress_country": trace.get("loc"),
}))
PY
"""

NODE_SETUP = f"""
set -euo pipefail
B=/tmp/bb; rm -rf $B; mkdir -p $B/node $B/wtb; cd $B
curl -fsSLo node.tar.xz https://nodejs.org/dist/v{NODE_VERSION}/node-v{NODE_VERSION}-linux-x64.tar.xz
echo "{NODE_SHA256}  node.tar.xz" | sha256sum -c - >/dev/null
tar -xJf node.tar.xz -C node --strip-components=1
# Hosts without an FHS loader (NixOS): BB_NODE_LOADER="ld.so --library-path ..." runs the same binary. VMs never set it.
if [ -n "${{BB_NODE_LOADER:-}}" ]; then mv node/bin/node node/bin/node.real
  printf '#!/bin/sh\nexec %s %s/node/bin/node.real "$@"\n' "$BB_NODE_LOADER" "$B" >node/bin/node; chmod +x node/bin/node; fi
cd wtb
curl -fsSLo web-tooling-benchmark-0.5.3.tar.gz https://github.com/v8/web-tooling-benchmark/archive/v0.5.3.tar.gz
echo "{WTB_SHA256}  web-tooling-benchmark-0.5.3.tar.gz" | sha256sum -c - >/dev/null
curl -fsSLo install.sh https://raw.githubusercontent.com/phoronix-test-suite/test-profiles/{PTS_COMMIT}/pts/node-web-tooling-1.0.1/install.sh
echo "{PTS_INSTALL_SHA256}  install.sh" | sha256sum -c - >/dev/null
PATH=$B/node/bin:$PATH HOME=$B/wtb sh install.sh >install.log 2>&1 || true
[ "$(cat install-exit-status 2>/dev/null)" = 0 ] && [ -f web-tooling-benchmark-0.5.3/dist/cli.js ] || {{ tail -40 install.log; exit 1; }}
echo "BB_JSON {{\\"node\\": \\"$($B/node/bin/node --version)\\"}}"
"""

NODE_RUN = "cd /tmp/bb/wtb/web-tooling-benchmark-0.5.3 && /tmp/bb/node/bin/node dist/cli.js"

DISK = r"""
D=$HOME/bbdisk; rm -rf "$D"; mkdir -p "$D"
command -v fio >/dev/null || timeout 300 sh -c 'sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 ||
  { sudo -n apt-get update -qq >/dev/null 2>&1 && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1; }' || true
BBD=$D python3 - <<'PY'
import json, os, re, shutil, subprocess
d = os.environ["BBD"]; f = os.path.join(d, "f")
out = {"tool": "fio" if shutil.which("fio") else "dd"}
def run(key, cmd, field, unit):
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=240)
    try:
        job = json.loads(r.stdout[r.stdout.index("{"):])["jobs"][0][field]
        out[key] = {"bw": round(job["bw_bytes"] / 1e6, 1), "mib": round(job["bw_bytes"] / 2**20, 1),
                    "iops": round(job["iops"], 0)}[unit]
    except Exception:
        out[key] = None
        out.setdefault("errors", []).append(f"{key}: {r.stderr.strip()[-200:]}")
def blk_irq_cpus(path, sys="/sys", interrupts="/proc/interrupts"):
    # (CPU taking most of the virtio-blk interrupts of path's disk, CPU taking fewest), or (None, None)
    # off virtio, on one CPU, or before any interrupt.
    try:
        st = os.stat(path)
        sysd = os.path.realpath(f"{sys}/dev/block/{os.major(st.st_dev)}:{os.minor(st.st_dev)}")
        if os.path.exists(os.path.join(sysd, "partition")):
            sysd = os.path.dirname(sysd)
        dev = os.path.basename(os.path.realpath(os.path.join(sysd, "device")))
        with open(interrupts) as fh:
            rows = [l.split() for l in fh]
        n = len(rows[0])
        counts = [0] * n
        for r in rows[1:]:
            if dev.startswith("virtio") and r and (r[-1] == dev or r[-1].startswith(dev + "-")):
                counts = [c + int(x) for c, x in zip(counts, r[1:1 + n])]
    except (OSError, ValueError, IndexError):
        return None, None
    if n < 2 or not any(counts):
        return None, None
    irq = counts.index(max(counts))
    return irq, min((c, i) for i, c in enumerate(counts) if i != irq)[1]
if out["tool"] == "fio":
    common = ["fio", "--direct=1", "--ioengine=libaio", "--numjobs=1", "--time_based", "--group_reporting",
              "--output-format=json"]
    ours = common + ["--name=bb", f"--filename={f}", "--size=2G", "--iodepth=32", "--runtime=15"]
    pts = common + ["--name=test", f"--filename={f}", "--size=1g", "--iodepth=64", "--startdelay=20",
                    "--ramp_time=5", "--runtime=60"]
    # Flushing jobs: under Firecracker's Unsafe cache the guest's fsync stops at the host
    # page cache, under Writeback it reaches the disk, so only these differ between the two.
    fsync4k = common + ["--name=bbfs", f"--filename={f}", "--size=1G", "--iodepth=1", "--runtime=15", "--fsync=1"]
    endfsync = common + ["--name=bbef", f"--filename={f}", "--size=2G", "--iodepth=32", "--runtime=15",
                         "--end_fsync=1"]
    for key, rw, bs, field, unit, base in [("seq_write_mbps", "write", "1M", "write", "bw", ours),
                                           ("seq_read_mbps", "read", "1M", "read", "bw", ours),
                                           ("rand_write_iops", "randwrite", "4k", "write", "iops", ours),
                                           ("rand_read_iops", "randread", "4k", "read", "iops", ours),
                                           ("pts_rand_read_iops", "randread", "4k", "read", "iops", pts),
                                           ("pts_rand_write_iops", "randwrite", "4k", "write", "iops", pts),
                                           ("pts_seq_write_mibps", "write", "1M", "write", "mib", pts),
                                           ("fsync_rand_write_iops", "randwrite", "4k", "write", "iops", fsync4k),
                                           ("seq_write_endfsync_mbps", "write", "1M", "write", "bw", endfsync)]:
        run(key, base + [f"--rw={rw}", f"--bs={bs}"], field, unit)
    # Rand read moves 4x with the vCPU fio lands on (AX42: 59k on the vCPU taking the virtio-blk
    # interrupt, 219k off it), so the unpinned number above is decided by chance: run it pinned too.
    irq_cpu, other_cpu = blk_irq_cpus(f)
    out["blk_irq_cpu"] = irq_cpu
    for key, cpu in (("rand_read_irq_cpu_iops", irq_cpu), ("rand_read_other_cpu_iops", other_cpu)):
        if cpu is not None:
            run(key, ours + ["--rw=randread", "--bs=4k", f"--cpus_allowed={cpu}"], "read", "iops")
else:
    def dd(args):
        r = subprocess.run(["dd"] + args, capture_output=True, text=True, timeout=300)
        m = re.search(r"^(\d+) bytes.*copied, ([\d.]+) s", r.stderr, re.M)
        return round(int(m.group(1)) / float(m.group(2)) / 1e6, 1) if m else None
    out["seq_write_mbps"] = dd(["if=/dev/zero", f"of={f}", "bs=1M", "count=2048", "oflag=direct", "conv=fsync"])
    out["seq_read_mbps"] = dd([f"if={f}", "of=/dev/null", "bs=1M", "iflag=direct"])
    out["rand_write_iops"] = out["rand_read_iops"] = None  # dd cannot do random I/O
os.remove(f) if os.path.exists(f) else None
print("BB_JSON " + json.dumps(out))
PY
"""

NET = r"""
BBDL='%s' BBLAT='%s' python3 - <<'PY'
import json, os, statistics, subprocess
out = {}
for name, url in json.loads(os.environ["BBDL"]).items():
    r = subprocess.run(["curl", "-fsSL", "--max-time", "120", "-o", "/dev/null", "-w", "%%{size_download} %%{time_total}", url],
                       capture_output=True, text=True)
    try:
        size, t = r.stdout.split()
        if r.returncode != 0 or int(size) < 10**6:  # an error page is not a throughput sample
            raise ValueError(f"curl exit {r.returncode}, {size} bytes")
        out[f"dl_{name}_mbit"] = round(int(size) * 8 / float(t) / 1e6, 1)
    except ValueError as e:
        out[f"dl_{name}_mbit"] = None
        out.setdefault("errors", []).append(f"{name}: {e} {r.stderr.strip()[-200:]}")
for name, url in json.loads(os.environ["BBLAT"]).items():
    v = []
    for _ in range(10):
        r = subprocess.run(["curl", "-sS", "--max-time", "10", "-o", "/dev/null", "-w", "%%{time_namelookup} %%{time_connect}", url],
                           capture_output=True, text=True)
        try:
            dns, conn = map(float, r.stdout.split())
            v.append((conn - dns) * 1000)
        except ValueError:
            pass
    out[f"tcp_connect_{name}_ms"] = round(statistics.median(v), 2) if v else None
print("BB_JSON " + json.dumps(out))
PY
""" % (json.dumps(DOWNLOADS), json.dumps(LATENCY_TARGETS))

NPM_COLD = f"""
set -euo pipefail
D=$HOME/bbnpm; rm -rf "$D" "$HOME/.bbnpm-cache"; mkdir -p "$D"; cp {NPM_SRC}/package.json {NPM_SRC}/package-lock.json "$D"; cd "$D"
s=$(date +%s.%N)
PATH=/tmp/bb/node/bin:$PATH npm ci --ignore-scripts --no-audit --no-fund --cache "$HOME/.bbnpm-cache" --loglevel=error >/dev/null
e=$(date +%s.%N)
n=$(python3 -c 'import json, os; print(sum(os.path.isdir(k) for k in json.load(open("package-lock.json"))["packages"] if k))')
rm -rf "$D" "$HOME/.bbnpm-cache"
echo "BB_JSON {{\\"npm_ci_s\\": $(awk -v a=$s -v b=$e 'BEGIN{{printf "%.3f", b-a}}'), \\"packages\\": $n}}"
"""

_GEO = re.compile(r"Geometric mean:\s+([\d.]+)\s+runs/s")


def parse_bb_json(stdout):
    for line in reversed(stdout.splitlines()):
        if line.startswith("BB_JSON "):
            return json.loads(line[len("BB_JSON "):])
    raise ValueError("no BB_JSON line in output: " + stdout[-300:])


def parse_geomean(stdout):
    m = _GEO.search(stdout)
    if not m:
        raise ValueError("no 'Geometric mean: N runs/s' in output: " + stdout[-300:])
    return float(m.group(1))
