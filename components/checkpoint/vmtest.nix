# panda-ckpt on a real btrfs disk, everything run unprivileged as the non-wheel `agent`:
# init, write, create, modify, diff, restore, fork, prune, the auto timer
# (+ retention), MCP (round trip, init, adopt refused while busy), an external
# git repo (discover, reflink checkpoint, restore, auto), and delete without
# user_subvol_rm_allowed.
# The disk is mounted over agent's home at runtime (like @home), so the test
# does not depend on the initrd flavour; tmpfiles and panda-ckpt-init are
# re-run to make ~/.checkpoints and ~/work on it.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "panda-ckpt";
  nodes.machine = {
    # agent-env owns the mcp.d registry the module registers into.
    imports = [ ./module.nix ../agent-env/module.nix ];
    boot.supportedFilesystems = [ "btrfs" ];
    virtualisation.emptyDiskImages = [ 1024 ];
    environment.systemPackages = [ pkgs.btrfs-progs pkgs.git ];
    users.users.agent = {
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

    H = "/home/agent"


    def p(cmd):
        return machine.succeed(f"runuser -l agent -c {shlex.quote(cmd)}")


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
        f"chown agent:users {H} && chmod 700 {H}",
        "systemd-tmpfiles --create",
        "systemctl restart panda-ckpt-init-agent.service",
    )

    with subtest("module: ~/work is a subvolume, MCP config, timer"):
        is_subvol(f"{H}/work")
        assert machine.succeed(f"stat -c %U {H}/work {H}/.checkpoints").split() == ["agent", "agent"]
        cfg = json.loads(machine.succeed("cat /etc/panda/mcp.d/panda-ckpt.json"))
        assert cfg == {"command": "panda-ckpt", "args": ["mcp"]}, cfg
        machine.succeed("systemctl is-active panda-ckpt-auto-agent.timer")
        # Paused until its own subtest so it cannot race the exact-count checks.
        machine.succeed("systemctl stop panda-ckpt-auto-agent.timer panda-ckpt-auto-agent.service")

    with subtest("init, write, create (read-only snapshot + metadata)"):
        ws = pj("panda-ckpt init proj")
        assert ws["path"] == f"{H}/work/proj", ws
        is_subvol(ws["path"])
        p("printf 'v1\\n' > ~/work/proj/a.txt && mkdir ~/work/proj/d && echo keep > ~/work/proj/d/b.txt")
        c1 = pj("panda-ckpt create proj -m first")
        snap = f"{H}/.checkpoints/proj/{c1['id']}"
        assert c1["label"] == "first" and c1["reason"] == "manual" and c1["sizeEstimate"] == 8, c1
        assert "ro=true" in machine.succeed(f"btrfs property get -ts {snap} ro")
        assert machine.succeed(f"stat -c %U {snap}").strip() == "agent"
        meta = json.loads(machine.succeed(f"cat {snap}.json"))
        assert set(meta) == {"id", "created", "label", "reason", "parent", "sizeEstimate"}, meta
        machine.fail(f"runuser -u agent -- touch {snap}/x")

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
        machine.succeed("systemctl start panda-ckpt-auto-agent.timer")
        machine.wait_until_succeeds(
            "runuser -l agent -c 'panda-ckpt list proj --json' | grep -q '\"reason\": \"auto\"'", timeout=90
        )
        machine.wait_until_succeeds("runuser -l agent -c 'panda-ckpt list proj-b --json' | grep -q auto", timeout=90)
        n = len(autos("proj"))
        machine.succeed("systemctl start panda-ckpt-auto-agent.service")
        assert len(autos("proj")) == n, "unchanged workspace got a new auto checkpoint"
        for i in range(3):
            p(f"echo {i} > ~/work/proj/t.txt")
            machine.succeed("systemctl start panda-ckpt-auto-agent.service")
        assert len(autos("proj")) == 2, autos("proj")
        assert extra["id"] in [c["id"] for c in ckpts("proj")], "manual checkpoint was pruned"
        machine.succeed("systemctl stop panda-ckpt-auto-agent.timer panda-ckpt-auto-agent.service")

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
        assert names == {"ckpt_create", "ckpt_list", "ckpt_diff", "ckpt_restore", "ckpt_fork", "ckpt_init", "ckpt_adopt", "ckpt_discover"}, names
        created = json.loads(resps[2]["result"]["content"][0]["text"])
        assert not resps[2]["result"]["isError"] and created["label"] == "via mcp", resps[2]
        assert resps[3]["result"]["isError"] and "confirm" in resps[3]["result"]["content"][0]["text"], resps[3]
        listed = json.loads(resps[4]["result"]["content"][0]["text"])
        assert created["id"] in [c["id"] for c in listed], listed

    def mcp(calls):
        """One stdio session as agent: initialize, then tools/call each (name, args)."""
        reqs = [{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {"protocolVersion": "2099-01-01"}}] + [
            {"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": {"name": n, "arguments": a}}
            for i, (n, a) in enumerate(calls)
        ]
        body = "\n".join(json.dumps(r) for r in reqs) + "\n"
        machine.succeed(f"printf %s {shlex.quote(body)} > /tmp/mcp2.jsonl && chmod 644 /tmp/mcp2.jsonl")
        resps = {r["id"]: r for r in map(json.loads, p("panda-ckpt mcp < /tmp/mcp2.jsonl").splitlines())}
        assert resps[0]["result"]["protocolVersion"] == "2025-06-18", resps[0]
        return [resps[i + 1]["result"] for i in range(len(calls))]


    def text(r):
        return r["content"][0]["text"]

    with subtest("MCP: ckpt_init, ckpt_adopt refused while a process is inside, then confirmed"):
        p("mkdir -p ~/work/plain && echo hi > ~/work/plain/f.txt")
        machine.succeed("runuser -u agent -- sh -c 'cd /home/agent/work/plain && exec sleep 600' >/dev/null 2>&1 &")
        machine.wait_until_succeeds("pgrep -u agent -x sleep")
        res = mcp([
            ("ckpt_init", {"workspace": "fresh"}),
            ("ckpt_adopt", {"path": "~/work/plain"}),
        ])
        assert not res[0]["isError"], text(res[0])
        is_subvol(f"{H}/work/fresh")
        assert res[1]["isError"] and "confirm" in text(res[1]), text(res[1])
        machine.fail(f"btrfs subvolume show {H}/work/plain")
        res = mcp([("ckpt_adopt", {"path": "~/work/plain", "confirm": True})])
        assert not res[0]["isError"], text(res[0])
        is_subvol(f"{H}/work/plain")
        assert p("cat ~/work/plain/f.txt") == "hi\n"
        machine.succeed("pkill -u agent -x sleep")

    with subtest("external git repo: discover, reflink checkpoint, restore in place, auto"):
        repo = f"{H}/src/app"
        p("mkdir -p ~/src/app && cd ~/src/app && git init -q && echo v1 > main.txt"
          " && git add . && git -c user.name=t -c user.email=t@t commit -qm init")
        found = pj("panda-ckpt discover")
        assert [r["path"] for r in found] == [repo], found
        e1 = pj(f"panda-ckpt create {repo} -m before")
        assert e1["source"] == repo, e1
        esnap = f"{H}/.checkpoints/_ext/{found[0]['slug']}/{e1['id']}"
        is_subvol(esnap)
        assert "ro=true" in machine.succeed(f"btrfs property get -ts {esnap} ro")
        p("cd ~/src/app && echo v2 > main.txt && echo junk > junk.txt")
        kinds = {c["path"]: c["kind"] for c in pj(f"panda-ckpt diff ~/src/app {e1['id']}")}
        assert kinds == {"main.txt": "modified", "junk.txt": "added"}, kinds
        res = mcp([("ckpt_restore", {"workspace": repo, "id": e1["id"], "confirm": True})])
        assert not res[0]["isError"], text(res[0])
        assert p("cat ~/src/app/main.txt") == "v1\n"
        p("test ! -e ~/src/app/junk.txt && cd ~/src/app && test -z \"$(git status --porcelain)\"")
        p("cd ~/src/app && git log --oneline | grep -q init")
        safety = json.loads(text(res[0]))["safety"]
        # The `git status` above refreshed .git/index, so compare the work tree only.
        d = pj(f"panda-ckpt diff {repo} {safety}")
        kinds = {c["path"]: c["kind"] for c in d if not c["path"].startswith(".git/")}
        assert kinds == {"main.txt": "modified", "junk.txt": "deleted"}, d
        p("echo v3 > ~/src/app/main.txt")
        machine.succeed("systemctl start panda-ckpt-auto-agent.service")
        assert any(c["reason"] == "auto" for c in ckpts(repo)), ckpts(repo)
        assert p("cat ~/src/app/main.txt") == "v3\n", "auto must not touch the repo"

    with subtest("delete still works without user_subvol_rm_allowed (rm + rmdir fallback)"):
        machine.succeed(f"umount {H}", f"mount /dev/vdb {H}")
        assert "user_subvol_rm_allowed" not in machine.succeed(f"findmnt -no OPTIONS {H}")
        pj("panda-ckpt prune proj --keep 1")
        assert len(ckpts("proj")) == 1
        pj("panda-ckpt gc")
  '';
}
