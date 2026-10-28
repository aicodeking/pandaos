# PandaOS VM tests, exposed as `checks.x86_64-linux.*` by the flake.
#
# Metrics: each test prints `METRIC {json}` in the build log (survives a failed
# run) and writes the same object to `$out/metrics.json` (what you get from
# `nix build .#checks.x86_64-linux.<name>`). bench/collect.py reads either.
{ pkgs, self }:
let
  inherit (pkgs) lib;
  budgets = lib.importJSON ./budgets.json;

  # The base module pins hostName = "pandaos", which would give every node the
  # same name and break `relay` resolution, so each node forces its own.
  nodeName = name: { networking.hostName = lib.mkForce name; };

  # Multi-node tests: keep NetworkManager off the test VLAN so the driver's
  # static eth1 addresses stay put; agentd is what's under test, not NM.
  vlanNode = name: {
    imports = [ (nodeName name) ];
    networking.networkmanager.enable = lib.mkForce false;
  };

  prelude = ''
    import json
    import os
    import time


    def emit(metrics):
        print("METRIC " + json.dumps(metrics, sort_keys=True))
        out = os.environ.get("out")
        if out:
            with open(os.path.join(out, "metrics.json"), "w") as f:
                json.dump(metrics, f, indent=2, sort_keys=True)


    def idle_ram_mb(m):
        time.sleep(30)
        return int(m.succeed(
            "awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{print int((t-a)/1024)}' /proc/meminfo"
        ).strip())
  '';

  # Closure size is computed host-side: closureInfo's total-nar-size is the same
  # number `nix path-info -S` prints, but the test VM's Nix DB does not register
  # its own system closure. Note this is the VM toplevel (includes test
  # instrumentation); the real ISO size is printed by CI.
  mkBootTest =
    {
      name,
      profile,
      unitChecks,
      extraConfig ? { },
    }:
    pkgs.testers.runNixOSTest {
      inherit name;
      nodes.machine = {
        imports = [
          self.nixosModules.default
          self.nixosModules.${profile}
          (nodeName "machine")
          extraConfig
        ];
      };
      testScript =
        { nodes, ... }:
        let
          closure = pkgs.closureInfo { rootPaths = [ nodes.machine.system.build.toplevel ]; };
          budget = budgets.${profile};
        in
        prelude
        + ''
          machine.wait_for_unit("multi-user.target")
          ${unitChecks}
          ram = idle_ram_mb(machine)
          with open("${closure}/total-nar-size") as f:
              closure_mb = int(f.read()) // (1024 * 1024)
          emit({"test": "${name}", "profile": "${profile}", "idleRamMB": ram, "closureMB": closure_mb})
          assert ram <= ${toString budget.idleRamMB}, f"idle RAM {ram} MB > budget ${toString budget.idleRamMB} MB"
          assert closure_mb <= ${toString budget.closureMB}, f"closure {closure_mb} MB > budget ${toString budget.closureMB} MB"
        '';
    };

  mkRoundtrip =
    { name, slow }:
    pkgs.testers.runNixOSTest {
      inherit name;
      nodes.relay =
        { config, pkgs, ... }:
        {
          imports = [
            self.nixosModules.default
            (vlanNode "relay")
          ];
          services.panda-agentd.enable = lib.mkForce false;
          networking.firewall.allowedTCPPorts = [ 8787 ];
          environment.systemPackages = [ pkgs.curl ];
          systemd.services.fake-relay = {
            wantedBy = [ "multi-user.target" ];
            after = [ "network.target" ];
            serviceConfig = {
              ExecStart = "${config.services.panda-agentd.package}/bin/fake-relay -listen :8787";
              DynamicUser = true;
            };
          };
        };
      nodes.box = {
        imports = [
          self.nixosModules.default
          (vlanNode "box")
        ];
        services.panda-agentd = {
          enable = true;
          relayUrl = lib.mkForce "http://relay:8787";
        };
        # Slow network: hold agentd back until netem is in place, so pairing
        # itself runs over the bad link.
        systemd.services.panda-agentd.wantedBy = lib.mkIf slow (lib.mkForce [ ]);
        boot.kernelModules = lib.optional slow "sch_netem";
      };
      testScript =
        let
          startAgent = lib.optionalString slow ''
            box.succeed("tc qdisc add dev eth1 root netem delay 300ms loss 2% rate 1mbit")
            box.succeed("systemctl start panda-agentd.service")
          '';
          limit = lib.optionalString slow ''
            assert elapsed <= 120, f"round trip took {elapsed}s over a slow link (limit 120s)"
          '';
        in
        prelude
        + ''
          start_all()
          relay.wait_for_unit("fake-relay.service")
          relay.wait_for_open_port(8787)
          box.wait_for_unit("multi-user.target")
          start = time.monotonic()
          ${startAgent}
          box.wait_for_file("/run/panda-agentd/pairing-code", timeout=120)
          box.wait_for_file("/var/lib/panda-agentd/token", timeout=120)
          job = json.dumps({"id": "echo-hi", "argv": ["echo", "hi"], "timeoutSec": 30, "memoryMaxMB": 64})
          relay.succeed(f"curl -sf -X POST -H 'Content-Type: application/json' -d '{job}' http://localhost:8787/admin/jobs")
          result = json.loads(relay.wait_until_succeeds("curl -sf http://localhost:8787/admin/results/echo-hi", timeout=120))
          elapsed = round(time.monotonic() - start, 1)
          emit({"test": "${name}", "roundtripSec": elapsed})
          assert result["exitCode"] == 0, f"job failed: {result}"
          assert result["stdout"].strip() == "hi", f"unexpected stdout: {result}"
          ${limit}
        '';
    };
in
{
  boot-headless = mkBootTest {
    name = "boot-headless";
    profile = "headless";
    unitChecks = ''
      machine.wait_for_unit("panda-agentd.service")
      machine.wait_for_unit("panda-mem-init.service")
    '';
  };

  boot-desktop = mkBootTest {
    name = "boot-desktop";
    profile = "desktop";
    unitChecks = ''
      machine.wait_for_unit("greetd.service")
    '';
    extraConfig.virtualisation.memorySize = 2048;
  };

  agentd-roundtrip = mkRoundtrip {
    name = "agentd-roundtrip";
    slow = false;
  };

  slow-network = mkRoundtrip {
    name = "slow-network";
    slow = true;
  };

  mem = pkgs.testers.runNixOSTest {
    name = "mem";
    nodes.machine = {
      imports = [
        self.nixosModules.default
        (nodeName "machine")
      ];
      services.panda-mem.enable = true;
    };
    testScript =
      { nodes, ... }:
      prelude
      + ''
        d = "${nodes.machine.services.panda-mem.dir}"
        machine.wait_for_unit("panda-mem-init.service")
        machine.succeed(f"panda-mem write -dir {d} -kind semantic -title 'ci probe' -body 'pandaos-ci-marker body'")
        hits = machine.succeed(f"panda-mem search -dir {d} pandaos-ci-marker")
        assert "ci probe" in hits or "pandaos-ci-marker" in hits, f"search missed the memory: {hits!r}"
        commits = machine.succeed(f"git -c safe.directory='*' -C {d} log --oneline")
        assert commits.strip(), "panda-mem write made no git commit"
        mcp = json.loads(machine.succeed("cat /etc/panda/mcp.json"))
        assert isinstance(mcp, dict) and mcp, f"bad mcp.json: {mcp!r}"
      '';
  };
}
