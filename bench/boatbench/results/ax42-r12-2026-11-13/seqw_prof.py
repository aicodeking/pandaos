"""Where a guest's 1M sequential write goes on the AX42 (r12). Runs ON the server from a copy of bench/boatbench:
  BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) BB_DIR=/tmp/bbr12 \
    nix shell nixpkgs#python3 nixpkgs#fio -c python3 seqw_prof.py OUT
One VM bb-r12-<hex>. The PTS-style seq write (1M, iodepth 64, O_DIRECT, libaio) runs 30 s in the guest under each
guest knob below while, from second 8 to 18, the host samples: the guest's /sys/block/vda/stat (requests and their
size as the guest sends them), the host's /sys/block/<nvme>/stat (requests at the host block layer, in flight),
and the CPU of each thread of the VM's firecracker (/proc/PID/task/*/stat). Then the host's own fio on a NOCOW file
in the store (the ceiling). Raw samples go to OUT/<label>.json, the sequence to OUT/log.txt."""
import json, os, secrets, subprocess, sys, time

sys.path.insert(0, os.environ.get("BB_DIR", "/tmp/bbr12"))
import boatbench  # noqa: E402
import drivers  # noqa: E402

OUT = sys.argv[1]
os.makedirs(OUT, exist_ok=True)
LOG = open(os.path.join(OUT, "log.txt"), "a")
FIO = ["fio", "--direct=1", "--ioengine=libaio", "--numjobs=1", "--time_based", "--group_reporting",
       "--output-format=json", "--name=v", "--size=1g", "--rw=write", "--bs=1M", "--iodepth=64"]
HZ = os.sysconf("SC_CLK_TCK")


def log(msg):
    line = f"{time.strftime('%H:%M:%SZ', time.gmtime())} {msg}"
    print(line, flush=True)
    LOG.write(line + "\n")
    LOG.flush()


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def blkstat(text):  # reads, read sectors, writes, write merges, write sectors, in flight, io ticks, time in queue
    f = [int(x) for x in text.split()]
    return {"r": f[0], "rsec": f[2], "w": f[4], "wmerge": f[5], "wsec": f[6], "inflight": f[8], "ticks": f[9],
            "inqueue": f[10]}


def threads(pid):
    out = {}
    for t in os.listdir(f"/proc/{pid}/task"):
        try:
            st = sh(f"sudo cat /proc/{pid}/task/{t}/stat")
            comm = st[st.index("(") + 1:st.rindex(")")]
            f = st[st.rindex(")") + 2:].split()
            out[f"{comm}/{t}"] = int(f[11]) + int(f[12])
        except (ValueError, IndexError):
            pass
    return out


def window(g, vid, nvme, pid, secs=10):
    g0, h0, t0, c0 = blkstat(g("cat /sys/block/vda/stat")[1]), blkstat(sh(f"cat /sys/block/{nvme}/stat")), threads(pid), time.time()
    inflight = []
    for _ in range(secs * 4):
        time.sleep(0.25)
        inflight.append(blkstat(sh(f"cat /sys/block/{nvme}/stat"))["inflight"])
    g1, h1, t1, dt = blkstat(g("cat /sys/block/vda/stat")[1]), blkstat(sh(f"cat /sys/block/{nvme}/stat")), threads(pid), time.time() - c0
    d = lambda a, b, k: b[k] - a[k]  # noqa: E731
    res = {"seconds": round(dt, 2)}
    for side, a, b in (("guest_vda", g0, g1), ("host_nvme", h0, h1)):
        w = d(a, b, "w")
        res[side] = {"writes_per_s": round(w / dt), "write_MBps": round(d(a, b, "wsec") * 512 / dt / 1e6, 1),
                     "avg_write_KiB": round(d(a, b, "wsec") * 512 / 1024 / w, 1) if w else None,
                     "write_merges": d(a, b, "wmerge"),
                     "avg_queue_depth": round(d(a, b, "inqueue") / 1000 / dt, 2)}
    res["host_nvme"]["inflight_samples"] = inflight
    res["firecracker_threads_cpu_pct"] = {k: round((t1[k] - t0.get(k, 0)) / HZ / dt * 100, 1) for k in t1 if t1[k] - t0.get(k, 0)}
    return res


def main():
    d = drivers.PandaOS()
    name = f"bb-r12-{secrets.token_hex(3)}"
    nvme = os.path.basename(sh("findmnt -no SOURCE /var/lib/boat").split("[")[0])
    if os.path.exists(f"/sys/class/block/{nvme}/partition"):
        nvme = os.path.basename(os.path.dirname(os.path.realpath(f"/sys/class/block/{nvme}")))
    log("host: " + sh("uname -r; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor; "
                      "sudo jq -c '{diskCache, diskEngine, untrustedTenants, warmPool}' /etc/boat/boatd.json; "
                      f"findmnt -no SOURCE,FSTYPE,OPTIONS /var/lib/boat; cat /sys/block/{nvme}/queue/scheduler "
                      f"/sys/block/{nvme}/queue/max_sectors_kb; uptime").replace("\n", " | ") + f" | disk {nvme}")
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        pid = sh(f"pgrep -f 'firecracker.*{vid}' | head -1") or sh(f"sudo cat /run/boat/{vid}.pid 2>/dev/null")
        log(f"firecracker pid {pid}: " + sh(f"sudo ls /proc/{pid}/task | while read t; do printf '%s ' $(sudo cat /proc/{pid}/task/$t/comm); done"))
        q = "/sys/block/vda/queue"
        log("guest queue: " + g(f"cd {q}; for k in scheduler nr_requests max_sectors_kb max_hw_sectors_kb max_segments "
                                "max_segment_size read_ahead_kb rq_affinity; do printf '%s=%s ' $k \"$(cat $k)\"; done; nproc")[1])
        F = "$HOME/bbdisk/f"
        g(f"mkdir -p $HOME/bbdisk; rm -f {F}; " + " ".join(FIO + [f"--filename={F}", "--runtime=10"]), 200)  # lay out
        hw = g(f"cat {q}/max_hw_sectors_kb")[1].strip()
        booted = g(f"cat {q}/scheduler {q}/max_sectors_kb {q}/nr_requests")[1].split()
        sched0 = next(s.strip("[]") for s in booted if s.startswith("["))
        reset = f"echo {sched0} > {q}/scheduler; echo {booted[-2]} > {q}/max_sectors_kb; echo {booted[-1]} > {q}/nr_requests"
        variants = {"booted": "", "max-sectors-hw": f"echo {hw} > {q}/max_sectors_kb",
                    "mq-deadline": f"echo mq-deadline > {q}/scheduler",
                    "hugepage-buffers": "HUGE"}  # fio's buffers in a huge page: physically contiguous, so one segment can be big
        for t in (1, 2):
            for label, cmd in variants.items():
                g(f"sudo -n sh -c '{reset}; echo 0 > /proc/sys/vm/nr_hugepages'")
                job = FIO + [f"--filename={F}", "--runtime=30"]
                if cmd == "HUGE":
                    g("sudo -n sh -c 'echo 64 > /proc/sys/vm/nr_hugepages'")
                    job = ["sudo", "-n"] + job + ["--iomem=mmaphuge", "--iomem_align=2M"]
                elif cmd:
                    g(f"sudo -n sh -c '{cmd}'")
                bg = subprocess.Popen(["python3", "-c", "import sys, drivers; d = drivers.PandaOS(); "
                                       "print(d.exec(sys.argv[1], sys.argv[2], 200)[1])", vid, " ".join(job)],
                                      stdout=subprocess.PIPE, text=True, env={**os.environ, "PYTHONPATH": os.environ.get("BB_DIR", "/tmp/bbr12")})
                time.sleep(8)
                win = window(g, vid, nvme, pid)
                raw = bg.communicate()[0]
                try:
                    io = json.loads(raw[raw.index("{"):])["jobs"][0]["write"]
                    win["fio_write_MiBps"] = round(io["bw_bytes"] / 2**20, 1)
                except ValueError:
                    win["fio_error"] = raw[-300:]
                json.dump(win, open(os.path.join(OUT, f"seqw-t{t}-{label}.json"), "w"), indent=1)
                top = sorted(win["firecracker_threads_cpu_pct"].items(), key=lambda kv: -kv[1])[:4]
                log(f"seqw-t{t}-{label}: fio {win.get('fio_write_MiBps')} MiB/s | guest {win['guest_vda']} | "
                    f"host {dict((k, v) for k, v in win['host_nvme'].items() if k != 'inflight_samples')} | threads {top}")
        g(f"sudo -n sh -c '{reset}; echo 0 > /proc/sys/vm/nr_hugepages'")
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")
    scratch = "/var/lib/boat/bb-r12-scratch"
    sh(f"sudo mkdir -p {scratch} && sudo chattr +C {scratch}")
    raw = sh(f"sudo fio --direct=1 --ioengine=libaio --numjobs=1 --time_based --group_reporting --output-format=json "
             f"--name=h --filename={scratch}/f --size=1g --rw=write --bs=1M --iodepth=64 --runtime=20")
    open(os.path.join(OUT, "host-nocow.json"), "w").write(raw)
    io = json.loads(raw[raw.index("{"):])["jobs"][0]["write"]
    log(f"host NOCOW fio (same job, 20 s): {io['bw_bytes'] / 2**20:.1f} MiB/s")
    sh(f"sudo rm -rf {scratch}")
    log(f"scratch removed: {sh(f'test -e {scratch} && echo no || echo yes')}")


main()
