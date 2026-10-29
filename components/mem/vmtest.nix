# panda-mem in a VM, used by the non-wheel `agent` through the socket, over
# MCP stdio: protocol negotiation, annotations, write (overwrite refused
# without overwrite:true), append, log_episode, delete (needs confirm), and
# the commits each makes in the repo (owned by the panda-mem user).
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "panda-mem-tools";
  nodes.machine = {
    imports = [ ./module.nix ];
    users.users.agent.isNormalUser = true; # not wheel
    environment.systemPackages = [ pkgs.git ];
    services.panda-mem = {
      enable = true;
      clients = [ "agent" ];
    };
  };
  testScript = ''
    import json
    import shlex

    machine.wait_for_unit("panda-mem.service")
    machine.wait_for_file("/run/panda-mem/sock")


    def mcp(calls, version="2025-06-18"):
        reqs = [{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {"protocolVersion": version}},
                {"jsonrpc": "2.0", "method": "notifications/initialized"},
                {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}] + [
            {"jsonrpc": "2.0", "id": i + 2, "method": "tools/call", "params": {"name": n, "arguments": a}}
            for i, (n, a) in enumerate(calls)
        ]
        body = "\n".join(json.dumps(r) for r in reqs) + "\n"
        machine.succeed(f"printf %s {shlex.quote(body)} > /tmp/mem.jsonl && chmod 644 /tmp/mem.jsonl")
        out = machine.succeed("runuser -u agent -- panda-mem mcp -socket /run/panda-mem/sock < /tmp/mem.jsonl")
        resps = {r["id"]: r for r in map(json.loads, out.splitlines())}
        return resps[0]["result"], resps[1]["result"]["tools"], [resps[i + 2]["result"] for i in range(len(calls))]


    def text(r):
        return r["content"][0]["text"]

    with subtest("negotiation and tool metadata"):
        init, tools, _ = mcp([], version="2024-11-05")
        assert init["protocolVersion"] == "2024-11-05", init
        init, _, _ = mcp([], version="1999-01-01")
        assert init["protocolVersion"] == "2025-06-18", init
        names = {t["name"] for t in tools}
        assert {"mem_write", "mem_append", "mem_delete", "mem_log_episode"} <= names, names
        for t in tools:
            assert {"readOnlyHint", "destructiveHint", "idempotentHint"} <= set(t["annotations"]), t
            for p, s in t["inputSchema"]["properties"].items():
                assert s.get("description"), (t["name"], p)

    with subtest("write, overwrite refused, append, episode, delete"):
        _, _, res = mcp([
            ("mem_write", {"kind": "procedural", "title": "deploy", "body": "step 1"}),
            ("mem_write", {"kind": "procedural", "title": "deploy", "body": "clobber"}),
            ("mem_append", {"title": "deploy", "text": "step 2"}),
            ("mem_log_episode", {"task": "Ship the deploy doc", "summary": "wrote it", "outcome": "success", "files": ["docs/deploy.md"]}),
            ("mem_delete", {"path": "procedural/x.md"}),
        ])
        path = text(res[0])
        assert not res[0]["isError"] and path.startswith("procedural/deploy-"), res[0]
        assert res[1]["isError"] and "mem_append" in text(res[1]), res[1]
        assert not res[2]["isError"] and text(res[2]) == path, res[2]
        ep = text(res[3])
        assert not res[3]["isError"] and ep.startswith("episodic/") and "ship-the-deploy-doc" in ep, res[3]
        assert res[4]["isError"] and "confirm" in text(res[4]), res[4]
        body = machine.succeed(f"cat /mem/{path}")
        assert "step 1" in body and "step 2" in body and "clobber" not in body and "\n### " in body, body
        assert "- docs/deploy.md" in machine.succeed(f"cat /mem/{ep}")

        _, _, res = mcp([("mem_delete", {"path": path, "confirm": True}), ("mem_read", {"path": path})])
        assert not res[0]["isError"] and res[1]["isError"], res
        machine.fail(f"test -e /mem/{path}")
        commits = machine.succeed("runuser -u panda-mem -- git -C /mem log --format=%s")
        assert f"mem: delete {path}" in commits and f"mem: append {path}" in commits and f"mem: episode {ep}" in commits, commits
  '';
}
