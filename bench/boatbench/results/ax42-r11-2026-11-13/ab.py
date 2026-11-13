"""Guest sequential write vs the host on the AX42 (PandaOS install), and the knobs inside Firecracker's limits.
Runs ON the server, from a copy of bench/boatbench (BB_DIR, default /tmp/bbr11):
  BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) nix shell nixpkgs#python3 -c python3 ab.py knobs OUT
  ... ab.py seqw OUT LABEL     # 3 x the PTS-style seq write in a fresh VM under boatd's current config
  ... ab.py irq OUT            # unpinned 4k rand read, virtio-blk IRQ where the guest put it vs moved off vCPU 0
  ... ab.py fixes OUT          # the post-install fixes, after: image extents + FICLONE, a snapshot's mem extents and
                               # write time (boatd's journal), the guest's boot chain (systemd-analyze, ldconfig, quiet)
Every fio JSON goes to OUT/<label>.json; OUT/log.txt has the sequence, guest queue settings and host facts."""
import json, os, secrets, subprocess, sys, time

sys.path.insert(0, os.environ.get("BB_DIR", "/tmp/bbr11"))
COMMON = ["--direct=1", "--ioengine=libaio", "--numjobs=1", "--time_based", "--group_reporting", "--output-format=json"]
SHORT = ["--name=v", "--size=1g", "--ramp_time=5", "--runtime=20"]
SEQW = SHORT + ["--rw=write", "--bs=1M", "--iodepth=64"]  # the PTS seq write job, 20 s instead of 60
RANDR = SHORT + ["--rw=randread", "--bs=4k", "--iodepth=64"]
F = "$HOME/bbdisk/f"
OUT = sys.argv[2]
os.makedirs(OUT, exist_ok=True)
LOG = open(os.path.join(OUT, "log.txt"), "a")


def log(msg):
    line = f"{time.strftime('%H:%M:%SZ', time.gmtime())} {msg}"
    print(line, flush=True)
    LOG.write(line + "\n")
    LOG.flush()


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def summary(label, raw):
    j = json.loads(raw[raw.index("{"):])["jobs"][0]
    io = j["read"] if j["read"]["io_bytes"] else j["write"]
    log(f"{label}: {io['iops']:.0f} IOPS, {io['bw_bytes'] / 2**20:.1f} MiB/s")


def host_facts():
    log("host: " + sh("uname -r; nixos-version; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor "
                      "/sys/devices/system/cpu/cpu0/cpufreq/energy_performance_preference; "
                      "sudo jq -c '{diskCache, diskEngine, untrustedTenants}' /etc/boat/boatd.json; "
                      "systemctl show -p ExecStart --value boatd | grep -o 'config [^ ;]*' || true; "
                      "findmnt -no FSTYPE,OPTIONS /var/lib/boat; uptime").replace("\n", " | "))


def vm_session(fn, fio=True):
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        if not fio:
            return fn(g, d, vid)
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        g(f"mkdir -p $HOME/bbdisk; rm -f {F}")
        g(" ".join(["fio"] + COMMON + SEQW + [f"--filename={F}", "--runtime=10"]), 200)  # lay the file out first
        log("guest queue: " + g("cd /sys/block/vda/queue; for k in scheduler nr_requests max_sectors_kb max_hw_sectors_kb "
                                "max_segments read_ahead_kb rq_affinity nomerges; do printf '%s=%s ' $k \"$(cat $k)\"; done")[1])
        fn(g)
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def gfio(g, label, job, prefix=""):
    rc, so, se = g(" ".join([prefix + "fio"] + COMMON + job + [f"--filename={F}"]), 200)
    open(os.path.join(OUT, label + ".json"), "w").write(so)
    summary(label, so)


def knobs(g):
    q = "/sys/block/vda/queue"
    hw = g(f"cat {q}/max_hw_sectors_kb")[1].strip()
    base = g(f"cat {q}/scheduler {q}/max_sectors_kb {q}/nr_requests")[1].split()
    sched0 = next(s.strip("[]") for s in base if s.startswith("["))
    variants = {  # each: the sysfs writes, from the as-booted state
        "booted": "",
        "sched-none": f"echo none > {q}/scheduler",
        "sched-mq-deadline": f"echo mq-deadline > {q}/scheduler",
        "max-sectors-hw": f"echo {hw} > {q}/max_sectors_kb",
        "nr-requests-1024": f"echo none > {q}/scheduler; echo 1024 > {q}/nr_requests",
    }
    reset = f"echo {sched0} > {q}/scheduler; echo {base[-2]} > {q}/max_sectors_kb"
    for t in (1, 2, 3):  # round-robin: drift hits every variant alike
        for name, cmd in variants.items():
            g(f"sudo -n sh -c '{reset}; {cmd}'")
            gfio(g, f"seqw-t{t}-{name}", SEQW)
    g(f"sudo -n sh -c '{reset}'")


def seqw(g):
    for t in (1, 2, 3):
        gfio(g, f"seqw-{sys.argv[3]}-t{t}", SEQW)


def irq(g):
    line = g("grep virtio /proc/interrupts")[1]
    log("guest /proc/interrupts (virtio):\n" + line)
    dev = g("basename $(readlink -f /sys/block/vda/device)")[1].strip()
    num = next(ln.split(":")[0].strip() for ln in line.splitlines() if ln.split() and ln.split()[-1].startswith(dev))
    nproc = int(g("nproc")[1].strip())
    booted = g(f"cat /proc/irq/{num}/smp_affinity_list")[1].strip()
    log(f"virtio-blk IRQ {num}, smp_affinity_list as booted: {booted}, {nproc} vCPUs")
    for t in range(1, 9):
        for tag, aff in (("booted", booted), ("off0", f"1-{nproc - 1}")):
            g(f"sudo -n sh -c 'echo {aff} > /proc/irq/{num}/smp_affinity_list'")
            gfio(g, f"irq-t{t}-{tag}", RANDR)
    g(f"sudo -n sh -c 'echo {booted} > /proc/irq/{num}/smp_affinity_list'")
    log("guest /proc/interrupts after:\n" + g("grep virtio /proc/interrupts")[1])


def fixes(g, d, vid):
    scratch = "/var/lib/boat/bb-r11-scratch"  # same btrfs as the images, removed below
    for img in ("ubuntu-24.04", "ubuntu-full"):
        src = f"/var/lib/boat/images/{img}/rootfs.ext4"
        log(f"{img}: " + sh(f"sudo filefrag {src}; sudo lsattr {src}; sudo du -h --apparent-size {src}; sudo du -h {src}").replace("\n", " | "))
        sh(f"sudo mkdir -p {scratch}")
        for i in (1, 2, 3):
            t = sh(f"sudo bash -c 'chattr +C {scratch} 2>/dev/null; s=$(date +%s%N); cp --reflink=always {src} {scratch}/c{i}; "
                   f"e=$(date +%s%N); echo $(( (e - s) / 1000 ))'")
            log(f"{img}: FICLONE (cp --reflink=always) #{i}: {t} us")
        sh(f"sudo rm -rf {scratch}")
    log(f"scratch removed: {sh(f'test -e {scratch} && echo no || echo yes')}")
    log("guest boot: " + g("cat /proc/cmdline; systemd-analyze; systemd-analyze critical-chain boat-agent.service | tail -n +3 | head -n 8; "
                           "systemctl show -p ConditionResult --value ldconfig.service; "
                           "sudo -n dmesg | grep -m1 'Run /' ; sudo -n dmesg | wc -l")[1])
    # The snapshot: same dirty memory as the fix's before (1 GiB random + /usr page cache).
    g("sudo -n sh -c 'head -c 1G /dev/urandom >/dev/shm/r; find /usr -type f -size -4M -exec cat {} + >/dev/null 2>&1; sync'", 600)
    t0 = time.time()
    snap = d.snapshot(vid, "r11-fixes")
    log(f"snapshot {snap}: {time.time() - t0:.2f}s")
    log("journal: " + sh(f"sudo journalctl -u boatd --since '-10min' --no-pager -o cat | grep {vid}"))
    log(f"snapshot mem: " + sh(f"sudo sh -c 'for f in /var/lib/boat/snapshots/*/mem /var/lib/boat/teams/*/snapshots/*/mem; do test -e $f && filefrag $f && lsattr $f; done'"))
    if snap:
        d.delete_snapshot(snap)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/snapshots"')
        log(f"deleted snapshot; GET /v1/snapshots now: {left}")


def flush(g):
    """Do guest fsyncs reach the drive? nvme0n1's flush counter (/sys/block/*/stat fields 16-17) around a
    20 s 4k random write with an fsync after every write (workloads.py's fsync job), under boatd's current cache."""
    dev = sh("findmnt -no SOURCE /var/lib/boat").split("/")[-1]
    stat = lambda: sh(f"cat /sys/block/{dev}/stat").split()  # noqa: E731
    a = stat()
    gfio(g, f"flush-{sys.argv[3]}", ["--name=f", "--size=256m", "--runtime=20", "--rw=randwrite", "--bs=4k",
                                     "--iodepth=1", "--fsync=1"])
    b = stat()
    log(f"{dev} flushes during the run: {int(b[15]) - int(a[15])} (writes completed: {int(b[4]) - int(a[4])})")


if sys.argv[1] == "fixes":
    vm_session(fixes, fio=False)
else:
    vm_session({"knobs": knobs, "seqw": seqw, "irq": irq, "flush": flush}[sys.argv[1]])
