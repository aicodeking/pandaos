# panda-ckpt on a real btrfs disk, everything run unprivileged as `panda`:
# init, write, create, modify, diff, restore, fork, prune, the auto timer
# (+ retention), an MCP round trip, and delete without user_subvol_rm_allowed.
# The disk is mounted over panda's home at runtime (like @home), so the test
# does not depend on the initrd flavour; tmpfiles is re-run to make ~/work.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "panda-ckpt";
  nodes.machine = {
    # agent-env owns the mcp.d registry the module registers into.
    imports = [ ./module.nix ../agent-env/module.nix ];
    boot.supportedFilesystems = [ "btrfs" ];
    virtualisation.emptyDiskImages = [ 1024 ];
    environment.systemPackages = [ pkgs.btrfs-progs ];
    users.users.panda = {
      isNormalUser = true;
      group = "users";
    };
    services.panda-ckpt = {
      enable = true;
      interval = "15s";
      keepAuto = 2;
    };
  };
  testScript = ''
    import json
    import shlex

    H = "/home/panda"


    def p(cmd):
        return machine.succeed(f"runuser -l panda -c {shlex.quote(cmd)}")


    def pj(cmd):
        return json.loads(p(cmd + " --json"))


    def ckpts(ws):
        return pj(f"panda-ckpt list {ws}")


    def autos(ws):
        return [c for c in ckpts(ws) if c["reason"] == "auto"]


    def is_subvol(path):
        machine.succeed(f"btrfs subvolume show {path}")


    machine.wait_for_unit("multi-user.target")
    machine.succeed(
        "mkfs.btrfs -f /dev/vdb",
        f"mount -o user_subvol_rm_allowed /dev/vdb {H}",
        f"chown panda:users {H} && chmod 700 {H}",
        "systemd-tmpfiles --create",
    )

    with subtest("module: ~/work is a subvolume, MCP config, timer"):
        is_subvol(f"{H}/work")
        assert machine.succeed(f"stat -c %U {H}/work {H}/.checkpoints").split() == ["panda", "panda"]
        cfg = json.loads(machine.succeed("cat /etc/panda/mcp.d/panda-ckpt.json"))
        assert cfg == {"command": "panda-ckpt", "args": ["mcp"]}, cfg
        machine.succeed("systemctl is-active panda-ckpt-auto-panda.timer")
        # Paused until its own subtest so it cannot race the exact-count checks.
        machine.succeed("systemctl stop panda-ckpt-auto-panda.timer panda-ckpt-auto-panda.service")

    with subtest("init, write, create (read-only snapshot + metadata)"):
        ws = pj("panda-ckpt init proj")
        assert ws["path"] == f"{H}/work/proj", ws
        is_subvol(ws["path"])
        p("printf 'v1\\n' > ~/work/proj/a.txt && mkdir ~/work/proj/d && echo keep > ~/work/proj/d/b.txt")
        c1 = pj("panda-ckpt create proj -m first")
        snap = f"{H}/.checkpoints/proj/{c1['id']}"
        assert c1["label"] == "first" and c1["reason"] == "manual" and c1["sizeEstimate"] == 8, c1
        assert "ro=true" in machine.succeed(f"btrfs property get -ts {snap} ro")
        assert machine.succeed(f"stat -c %U {snap}").strip() == "panda"
        meta = json.loads(machine.succeed(f"cat {snap}.json"))
        assert set(meta) == {"id", "created", "label", "reason", "parent", "sizeEstimate"}, meta
        machine.fail(f"runuser -u panda -- touch {snap}/x")

    with subtest("modify and diff"):
        p("printf 'v2\\n' > ~/work/proj/a.txt && rm ~/work/proj/d/b.txt && echo new > ~/work/proj/new.txt")
        d = pj(f"panda-ckpt diff proj {c1['id']} current")
        kinds = {c["path"]: c["kind"] for c in d}
        assert kinds == {"a.txt": "modified", "d/b.txt": "deleted", "new.txt": "added"}, kinds
        a = next(c for c in d if c["path"] == "a.txt")
        assert "-v1" in a["diff"] and "+v2" in a["diff"], a

    with subtest("restore brings content back, after a safety checkpoint"):
        r = pj(f"panda-ckpt restore proj {c1['id']}")
        assert p("cat ~/work/proj/a.txt") == "v1\n"
        assert p("cat ~/work/proj/d/b.txt") == "keep\n"
        p("test ! -e ~/work/proj/new.txt")
        is_subvol(f"{H}/work/proj")
        machine.fail(f"test -e {H}/work/.proj.restore")
        safety = [c for c in ckpts("proj") if c["id"] == r["safety"]]
        assert safety and safety[0]["reason"] == "pre-restore", r
        assert len(pj(f"panda-ckpt diff proj {r['safety']}")) == 3

    with subtest("fork is writable and independent"):
        f = pj(f"panda-ckpt fork proj {c1['id']} proj-b")
        is_subvol(f["path"])
        p("echo forked > ~/work/proj-b/a.txt")
        assert p("cat ~/work/proj/a.txt") == "v1\n"
        assert p("cat ~/work/proj-b/a.txt") == "forked\n"

    with subtest("prune deletes snapshots unprivileged"):
        extra = pj("panda-ckpt create proj -m extra")
        before = [c["id"] for c in ckpts("proj")]
        pruned = pj("panda-ckpt prune proj --keep 1")
        assert pruned == before[:-1] and [c["id"] for c in ckpts("proj")] == [extra["id"]], pruned
        machine.fail(f"test -e {H}/.checkpoints/proj/{c1['id']}")

    with subtest("auto timer checkpoints changed workspaces, keeps keepAuto"):
        p("echo tick > ~/work/proj/t.txt")
        machine.succeed("systemctl start panda-ckpt-auto-panda.timer")
        machine.wait_until_succeeds(
            "runuser -l panda -c 'panda-ckpt list proj --json' | grep -q '\"reason\": \"auto\"'", timeout=90
        )
        machine.wait_until_succeeds("runuser -l panda -c 'panda-ckpt list proj-b --json' | grep -q auto", timeout=90)
        n = len(autos("proj"))
        machine.succeed("systemctl start panda-ckpt-auto-panda.service")
        assert len(autos("proj")) == n, "unchanged workspace got a new auto checkpoint"
        for i in range(3):
            p(f"echo {i} > ~/work/proj/t.txt")
            machine.succeed("systemctl start panda-ckpt-auto-panda.service")
        assert len(autos("proj")) == 2, autos("proj")
        assert extra["id"] in [c["id"] for c in ckpts("proj")], "manual checkpoint was pruned"
        machine.succeed("systemctl stop panda-ckpt-auto-panda.timer panda-ckpt-auto-panda.service")

    with subtest("MCP round trip"):
        reqs = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            {"jsonrpc": "2.0", "id": 3, "method": "tools/call",
             "params": {"name": "ckpt_create", "arguments": {"workspace": "proj", "label": "via mcp"}}},
            {"jsonrpc": "2.0", "id": 4, "method": "tools/call",
             "params": {"name": "ckpt_restore", "arguments": {"workspace": "proj", "id": extra["id"]}}},
            {"jsonrpc": "2.0", "id": 5, "method": "tools/call",
             "params": {"name": "ckpt_list", "arguments": {"workspace": "proj"}}},
        ]
        body = "\n".join(json.dumps(r) for r in reqs) + "\n"
        machine.succeed(f"printf %s {shlex.quote(body)} > /tmp/mcp.jsonl && chmod 644 /tmp/mcp.jsonl")
        resps = [json.loads(line) for line in p("panda-ckpt mcp < /tmp/mcp.jsonl").splitlines()]
        assert [r["id"] for r in resps] == [1, 2, 3, 4, 5], resps
        names = {t["name"] for t in resps[1]["result"]["tools"]}
        assert names == {"ckpt_create", "ckpt_list", "ckpt_diff", "ckpt_restore", "ckpt_fork"}, names
        created = json.loads(resps[2]["result"]["content"][0]["text"])
        assert not resps[2]["result"]["isError"] and created["label"] == "via mcp", resps[2]
        assert resps[3]["result"]["isError"] and "confirm" in resps[3]["result"]["content"][0]["text"], resps[3]
        listed = json.loads(resps[4]["result"]["content"][0]["text"])
        assert created["id"] in [c["id"] for c in listed], listed

    with subtest("delete still works without user_subvol_rm_allowed (rm + rmdir fallback)"):
        machine.succeed(f"umount {H}", f"mount /dev/vdb {H}")
        assert "user_subvol_rm_allowed" not in machine.succeed(f"findmnt -no OPTIONS {H}")
        pj("panda-ckpt prune proj --keep 1")
        assert len(ckpts("proj")) == 1
        pj("panda-ckpt gc")
  '';
}
