# panda-cu headless in a VM. scenario.py drives one `panda-cu mcp` stdio
# session as `agent`, inside a sandbox like pandacode-host's
# (ProtectSystem=strict, PrivateTmp, NoNewPrivileges): screenshot size and
# latency; Mousepad's text and its save dialog by keyboard (~/hello.txt);
# Set-of-Marks; p50 of cu_find/cu_act/cu_wait; clicks by element_id and by
# coordinates on 80 GTK buttons and on 24 px buttons (all must hit); a menu
# item and a dialog button by element_id; a fixed-size dialog (hit or
# refused, never missed); scroll and drag verified by the app; Unicode
# multi-line typing; a Chromium form; an app killed mid-task and one that
# crashes at launch (clean errors); clipboard; refused inputs. Another user
# cannot reach the session.
{ pkgs, self }:
let
  # A Python GTK 3 test app.
  gtkApp = name: src: pkgs.stdenv.mkDerivation {
    inherit name src;
    dontUnpack = true;
    nativeBuildInputs = [ pkgs.gobject-introspection pkgs.wrapGAppsHook3 ];
    buildInputs = [ pkgs.gtk3 (pkgs.python3.withPackages (ps: [ ps.pygobject3 ])) ];
    installPhase = "install -Dm755 $src $out/bin/${name}";
  };
  # 80 buttons that log their label when clicked.
  buttons = gtkApp "cu-buttons" ./buttons.py;
  # 24 px buttons, a menu, a slider, a long list, text, two dialogs.
  lab = gtkApp "cu-lab" ./lab.py;
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
      virtualisation.memorySize = 4096;
      virtualisation.cores = 2;
      services.panda-cu = {
        enable = true;
        browser = pkgs.chromium;
        extraApps = {
          buttons = { name = "CU Buttons"; command = [ "${buttons}/bin/cu-buttons" ]; };
          lab = { name = "CU Lab"; command = [ "${lab}/bin/cu-lab" ]; };
          crasher = { name = "Crasher"; command = [ "${pkgs.bash}/bin/sh" "-c" "echo crash-test >&2; exit 7" ]; };
        };
      };
      users.users.other.isNormalUser = true;
      # `agent` (non-wheel system user) comes from pandacode-host in the default module.
    };
  testScript = t.prelude + ''
    start_all()
    machine.wait_for_unit("panda-cu.service")
    try:
        machine.wait_for_file("/run/panda-cu/ready", timeout=90)
    except Exception:
        print(machine.execute("journalctl -b --no-pager -u panda-cu; ps -u agent -o pid,args; ls -la /run/panda-cu")[1])
        raise
    print(machine.succeed("cat /run/panda-cu/ready; ls -la /run/panda-cu"))

    with subtest("mcp.d entry, config, session ownership"):
        entry = json.loads(machine.succeed("cat /etc/panda/mcp.d/panda-cu.json"))
        assert entry["command"] == "panda-cu" and entry["args"] == ["mcp"], entry
        cfg = json.loads(machine.succeed("cat /etc/panda/cu.json"))
        assert cfg["mode"] == "headless" and (cfg["width"], cfg["height"]) == (1280, 800), cfg
        assert sorted(cfg["apps"]) == ["browser", "buttons", "crasher", "editor", "files", "lab"], cfg["apps"]
        assert cfg["allowClipboardRead"] is False, cfg
        unit = machine.succeed("systemctl show panda-cu -p Slice,MemoryMax,TasksMax,ProtectHome,Restart")
        print(unit)
        assert "Slice=agents.slice" in unit and "Restart=always" in unit and "ProtectHome=yes" in unit, unit
        bus = machine.succeed("ps -u agent -o args= | grep -o '/nix/store/[^ ]*panda-cu-bus.conf' | head -1").strip()
        assert 'eavesdrop="true"' not in machine.succeed(f"cat {bus}"), "private bus allows eavesdropping"
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
            " ${scenario} /run/current-system/sw/bin/panda-cu ${./form.html} 2>&1"
        )
        status, out = machine.execute(cmd, timeout=1500)
        print(out)
        metrics = json.loads(next((l for l in out.splitlines() if l.startswith("RESULT ")), "RESULT {}")[7:])
        emit({"test": "panda-cu", **metrics})
        if status != 0:
            print(machine.succeed("journalctl -u panda-cu --no-pager | tail -n 300"))
        assert status == 0, f"scenario failed: {metrics.get('failures')}"

    with subtest("hello.txt has the exact content"):
        assert machine.succeed("cat /var/lib/agent/hello.txt").rstrip("\n") == "Hello, panda-cu!"
        assert machine.succeed("stat -c %U /var/lib/agent/hello.txt").strip() == "agent"

    with subtest("a failing session helper restarts the whole session"):
        pid = machine.succeed("systemctl show -p MainPID --value panda-cu").strip()
        t0 = machine.succeed("date +%s").strip()
        machine.succeed("pkill -KILL -u agent -f 'panda-cu session'")
        machine.wait_until_succeeds(f"test \"$(systemctl show -p MainPID --value panda-cu)\" != {pid}", timeout=60)
        machine.wait_until_succeeds(f"test \"$(stat -c %Y /run/panda-cu/ready)\" -ge {t0}", timeout=90)
        print(machine.succeed("journalctl -u panda-cu --no-pager | grep -i 'session ended' | tail -3"))
  '';
}
