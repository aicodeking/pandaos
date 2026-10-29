# agent-env + pandacode-host runtime:
# - the host runs as the non-wheel `agent` user, who can't touch /home/panda;
# - agents.slice limits; per-task cgroups: the real pandacode-host unit
#   (wrapper, Delegate=yes, oomd) runs a stub host that starts two fake
#   `claude` tasks through the shim; the hog's task is killed while the host,
#   the other task and sshd live, and its empty cgroup is swept;
# - `panda-agents mcp sync` edits fake Claude/Codex configs.
{ pkgs, self }:
let
  fakeClaude = pkgs.writeShellScriptBin "claude" ''
    case "''${1-}" in
      --version) echo "fake-claude 0.0"; exit 0 ;;
      idle | hog) cut -d: -f3 /proc/self/cgroup >"$HOME/$1.cg" ;;
    esac
    if [ "''${1-}" = hog ]; then
      while [ ! -e "$HOME/go" ]; do sleep 1; done
      # Random data: zero pages would just sit in zram for free.
      x=$(head -c 6G /dev/urandom | tr -d '\0')
      echo "hog survived with ''${#x} bytes" >&2
    fi
    exec sleep infinity
  '';
  # Stands in for `pandacode serve`: starts agents by name, like the real one.
  stubHost = pkgs.writeShellScript "stub-host" ''
    claude idle &
    claude hog &
    wait
  '';
in
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
    virtualisation.memorySize = 2048;
    # test-instrumentation.nix sets vm.panic_on_oom = mkDefault 2, which turns
    # the OOM kill under test into a kernel panic. Production keeps 0.
    boot.kernel.sysctl."vm.panic_on_oom" = 0;
    environment.systemPackages = [ fakeClaude pkgs.attr ];
    services.pandacode-host.serveCommand = "${stubHost}";
    # The real ExecStartPre runs pandacode itself; the stub host doesn't need it.
    systemd.services.pandacode-host.serviceConfig.ExecStartPre = pkgs.lib.mkForce [ ];
  };
  testScript = ''
    import json
    import re

    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("sshd.service")
    A = "/var/lib/agent"

    def prop(unit, p):
        return machine.succeed(f"systemctl show -p {p} --value {unit}").strip()

    with subtest("agent user: not in wheel, cannot read or write the owner's home"):
        groups = machine.succeed("id -nG agent").split()
        assert "wheel" not in groups and "panda-mem" in groups, groups
        assert machine.succeed("stat -c '%a %U' /home/panda").strip() == "700 panda"
        assert machine.succeed(f"stat -c '%a %U' {A}").strip() == "700 agent"
        machine.fail("runuser -u agent -- touch /home/panda/.bashrc")
        machine.fail("runuser -u agent -- ls /home/panda")
        machine.fail("runuser -u agent -- sudo -n true")

    with subtest("agents.slice limits; pandacode-host is delegated and oomd-watched"):
        total = int(machine.succeed("awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo"))
        assert prop("agents.slice", "MemoryHigh") == "infinity"  # no throttle, see module.nix
        mx = int(prop("agents.slice", "MemoryMax"))
        assert abs(mx - total * 0.75) < total * 0.02, (mx, total)
        assert prop("agents.slice", "CPUWeight") == "50"
        assert prop("agents.slice", "TasksMax") == "8192"
        print("MemorySwapMax=" + prop("agents.slice", "MemorySwapMax"))
        h = "pandacode-host.service"
        assert prop(h, "User") == "agent"
        assert prop(h, "Slice") == "agents.slice"
        assert prop(h, "OOMPolicy") == "continue"
        assert prop(h, "Delegate") == "yes"
        assert prop(h, "ManagedOOMMemoryPressure") == "kill"
        assert prop(h, "ManagedOOMSwap") == "kill"
        machine.succeed("systemctl is-active systemd-oomd.service")
        for u in ("sshd", "systemd-journald", "NetworkManager", "panda-mem"):
            assert prop(f"{u}.service", "OOMScoreAdjust") == "-500", u

    with subtest("boot sync linked AGENTS.md for the agent user"):
        machine.succeed(f"test \"$(readlink {A}/AGENTS.md)\" = /etc/panda/AGENTS.md")
        machine.succeed("grep -q 'own cgroup' /etc/panda/AGENTS.md")
        assert "panda-mem" in machine.succeed("panda-agents mcp list")

    with subtest("mcp sync writes fake claude/codex configs, keeps user entries, idempotent"):
        machine.succeed(
            "runuser -u agent -- sh -c '"
            "mkdir -p ~/.claude ~/.codex && "
            "echo \"{\\\"mcpServers\\\":{\\\"mine\\\":{\\\"command\\\":\\\"x\\\"}},\\\"theme\\\":\\\"dark\\\"}\" > ~/.claude.json && "
            "printf \"[mcp_servers.mine]\\ncommand = \\\"x\\\"\\n\" > ~/.codex/config.toml'"
        )
        # As root for another user: drops to agent, files stay agent's.
        machine.succeed("panda-agents mcp sync --user agent")
        claude = json.loads(machine.succeed(f"cat {A}/.claude.json"))
        assert claude["theme"] == "dark" and "mine" in claude["mcpServers"], claude
        assert claude["mcpServers"]["panda-mem"]["command"] == "panda-mem", claude
        codex = machine.succeed(f"cat {A}/.codex/config.toml")
        assert "[mcp_servers.mine]" in codex and "[mcp_servers.panda-mem]" in codex, codex
        files = f"{A}/.claude.json {A}/.codex/config.toml"
        assert machine.succeed(f"stat -c %U {files}").split() == ["agent", "agent"]
        before = machine.succeed(f"sha256sum {files}")
        machine.succeed("runuser -u agent -- panda-agents mcp sync")
        assert machine.succeed(f"sha256sum {files}") == before
        print(machine.succeed("runuser -u agent -- panda-agents doctor"))

    with subtest("each shimmed agent run gets a task cgroup; the host sits in host/ (oomd_avoid)"):
        machine.succeed("touch /var/lib/pandacode-host/signed-in && systemctl start pandacode-host.service")
        machine.wait_until_succeeds(f"test -s {A}/idle.cg && test -s {A}/hog.cg", timeout=60)
        root = "/sys/fs/cgroup" + prop("pandacode-host.service", "ControlGroup")
        idle_cg = "/sys/fs/cgroup" + machine.succeed(f"cat {A}/idle.cg").strip()
        hog_cg = "/sys/fs/cgroup" + machine.succeed(f"cat {A}/hog.cg").strip()
        for cg in (idle_cg, hog_cg):
            assert re.fullmatch(re.escape(root) + r"/task-\d+-\d+", cg), (cg, root)
        assert idle_cg != hog_cg
        main = prop("pandacode-host.service", "MainPID")
        assert main in machine.succeed(f"cat {root}/host/cgroup.procs").split()
        assert machine.succeed(f"getfattr -n user.oomd_avoid --only-values {root}/host").strip() == "1"
        assert "memory" in machine.succeed(f"cat {root}/cgroup.subtree_control")
        assert machine.succeed(f"cat {hog_cg}/memory.oom.group").strip() == "1"

    with subtest("the hog's task is OOM-killed; host, the other task and sshd live; empty cgroup swept"):
        sshd_pid = prop("sshd.service", "MainPID")
        machine.succeed(f"touch {A}/go")
        machine.wait_until_succeeds(
            f"! test -d {hog_cg} || grep -qx 'populated 0' {hog_cg}/cgroup.events", timeout=240
        )
        print(machine.succeed("journalctl -b -k -u systemd-oomd -u pandacode-host --no-pager | grep -iE 'oom|kill' | tail -n 20 || true"))
        machine.succeed("systemctl is-active pandacode-host.service")
        assert prop("pandacode-host.service", "MainPID") == main, "host restarted"
        machine.succeed(f"grep -qx 'populated 1' {idle_cg}/cgroup.events")
        machine.succeed("systemctl is-active sshd.service")
        assert prop("sshd.service", "MainPID") == sshd_pid, "sshd restarted"
        machine.wait_until_fails(f"test -d {hog_cg}", timeout=90)
  '';
}
