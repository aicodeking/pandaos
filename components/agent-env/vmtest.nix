# agent-env: agents.slice limits, oomd kills a hog in the slice while sshd
# lives, and `panda-agents mcp sync` edits fake Claude/Codex configs.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "agent-env";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      ./module.nix # no-op once nixosModules.default imports it
    ];
    networking.hostName = pkgs.lib.mkForce "machine";
    pandaos.firstboot.enable = false;
    virtualisation.memorySize = 1536;
  };
  testScript = ''
    import json

    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("sshd.service")

    def prop(unit, p):
        return machine.succeed(f"systemctl show -p {p} --value {unit}").strip()

    with subtest("agents.slice has its limits; pandacode-host and system units are wired"):
        total = int(machine.succeed("awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo"))
        high, mx = int(prop("agents.slice", "MemoryHigh")), int(prop("agents.slice", "MemoryMax"))
        assert abs(high - total * 0.75) < total * 0.02, (high, total)
        assert abs(mx - total * 0.85) < total * 0.02, (mx, total)
        assert prop("agents.slice", "CPUWeight") == "50"
        assert prop("agents.slice", "TasksMax") == "8192"
        assert prop("agents.slice", "ManagedOOMMemoryPressure") == "kill"
        print("MemorySwapMax=" + prop("agents.slice", "MemorySwapMax"))
        assert prop("pandacode-host.service", "Slice") == "agents.slice"
        assert prop("pandacode-host.service", "OOMPolicy") == "continue"
        machine.succeed("systemctl is-active systemd-oomd.service")
        for u in ("sshd", "systemd-journald", "NetworkManager", "panda-mem"):
            assert prop(f"{u}.service", "OOMScoreAdjust") == "-500", u

    with subtest("a memory hog in agents.slice is killed; sshd survives"):
        sshd_pid = prop("sshd.service", "MainPID")
        # Random data: zero pages would just sit in zram for free.
        machine.succeed(
            # systemd-run units get systemd's default PATH, not NixOS's.
            "systemd-run --unit=hog --slice=agents.slice --setenv=PATH=/run/current-system/sw/bin "
            "bash -c 'x=$(head -c 6G /dev/urandom | tr -d \"\\0\"); sleep 600'"
        )
        machine.wait_until_fails("systemctl is-active hog.service", timeout=240)
        result = prop("hog.service", "Result")
        print(f"hog Result={result}")
        print(machine.succeed("journalctl -b -u systemd-oomd -u hog --no-pager | tail -n 20"))
        assert result in ("oom-kill", "signal"), result
        machine.succeed("systemctl is-active sshd.service")
        assert prop("sshd.service", "MainPID") == sshd_pid, "sshd restarted"

    with subtest("boot sync linked AGENTS.md"):
        machine.succeed("test \"$(readlink /home/panda/AGENTS.md)\" = /etc/panda/AGENTS.md")
        machine.succeed("grep -q 'agents.slice' /etc/panda/AGENTS.md")
        assert "panda-mem" in machine.succeed("panda-agents mcp list")

    with subtest("mcp sync writes fake claude/codex configs, keeps user entries, idempotent"):
        machine.succeed(
            "runuser -u panda -- sh -c '"
            "mkdir -p ~/.claude ~/.codex && "
            "echo \"{\\\"mcpServers\\\":{\\\"mine\\\":{\\\"command\\\":\\\"x\\\"}},\\\"theme\\\":\\\"dark\\\"}\" > ~/.claude.json && "
            "printf \"[mcp_servers.mine]\\ncommand = \\\"x\\\"\\n\" > ~/.codex/config.toml'"
        )
        # As root for another user: drops to panda, files stay panda's.
        machine.succeed("panda-agents mcp sync --user panda")
        claude = json.loads(machine.succeed("cat /home/panda/.claude.json"))
        assert claude["theme"] == "dark" and "mine" in claude["mcpServers"], claude
        assert claude["mcpServers"]["panda-mem"]["command"] == "panda-mem", claude
        codex = machine.succeed("cat /home/panda/.codex/config.toml")
        assert "[mcp_servers.mine]" in codex and "[mcp_servers.panda-mem]" in codex, codex
        assert machine.succeed("stat -c %U /home/panda/.claude.json /home/panda/.codex/config.toml").split() == ["panda", "panda"]
        before = machine.succeed("sha256sum /home/panda/.claude.json /home/panda/.codex/config.toml")
        machine.succeed("runuser -u panda -- panda-agents mcp sync")
        assert machine.succeed("sha256sum /home/panda/.claude.json /home/panda/.codex/config.toml") == before
        print(machine.succeed("runuser -u panda -- panda-agents doctor"))
  '';
}
