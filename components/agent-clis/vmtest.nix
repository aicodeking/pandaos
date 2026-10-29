# pandaos.agents.clis = [ "codex" ]: codex is on the agent user's PATH and
# `codex --version` runs with no network (test VMs have none). Also runs the
# panda-agents-install unit test and checks the installed wrapper validates.
{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "agent-clis";
  nodes.machine = {
    imports = [ ./module.nix ];
    pandaos.agents.clis = [ "codex" ];
    users.users.agent = {
      isNormalUser = true;
      group = "users";
    };
  };
  testScript = ''
    machine.wait_for_unit("multi-user.target")
    with subtest("codex on agent's PATH, --version offline"):
        path = machine.succeed("runuser -l agent -c 'command -v codex'").strip()
        assert path == "/run/current-system/sw/bin/codex", path
        print(machine.succeed("runuser -l agent -c 'codex --version'"))
    with subtest("only selected CLIs installed"):
        machine.fail("runuser -l agent -c 'command -v claude'")
    with subtest("panda-agents-install validates"):
        machine.succeed("panda-agents-install --check codex claude-code opencode gemini-cli")
        machine.fail("panda-agents-install --check kimi")
        machine.fail("panda-agents-install --check nope")
        # Not an installed system: refuses and leaves /etc alone.
        machine.fail("panda-agents-install codex")
        machine.fail("test -e /etc/pandaos/agents.nix")
    with subtest("unit test"):
        machine.succeed("bash ${./test-agents-install.sh} ${./panda-agents-install.sh}")
  '';
}
