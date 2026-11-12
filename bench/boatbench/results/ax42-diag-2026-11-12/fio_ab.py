"""Host vs VM fio on the AX42 boat store. Runs ON the server:
  nix shell nixpkgs#fio nixpkgs#python3 nixpkgs#e2fsprogs -c python3 fio_ab.py host OUT
  BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) nix shell nixpkgs#python3 nixpkgs#util-linux -c python3 fio_ab.py vm OUT
Every fio run's full JSON goes to OUT/<label>.json; OUT/log.txt has the sequence and the host facts."""
import json, os, secrets, subprocess, sys, time

sys.path.insert(0, os.environ.get("BB_DIR", "/tmp/bbfio"))
COMMON = ["--direct=1", "--ioengine=libaio", "--numjobs=1", "--time_based", "--group_reporting", "--output-format=json"]
PTS = ["--name=test", "--size=1g", "--iodepth=64", "--startdelay=20", "--ramp_time=5", "--runtime=60"]  # pts/fio-2.1.0
SHORT = ["--name=v", "--size=1g", "--ramp_time=5", "--runtime=20"]  # variants: shorter, same file
SCRATCH = "/var/lib/boat/bb-fio-scratch"  # same btrfs as boatd's stateDir, deleted at the end
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
    rd, wr = j["read"], j["write"]
    io = rd if rd["io_bytes"] else wr
    log(f"{label}: {io['iops']:.0f} IOPS, {io['bw_bytes'] / 2**20:.1f} MiB/s, clat p50 {io['clat_ns']['percentile'].get('50.000000', 0) / 1000:.1f} us")


def host_facts():
    log("host: " + sh("uname -r; nixos-version; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor "
                      "/sys/devices/system/cpu/cpu0/cpufreq/energy_performance_preference; findmnt -no SOURCE,FSTYPE,OPTIONS /var/lib/boat; "
                      "cat /sys/block/nvme0n1/queue/scheduler; free -m | sed -n 2p; uptime").replace("\n", " | "))


def host_fio(label, extra, filename):
    raw = sh(" ".join(["sudo", "fio"] + COMMON + extra + [f"--filename={filename}"]))
    open(os.path.join(OUT, label + ".json"), "w").write(raw)
    summary(label, raw)


def host():
    host_facts()
    sh(f"sudo mkdir -p {SCRATCH}/nocow {SCRATCH}/cow && sudo chattr +C {SCRATCH}/nocow")
    log("attrs: " + sh(f"sudo lsattr -d {SCRATCH}/nocow {SCRATCH}/cow").replace("\n", " | "))
    try:
        for d in ("nocow", "cow"):
            f = f"{SCRATCH}/{d}/f"
            host_fio(f"host-{d}-pts-seqwrite", PTS + ["--rw=write", "--bs=1M"], f)
            for i in (1, 2, 3):
                host_fio(f"host-{d}-pts-randread-{i}", PTS + ["--rw=randread", "--bs=4k"], f)
            log(f"{d} extents: " + sh(f"sudo filefrag {f} | tail -1"))
        f = f"{SCRATCH}/nocow/f"
        host_fio("host-nocow-randread-qd1", SHORT + ["--rw=randread", "--bs=4k", "--iodepth=1"], f)
        host_fio("host-nocow-randread-iouring-qd64", SHORT + ["--rw=randread", "--bs=4k", "--iodepth=64", "--ioengine=io_uring"], f)
        sh(f"sudo cat {f} >/dev/null")  # warm the page cache, then read it buffered: what a VM sees on a cache hit
        host_fio("host-nocow-randread-buffered-warm", SHORT + ["--rw=randread", "--bs=4k", "--iodepth=64", "--direct=0"], f)
    finally:
        sh(f"sudo rm -rf {SCRATCH}")
        log("scratch removed: " + str(not os.path.exists(SCRATCH)))


def vm():
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    log("boatd Async refusals in journal: " + sh("sudo journalctl -u boatd --no-pager | grep -c 'Async disk engine refused'"))
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        log("guest: " + g("uname -r; nproc; cat /sys/block/vda/queue/scheduler; findmnt -no SOURCE,FSTYPE,OPTIONS /")[1].replace("\n", " | "))
        rc, so, se = g("command -v fio || (sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
                       "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio)) >/dev/null 2>&1; fio --version")
        log(f"guest fio: {so.strip()} {se.strip()[-200:]}")
        disk = sh(f"sudo find /var/lib/boat/teams -path '*{vid}*' -type f -size +1G | head -1")
        log(f"disk file: {disk}; attrs {sh(f'sudo lsattr {disk}')}; io_engine/cache from boatd: Async (default auto), Unsafe")
        f = "$HOME/bbdisk/f"

        def gfio(label, extra):
            rc, so, se = g(" ".join(["mkdir -p $HOME/bbdisk; fio"] + COMMON + extra + [f"--filename={f}"]), 400)
            open(os.path.join(OUT, label + ".json"), "w").write(so)
            try:
                summary(label, so)
            except Exception:
                log(f"{label}: failed rc={rc} {se[-300:]}")

        def resident():
            return sh(f"sudo fincore -b -n -o RES {disk}")

        for t in (1, 2, 3):
            g(f"rm -f {f}")
            gfio(f"vm-t{t}-pts-seqwrite", PTS + ["--rw=write", "--bs=1M"])
            log(f"t{t} host page cache of the VM disk after write: {resident()} bytes")
            gfio(f"vm-t{t}-pts-randread-cached", PTS + ["--rw=randread", "--bs=4k"])
            sh("sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null")
            log(f"t{t} dropped host caches: {resident()} bytes resident")
            gfio(f"vm-t{t}-pts-randread-dropped", PTS + ["--rw=randread", "--bs=4k"])
            log(f"t{t} after the read: {resident()} bytes resident")
        gfio("vm-randread-qd1", SHORT + ["--rw=randread", "--bs=4k", "--iodepth=1"])
        gfio("vm-randread-iouring-qd64", SHORT + ["--rw=randread", "--bs=4k", "--iodepth=64", "--ioengine=io_uring"])
        # the original sequence (workloads.DISK), twice, to see the split in situ
        import workloads
        for i in (1, 2):
            out = workloads.parse_bb_json(g(workloads.DISK, 900)[1])
            open(os.path.join(OUT, f"vm-disk-script-{i}.json"), "w").write(json.dumps(out, indent=1))
            log(f"DISK script {i}: {json.dumps(out)}; resident {resident()}")
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def wb():
    """The 56k / 220k split: does a slow read coincide with host writeback of the VM's dirty pages?
    A guest write lands in the host page cache (Unsafe); the read after it either overlaps the flusher or not.
    Odd trials read right after the write, even ones after a host `sync`. Host Dirty/Writeback sampled every 1 s."""
    import threading
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        f = "$HOME/bbdisk/f"
        for t in range(1, 7):
            g(f"mkdir -p $HOME/bbdisk; rm -f {f}")
            g(" ".join(["fio"] + COMMON + PTS + ["--rw=write", "--bs=1M", f"--filename={f}"]), 400)
            synced = t % 2 == 0
            if synced:
                sh("sync")
            samples, stop = [], threading.Event()

            def sample():
                t0 = time.time()
                while not stop.is_set():
                    m = dict(ln.split(":", 1) for ln in open("/proc/meminfo"))
                    samples.append((round(time.time() - t0), int(m["Dirty"].split()[0]) // 1024,
                                    int(m["Writeback"].split()[0]) // 1024))
                    time.sleep(1)
            th = threading.Thread(target=sample)
            th.start()
            rc, so, se = g(" ".join(["fio"] + COMMON + PTS + ["--rw=randread", "--bs=4k", f"--filename={f}"]), 400)
            stop.set()
            th.join()
            label = f"wb-t{t}-{'sync' if synced else 'nosync'}-randread"
            open(os.path.join(OUT, label + ".json"), "w").write(so)
            open(os.path.join(OUT, label + "-meminfo.json"), "w").write(json.dumps(
                {"columns": ["s_since_read_start", "Dirty_MiB", "Writeback_MiB"], "samples": samples}))
            summary(label, so)
            wbs = [s for s in samples if s[2] > 0]
            log(f"  dirty at read start {samples[0][1] if samples else '?'} MiB; Writeback>0 in {len(wbs)} samples "
                f"(s {wbs[0][0] if wbs else '-'}..{wbs[-1][0] if wbs else '-'}); measured window is s 25..85")
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def nvme_reads():
    return int(open("/sys/block/nvme0n1/stat").read().split()[0])


def wb2():
    """Same as wb (no host sync), but records where the reads go: NVMe read I/Os during the guest's read,
    the disk file's host page-cache residency, and the VM cgroup's file / file_dirty / memory.current."""
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        disk = sh(f"sudo find /var/lib/boat/teams -path '*{vid}*' -type f -size +1G | head -1")
        pid = sh(f"pgrep -f 'firecracker.*{vid}' | head -1") or sh(f"pgrep -f '{vid}' | tail -1")
        cg = sh(f"cat /proc/{pid}/cgroup").split("::")[-1]
        log(f"disk {disk}; attrs {sh(f'sudo lsattr {disk}')}; firecracker pid {pid}; cgroup {cg}; "
            f"memory.high {sh(f'cat /sys/fs/cgroup{cg}/memory.high')} memory.max {sh(f'cat /sys/fs/cgroup{cg}/memory.max')}")

        def state(tag):
            st = dict(ln.split() for ln in open(f"/sys/fs/cgroup{cg}/memory.stat") if ln.split()[0] in ("anon", "file", "file_dirty", "file_writeback"))
            cur = int(open(f"/sys/fs/cgroup{cg}/memory.current").read())
            res = sh(f"sudo fincore -b -n -o RES {disk}") or "0"
            log(f"  {tag}: cgroup memory.current {cur >> 20} MiB, " + ", ".join(f"{k} {int(v) >> 20} MiB" for k, v in st.items())
                + f"; disk file resident {int(res) >> 20} MiB")
        f = "$HOME/bbdisk/f"
        for t in range(1, 5):
            g(f"mkdir -p $HOME/bbdisk; rm -f {f}")
            g(" ".join(["fio"] + COMMON + PTS + ["--rw=write", "--bs=1M", f"--filename={f}"]), 400)
            state(f"t{t} after write")
            n0 = nvme_reads()
            rc, so, se = g(" ".join(["fio"] + COMMON + PTS + ["--rw=randread", "--bs=4k", f"--filename={f}"]), 400)
            n1 = nvme_reads()
            label = f"wb2-t{t}-nosync-randread"
            open(os.path.join(OUT, label + ".json"), "w").write(so)
            summary(label, so)
            log(f"  t{t}: nvme0n1 read I/Os during the read: {n1 - n0:,}")
            state(f"t{t} after read")
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def life():
    """Why create and new-from-snapshot pay ~150-250 ms in boatd's materialize on this store:
    extents of the image and of a snapshot, and the cost of one reflink copy of each into scratch."""
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    imgs = sh("sudo find /var/lib/boat/images -type f -size +100M")
    sh(f"sudo mkdir -p {SCRATCH}")
    try:
        for img in imgs.split():
            log(f"image {img}: attrs {sh(f'sudo lsattr {img}').split()[0]}; {sh(f'sudo filefrag {img}').split(': ')[-1]}; "
                f"size {sh(f'sudo du -h --apparent-size {img}').split()[0]} apparent, {sh(f'sudo du -h {img}').split()[0]} used")
            log("  compsize: " + sh(f"sudo compsize {img} | tail -n +2").replace("\n", " | "))
            t0 = time.time()
            sh(f"sudo cp --reflink=always {img} {SCRATCH}/clone && sync")
            log(f"  one reflink copy into scratch: {(time.time() - t0) * 1000:.0f} ms (incl. sync)")
            sh(f"sudo rm -f {SCRATCH}/clone")
        name = f"bb-fio-{secrets.token_hex(3)}"
        vid, secs = boatbench.ready(d, lambda: d.create(name))
        log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
        try:
            t0 = time.time()
            sid = d.snapshot(vid, name + "-s")
            log(f"snapshot {sid} in {time.time() - t0:.2f}s")
            for fpath in sh(f"sudo find /var/lib/boat/teams -path '*{sid}*' -type f").split():
                log(f"  {fpath.split('/')[-1]}: attrs {sh(f'sudo lsattr {fpath}').split()[0]}; "
                    f"{sh(f'sudo filefrag {fpath}').split(': ')[-1]}; compsize: "
                    + sh(f"sudo compsize {fpath} | tail -n +3").replace("\n", " | "))
            t0 = time.time()
            vid2, secs2 = boatbench.ready(d, lambda: d.from_snapshot(sid, name + "-f"))
            log(f"new-from-snapshot {vid2} exec-ready in {secs2:.2f}s")
            d.delete(vid2)
            d.delete_snapshot(sid)
        finally:
            d.delete(vid)
        log("boatd journal for these VMs: " + sh(f"sudo journalctl -u boatd --since '-10min' --no-pager -o cat | grep -E '{vid}|materialize|snapshot' | tail -20").replace("\n", " | "))
    finally:
        sh(f"sudo rm -rf {SCRATCH}")
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        snaps = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/snapshots"')
        log(f"scratch removed: {not os.path.exists(SCRATCH)}; GET /v1/vms: {left}; GET /v1/snapshots: {snaps}")


def wb3():
    """Same reads as wb2, sampling where Firecracker's threads run (ps psr = logical CPU) every 2 s, and each
    thread's CPU time over the read. Logical CPUs n and n+8 are SMT siblings on this 8-core part (lscpu)."""
    import threading
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    log("SMT siblings of cpu0: " + sh("cat /sys/devices/system/cpu/cpu0/topology/thread_siblings_list"))
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        pid = sh(f"pgrep -f 'firecracker.*{vid}' | head -1")
        f = "$HOME/bbdisk/f"

        def threads():
            out = {}
            for tid in os.listdir(f"/proc/{pid}/task"):
                st = open(f"/proc/{pid}/task/{tid}/stat").read()
                comm = st[st.index("(") + 1:st.rindex(")")]
                rest = st[st.rindex(")") + 2:].split()
                out[f"{comm}:{tid}"] = (int(rest[36]), int(rest[11]) + int(rest[12]))  # processor, utime+stime ticks
            return out
        for t in range(1, 7):
            g(f"mkdir -p $HOME/bbdisk; rm -f {f}")
            g(" ".join(["fio"] + COMMON + PTS + ["--rw=write", "--bs=1M", f"--filename={f}"]), 400)
            samples, stop = [], threading.Event()

            def sample():
                t0 = time.time()
                while not stop.is_set():
                    samples.append((round(time.time() - t0), threads()))
                    time.sleep(2)
            th = threading.Thread(target=sample)
            th.start()
            rc, so, se = g(" ".join(["fio"] + COMMON + PTS + ["--rw=randread", "--bs=4k", f"--filename={f}"]), 400)
            stop.set()
            th.join()
            label = f"wb3-t{t}-nosync-randread"
            open(os.path.join(OUT, label + ".json"), "w").write(so)
            open(os.path.join(OUT, label + "-threads.json"), "w").write(json.dumps(
                {"columns": "s_since_read_start -> {thread: [logical cpu, utime+stime ticks]}", "samples": samples}))
            summary(label, so)
            win = [x for x in samples if 30 <= x[0] <= 80]
            if len(win) >= 2:
                a, b = win[0][1], win[-1][1]
                dt = win[-1][0] - win[0][0]
                busy = {k: round((b[k][1] - a[k][1]) / 100 / dt, 2) for k in b if k in a and b[k][1] - a[k][1] > 5}
                cpus = {k: sorted({x[1][k][0] for x in win if k in x[1]}) for k in busy}
                log(f"  t{t} busy threads in s 30-80 (CPUs used, logical cpu ids seen): "
                    + "; ".join(f"{k} {busy[k]} on {cpus[k]}" for k in busy))
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def irq():
    """Test the colocation explanation: pin the guest's fio to the vCPU that takes virtio-blk's interrupt, then to
    another vCPU. Short PTS-style reads (20 s) on a file written first, 3 of each, alternating."""
    import drivers
    import boatbench
    d = drivers.PandaOS()
    host_facts()
    name = f"bb-fio-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        f = "$HOME/bbdisk/f"
        g(f"mkdir -p $HOME/bbdisk; rm -f {f}")
        g(" ".join(["fio"] + COMMON + PTS + ["--rw=write", "--bs=1M", f"--filename={f}"]), 400)
        before = g("grep -i virtio /proc/interrupts")[1]
        log("guest /proc/interrupts (virtio):\n" + before)
        dev = g("basename $(readlink -f /sys/block/vda/device)")[1].strip()  # virtioN behind vda
        line = next(ln for ln in before.splitlines() if ln.split() and ln.split()[-1].startswith(dev))
        nproc = int(g("nproc")[1].strip())
        counts = [int(x) for x in line.split()[1:1 + nproc]]
        irq_cpu = counts.index(max(counts))
        other = (irq_cpu + 2) % nproc
        log(f"virtio-blk interrupt line: {line.strip()[:120]} -> vCPU {irq_cpu}; other vCPU {other}")
        for t in range(1, 4):
            for tag, cpu in (("same", irq_cpu), ("other", other)):
                label = f"irq-t{t}-fio-on-{tag}-vcpu{cpu}"
                rc, so, se = g(" ".join([f"taskset -c {cpu} fio"] + COMMON + SHORT + ["--rw=randread", "--bs=4k", "--iodepth=64", f"--filename={f}"]), 200)
                open(os.path.join(OUT, label + ".json"), "w").write(so)
                summary(label, so)
        log("guest /proc/interrupts after:\n" + g("grep -i virtio /proc/interrupts")[1])
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


{"host": host, "vm": vm, "wb": wb, "wb2": wb2, "life": life, "wb3": wb3, "irq": irq}[sys.argv[1]]()
