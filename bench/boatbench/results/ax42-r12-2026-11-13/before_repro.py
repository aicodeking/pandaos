"""docs/cost.md "before" figures that were observed but not archived, re-run on the AX42 without changing its config (r12).
Runs ON the server from a copy of bench/boatbench:
  BOAT_URL=http://127.0.0.1:8740 BOAT_KEY=$(cat ~/boat-owner-key) BB_DIR=/tmp/bbr12 \
    nix shell nixpkgs#python3 nixpkgs#e2fsprogs -c python3 before_repro.py OUT irq|image
  irq    one VM bb-r12-<hex>: 4k rand read (iodepth 32, 8 s) with fio pinned to each vCPU, 3 rounds, under: the
         virtio-blk interrupt as booted; moved to another vCPU; rq_affinity 2. Each fio JSON in OUT/irq/.
  image  the pre-fix image layout: the installed ubuntu-24.04 rootfs copied with `cp --sparse=always` into a NOCOW
         scratch dir (how boatd converted images before the fix), its extents, and FICLONE of it vs of the
         installed (dense) image, 3 x each, in a scratch dir under /var/lib/boat that is removed at the end."""
import json, os, secrets, subprocess, sys, time

sys.path.insert(0, os.environ.get("BB_DIR", "/tmp/bbr12"))
OUT = sys.argv[1]
os.makedirs(os.path.join(OUT, "irq"), exist_ok=True)
LOG = open(os.path.join(OUT, "log.txt"), "a")
FIO = ("fio --direct=1 --ioengine=libaio --numjobs=1 --time_based --group_reporting --output-format=json --name=r "
       "--filename=$HOME/bbdisk/f --size=2G --iodepth=32 --rw=randread --bs=4k --runtime=8")


def log(msg):
    line = f"{time.strftime('%H:%M:%SZ', time.gmtime())} {msg}"
    print(line, flush=True)
    LOG.write(line + "\n")
    LOG.flush()


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def irq():
    import boatbench
    import drivers
    d = drivers.PandaOS()
    name = f"bb-r12-{secrets.token_hex(3)}"
    vid, secs = boatbench.ready(d, lambda: d.create(name))
    log(f"created {name} {vid}, exec-ready in {secs:.2f}s")
    try:
        g = lambda c, t=600: d.exec(vid, c, t)[1]  # noqa: E731
        g("sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio >/dev/null 2>&1 || "
          "(sudo -n apt-get update -qq && sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fio) >/dev/null 2>&1")
        g("mkdir -p $HOME/bbdisk; fio --name=w --filename=$HOME/bbdisk/f --size=2G --direct=1 --rw=write --bs=1M "
          "--ioengine=libaio --iodepth=32 >/dev/null", 300)
        dev = g("basename $(readlink -f /sys/block/vda/device)").strip()
        num = next(ln.split(":")[0].strip() for ln in g("grep virtio /proc/interrupts").splitlines()
                   if ln.split() and ln.split()[-1] == dev)
        n = int(g("nproc").strip())
        booted = g(f"cat /proc/irq/{num}/smp_affinity_list").strip()
        rqa0 = g("cat /sys/block/vda/queue/rq_affinity").strip()
        log(f"virtio-blk {dev} IRQ {num}, smp_affinity_list {booted}, rq_affinity {rqa0}, {n} vCPUs; "
            "interrupts: " + g(f"grep -E '^ *{num}:' /proc/interrupts"))

        def setup(aff, rqa):
            g(f"sudo -n sh -c 'echo {aff} > /proc/irq/{num}/smp_affinity_list; echo {rqa} > /sys/block/vda/queue/rq_affinity'")
        for rnd in (1, 2, 3):
            for tag, aff, rqa in (("booted", booted, rqa0), ("irq-moved", "1" if booted.split("-")[0] != "1" else "2", rqa0),
                                  ("rq-affinity-2", booted, "2")):
                setup(aff, rqa)
                before = g(f"grep -E '^ *{num}:' /proc/interrupts").split()[1:1 + n]
                for cpu in range(n):
                    raw = g(f"{FIO} --cpus_allowed={cpu}", 120)
                    open(os.path.join(OUT, "irq", f"r{rnd}-{tag}-fio-cpu{cpu}.json"), "w").write(raw)
                    iops = json.loads(raw[raw.index("{"):])["jobs"][0]["read"]["iops"]
                    log(f"r{rnd} {tag} (affinity {aff}, rq_affinity {rqa}): fio on vCPU {cpu}: {iops:.0f} IOPS")
                after = g(f"grep -E '^ *{num}:' /proc/interrupts").split()[1:1 + n]
                log(f"r{rnd} {tag}: interrupts per vCPU during the round: {[int(b) - int(a) for a, b in zip(before, after)]}")
        setup(booted, rqa0)
    finally:
        d.delete(vid)
        left = sh('curl -fsS -H "Authorization: Bearer $BOAT_KEY" "$BOAT_URL/v1/vms"')
        log(f"deleted {vid}; GET /v1/vms now: {left}")


def image():
    src = "/var/lib/boat/images/ubuntu-24.04/rootfs.ext4"
    scratch = "/var/lib/boat/bb-r12-scratch"
    sh(f"sudo mkdir -p {scratch} && sudo chattr +C {scratch}")
    sh(f"sudo cp --sparse=always {src} {scratch}/sparse.ext4")
    sh("sync")
    for f in (src, f"{scratch}/sparse.ext4"):
        log(f"{f}: {sh(f'sudo filefrag {f}')} | {sh(f'sudo lsattr {f}')} | allocated {sh(f'sudo du -h {f}').split()[0]}")
        for i in (1, 2, 3):
            us = sh(f"sudo bash -c 's=$(date +%s%N); cp --reflink=always {f} {scratch}/c{i}; e=$(date +%s%N); "
                    f"echo $(( (e - s) / 1000 ))'; sudo rm -f {scratch}/c{i}")
            log(f"FICLONE (cp --reflink=always, wall incl. process start) of {os.path.basename(f)} #{i}: {us} us")
    sh(f"sudo rm -rf {scratch}")
    log(f"scratch removed: {sh(f'test -e {scratch} && echo no || echo yes')}")


{"irq": irq, "image": image}[sys.argv[2]]()
