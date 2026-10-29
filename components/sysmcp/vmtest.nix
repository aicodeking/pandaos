# panda-sys over MCP stdio in VMs: every tool answers, mutations are refused
# without services.panda-sys.allowMutations, a non-wheel caller cannot read
# journals outside the PandaOS unit allow-list, and (node `mut`) with
# allowMutations the non-wheel `agent` starts a stub nixos-upgrade.service
# through the polkit rule, which allows nothing else.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "panda-sys";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      ./module.nix
    ];
    networking.hostName = pkgs.lib.mkForce "machine";
    pandaos.firstboot.enable = false;
    services.panda-sys.enable = true;
    services.panda-sys.connectivityUrl = "http://127.0.0.1:9/";
    # `agent` (non-wheel system user) comes from pandacode-host in the default module.
  };
  nodes.mut = {
    imports = [
      ./module.nix
      ../agent-env/module.nix
    ];
    services.panda-sys = {
      enable = true;
      allowMutations = true;
      connectivityUrl = "http://127.0.0.1:9/";
    };
    users.users.agent.isNormalUser = true; # not wheel; updateUsers default
    users.users.other.isNormalUser = true; # neither wheel nor in the group
    # Stub upgrade: proves the start went through without building anything.
    systemd.services.nixos-upgrade = {
      serviceConfig.Type = "oneshot";
      script = "date > /run/stub-upgraded";
    };
  };
  testScript = ''
    import json
    import shlex

    start_all()
    machine.wait_for_unit("multi-user.target")


    def mcp(user, calls, node=machine):
        """One stdio session: initialize, then tools/call each (name, args)."""
        reqs = [
            {"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
        ] + [
            {"jsonrpc": "2.0", "id": i + 2, "method": "tools/call", "params": {"name": n, "arguments": a}}
            for i, (n, a) in enumerate(calls)
        ]
        body = "\n".join(json.dumps(r) for r in reqs) + "\n"
        out = node.succeed(
            f"printf %s {shlex.quote(body)} | runuser -u {user} -- timeout 120 panda-sys mcp"
        )
        resps = {r["id"]: r for r in map(json.loads, out.splitlines())}
        assert resps[0]["result"]["serverInfo"]["name"] == "panda-sys", resps[0]
        names = [t["name"] for t in resps[1]["result"]["tools"]]
        return names, [resps[i + 2]["result"] for i in range(len(calls))]


    def text(r):
        return r["content"][0]["text"]


    with subtest("mcp.d entry and config"):
        entry = json.loads(machine.succeed("cat /etc/panda/mcp.d/panda-sys.json"))
        assert entry == {"command": "panda-sys", "args": ["mcp"]}, entry
        assert json.loads(machine.succeed("cat /etc/panda/sys.json"))["allowMutations"] is False

    calls = [
        ("sys_status", {}),
        ("sys_services", {}),
        ("sys_logs", {"unit": "sshd", "lines": 5}),
        ("sys_top", {"n": 3}),
        ("sys_net", {}),
        ("sys_updates", {}),
        ("sys_ports", {}),
        ("sys_ckpt_space", {}),
        ("sys_updates", {"action": "run", "confirm": True}),
        ("sys_logs", {"lines": 501}),
    ]

    with subtest("every tool answers (wheel caller)"):
        names, res = mcp("panda", calls)
        assert sorted(names) == sorted({n for n, _ in calls}), names
        for (n, a), r in zip(calls[:8], res[:8]):
            print(n, a, text(r)[:400])
            assert r["isError"] is False, f"{n}: {text(r)}"
        st = json.loads(text(res[0]))
        assert st["hostname"] == "machine" and st["cpus"] >= 1 and st["memory"]["totalKB"] > 0, st
        assert any(d["mount"] == "/" for d in st["disks"]), st
        units = {u["Id"]: u for u in json.loads(text(res[1]))["units"]}
        assert units["sshd.service"]["ActiveState"] == "active", units
        assert json.loads(text(res[3]))["byCPU"], text(res[3])
        net = json.loads(text(res[4]))
        assert net["connectivity"]["ok"] is False and isinstance(net["interfaces"], list), net
        assert any(p["local"].endswith(":22") for p in json.loads(text(res[6]))["listening"]), text(res[6])
        assert all(("source" in e) or ("error" in e) for e in json.loads(text(res[7]))), text(res[7])

    with subtest("mutations refused without allowMutations; bad input rejected"):
        assert res[8]["isError"] is True and "mutations are disabled" in text(res[8]), res[8]
        assert res[9]["isError"] is True and "1..500" in text(res[9]), res[9]
        machine.fail("systemctl is-active nixos-upgrade.service")

    with subtest("non-wheel caller: journal allow-list"):
        _, res = mcp("agent", [
            ("sys_logs", {"unit": "systemd-journald"}),
            ("sys_logs", {"unit": "panda-mem", "lines": 5}),
            ("sys_updates", {"action": "run", "confirm": True}),
        ])
        assert res[0]["isError"] is True and "not readable" in text(res[0]), res[0]
        # Allowed; journald may still hide system logs from a non-wheel user.
        assert "not readable" not in text(res[1]), text(res[1])
        assert res[2]["isError"] is True, res[2]

    with subtest("polkit: agent (group panda-sys-update) starts nixos-upgrade only"):
        mut.wait_for_unit("multi-user.target")
        assert "panda-sys-update" in mut.succeed("id -nG agent").split()
        assert "panda-sys-update" not in mut.succeed("id -nG other").split()
        _, res = mcp("agent", [("sys_updates", {"action": "run"}), ("sys_updates", {"action": "run", "confirm": True})], node=mut)
        assert res[0]["isError"] is True and "confirm" in text(res[0]), res[0]
        assert res[1]["isError"] is False, text(res[1])
        assert json.loads(text(res[1]))["started"] == "nixos-upgrade.service", text(res[1])
        mut.wait_for_file("/run/stub-upgraded")
        mut.wait_until_succeeds("systemctl show -p ActiveState nixos-upgrade.service | grep -q =inactive")
        _, res = mcp("agent", [("sys_updates", {})], node=mut)
        st = json.loads(text(res[0]))
        assert st["status"]["Result"] == "success" and st["onHold"] is False, st
        # The rule covers this unit and verb only.
        mut.fail("runuser -u agent -- systemctl start --no-block systemd-tmpfiles-clean.service")
        mut.fail("runuser -u agent -- systemctl stop nixos-upgrade.service")
        mut.fail("runuser -u other -- systemctl start --no-block nixos-upgrade.service")
        _, res = mcp("other", [("sys_updates", {"action": "run", "confirm": True})], node=mut)
        assert res[0]["isError"] is True and "panda-sys-update" in text(res[0]), res[0]
        mut.succeed("mkdir -p /var/lib/pandaos && echo test > /var/lib/pandaos/update-hold")
        _, res = mcp("agent", [("sys_updates", {"action": "run", "confirm": True})], node=mut)
        assert res[0]["isError"] is True and "on hold" in text(res[0]), res[0]
  '';
}
