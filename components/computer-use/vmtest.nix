# panda-cu headless in a VM. scenario.py drives one `panda-cu mcp` stdio
# session as `agent`, inside a sandbox like pandacode-host's
# (ProtectSystem=strict, PrivateTmp, NoNewPrivileges): screenshot size and
# latency, the editor (launch, AT-SPI text area, set_text + typing, ctrl+s
# and the save dialog writing ~/hello.txt), Set-of-Marks, clicks by
# element_id and by coordinates on a grid of 80 GTK buttons (accuracy),
# cu_act, clipboard, windows, refused inputs. Another user cannot reach the
# session.
{ pkgs, self }:
let
  # A GTK 3 test app: 80 buttons that log their label when clicked.
  buttons = pkgs.stdenv.mkDerivation {
    name = "cu-buttons";
    src = ./buttons.py;
    dontUnpack = true;
    nativeBuildInputs = [ pkgs.gobject-introspection pkgs.wrapGAppsHook3 ];
    buildInputs = [ pkgs.gtk3 (pkgs.python3.withPackages (ps: [ ps.pygobject3 ])) ];
    installPhase = "install -Dm755 $src $out/bin/cu-buttons";
  };
  t = import ../../tests/lib.nix { inherit (pkgs) lib; };
  scenario = pkgs.writeScript "cu-scenario" ("#!${pkgs.python3}/bin/python3\n" + builtins.readFile ./scenario.py);
in
pkgs.testers.runNixOSTest {
  name = "panda-cu";
  nodes.machine =
    { config, ... }:
    {
      imports = [
        self.nixosModules.default
      ];
      networking.hostName = pkgs.lib.mkForce "machine";
      pandaos.firstboot.enable = false;
      virtualisation.memorySize = 3072;
      virtualisation.cores = 2;
      services.panda-cu = {
        enable = true;
        extraApps.buttons = {
          name = "CU Buttons";
          command = [ "${buttons}/bin/cu-buttons" ];
        };
      };
      users.users.other.isNormalUser = true;
      # `agent` (non-wheel system user) comes from pandacode-host in the default module.
    };
  testScript = t.prelude + ''
    start_all()
    machine.wait_for_unit("panda-cu.service")
    machine.wait_for_file("/run/panda-cu/ready", timeout=90)
    print(machine.succeed("cat /run/panda-cu/ready; ls -la /run/panda-cu"))

    with subtest("mcp.d entry, config, session ownership"):
        entry = json.loads(machine.succeed("cat /etc/panda/mcp.d/panda-cu.json"))
        assert entry["command"] == "panda-cu" and entry["args"] == ["mcp"], entry
        cfg = json.loads(machine.succeed("cat /etc/panda/cu.json"))
        assert cfg["mode"] == "headless" and (cfg["width"], cfg["height"]) == (1280, 800), cfg
        assert sorted(cfg["apps"]) == ["buttons", "editor", "files"], cfg["apps"]
        assert machine.succeed("stat -c '%a %U' /run/panda-cu").split() == ["700", "agent"]
        # Xwayland is off by default.
        machine.fail("pgrep -x Xwayland")

    with subtest("another user cannot reach the session"):
        out = machine.fail("runuser -u other -- panda-cu call cu_screenshot 2>&1")
        print(out)

    with subtest("computer-use scenario as agent over MCP stdio"):
        cmd = (
            "systemd-run --wait --pipe --quiet --collect -p User=agent"
            " -p ProtectSystem=strict -p ReadWritePaths=/var/lib/agent -p PrivateTmp=yes -p NoNewPrivileges=yes"
            " -p WorkingDirectory=/var/lib/agent -E PATH=/run/current-system/sw/bin"
            " ${scenario} /run/current-system/sw/bin/panda-cu 2>&1"
        )
        status, out = machine.execute(cmd, timeout=900)
        print(out)
        if status != 0:
            print(machine.succeed("journalctl -u panda-cu --no-pager | tail -n 200"))
        assert status == 0, "scenario failed"
        metrics = json.loads(next(l for l in out.splitlines() if l.startswith("RESULT "))[7:])

    with subtest("hello.txt has the exact content"):
        assert machine.succeed("cat /var/lib/agent/hello.txt").rstrip("\n") == "Hello, panda-cu!"
        assert machine.succeed("stat -c %U /var/lib/agent/hello.txt").strip() == "agent"

    emit({"test": "panda-cu", **metrics})
    assert metrics["click_accuracy"] >= 0.9, metrics
  '';
}
