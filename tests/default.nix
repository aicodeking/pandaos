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
    import re
    import shlex
    import os
    import time


    def emit(metrics):
        print("METRIC " + json.dumps(metrics, sort_keys=True))
        out = os.environ.get("out")
        if out:
            with open(os.path.join(out, "metrics.json"), "w") as f:
                json.dump(metrics, f, indent=2, sort_keys=True)


    def idle_ram_mb(m):
        """Return (used, total_minus_available) in MB after 30s of idle.

        `used` is what `free` reports as used, plus tmpfs/shmem: anon memory,
        unreclaimable slab, kernel stacks, etc. MemTotal - MemAvailable is
        also returned for reference, but it counts the kernel's free-page
        watermark reserve (tens of MB, scaling with VM RAM) as used, so the
        budget is checked against `used`.
        """
        time.sleep(30)
        # Breakdown in the log so a budget miss can be diagnosed from CI alone.
        print(m.succeed("ps -eo rss=,comm= --sort=-rss | head -n 15"))
        mi = {}
        for line in m.succeed("cat /proc/meminfo").splitlines():
            k, v = line.split(":", 1)
            mi[k] = int(v.split()[0])
        print({k: mi[k] for k in ("MemTotal", "MemFree", "MemAvailable", "Buffers", "Cached",
                                  "Shmem", "SReclaimable", "SUnreclaim", "AnonPages")})
        used = mi["MemTotal"] - mi["MemFree"] - mi["Buffers"] - (mi["Cached"] - mi["Shmem"]) - mi["SReclaimable"]
        return used // 1024, (mi["MemTotal"] - mi["MemAvailable"]) // 1024


    def top_paths(registration, n=40):
        """Print the n largest store paths from a closureInfo registration file."""
        # Format per path: path, narHash, narSize, deriver, nRefs, refs...
        lines = open(registration).read().split("\n")
        sizes = []
        referrers = {}
        i = 0
        try:
            while i + 4 < len(lines) and lines[i]:
                sizes.append((int(lines[i + 2]), lines[i]))
                refs = lines[i + 5 : i + 5 + int(lines[i + 4])]
                for r in refs:
                    if r != lines[i]:
                        referrers.setdefault(r, []).append(lines[i][44:])
                i += 5 + len(refs)
        except ValueError as e:
            print(f"top_paths: unexpected registration format near line {i}: {e}")
        for size, path in sorted(sizes, reverse=True)[:n]:
            print(f"{size // (1024 * 1024):6d} MB  {path[44:]}  <- {', '.join(referrers.get(path, [])[:4])}")
  '';

  # Tests that don't exercise first boot keep it off: it would grab tty8 and
  # switch VTs on every boot of a passwordless VM.
  noFirstboot = {
    pandaos.firstboot.enable = false;
  };

  # Closure size is computed host-side: closureInfo's total-nar-size is the same
  # number `nix path-info -S` prints, but the test VM's Nix DB does not register
  # its own system closure. Note this is the VM toplevel (includes test
  # instrumentation); the real ISO sizes are printed by CI.
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
          noFirstboot
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
          ram, ram_minus_avail = idle_ram_mb(machine)
          with open("${closure}/total-nar-size") as f:
              closure_mb = int(f.read()) // (1024 * 1024)
          top_paths("${closure}/registration")
          emit({"test": "${name}", "profile": "${profile}", "idleRamMB": ram,
                "memTotalMinusAvailableMB": ram_minus_avail, "closureMB": closure_mb})
          assert ram <= ${toString budget.idleRamMB}, f"idle RAM {ram} MB > budget ${toString budget.idleRamMB} MB"
          assert closure_mb <= ${toString budget.closureMB}, f"closure {closure_mb} MB > budget ${toString budget.closureMB} MB"
        '';
    };

  # Cancel: a long job is stopped through the relay and reported as cancelled.
  cancelCheck = ''
    with subtest("cancel"):
        job = json.dumps({"id": "sleeper", "argv": ["sleep", "600"], "timeoutSec": 900})
        relay.succeed(f"curl -sf -X POST -H 'Content-Type: application/json' -d '{job}' http://localhost:8787/admin/jobs")
        box.wait_until_succeeds("systemctl is-active panda-job-sleeper.service", timeout=60)
        box.wait_until_succeeds("grep -q '\"running\": 1' /run/panda-agentd/status.json", timeout=30)
        relay.succeed("curl -sf -X POST http://localhost:8787/v1/jobs/sleeper/cancel")
        res = json.loads(relay.wait_until_succeeds("curl -sf http://localhost:8787/admin/results/sleeper", timeout=60))
        assert res.get("cancelled") is True and res["exitCode"] == 130, f"cancel result: {res}"
        box.wait_until_fails("systemctl is-active panda-job-sleeper.service", timeout=30)
        box.wait_until_succeeds("grep -q '\"running\": 0' /run/panda-agentd/status.json", timeout=30)
  '';

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
            noFirstboot
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
          noFirstboot
        ];
        services.panda-agentd = {
          enable = true;
          relayUrl = lib.mkForce "http://relay:8787";
          # Plain http to the single-label test host; production relays are https.
          allowInsecure = true;
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
          # agentd deletes pairing-code once paired, which can happen before we
          # look; the token plus the logged code prove the pairing flow ran.
          box.wait_for_file("/var/lib/panda-agentd/token", timeout=120)
          box.succeed("journalctl -u panda-agentd.service | grep -q 'pairing code:'")
          job = json.dumps({"id": "echo-hi", "argv": ["echo", "hi"], "timeoutSec": 30, "memoryMaxMB": 64})
          relay.succeed(f"curl -sf -X POST -H 'Content-Type: application/json' -d '{job}' http://localhost:8787/admin/jobs")
          result = json.loads(relay.wait_until_succeeds("curl -sf http://localhost:8787/admin/results/echo-hi", timeout=120))
          elapsed = round(time.monotonic() - start, 1)
          emit({"test": "${name}", "roundtripSec": elapsed})
          assert result["exitCode"] == 0, f"job failed: {result}"
          assert result["stdout"].strip() == "hi", f"unexpected stdout: {result}"
          ${limit}
          status = json.loads(box.succeed("cat /run/panda-agentd/status.json"))
          assert status["paired"] is True, f"status.json is not paired: {status}"
          ${lib.optionalString (!slow) cancelCheck}
        '';
    };

  # Desktop shell scripts, run against fixture status files.
  desktopChecks = ''
    machine.wait_for_unit("greetd.service")
    with subtest("desktop: panda-menu and panda-agents-status"):
        machine.succeed("test -x \"$(command -v panda-menu)\"")


        def agents_status(fixture):
            path = "/tmp/fixture-status.json"
            machine.succeed(f"printf %s {shlex.quote(fixture)} > {path}")
            return json.loads(machine.succeed(f"PANDA_AGENTD_STATUS={path} panda-agents-status"))


        busy = agents_status(json.dumps({"paired": True, "slots": 2, "running": 1, "completed": 3,
                                         "failed": 0, "relay": "https://relay.example", "version": "0.2.0"}))
        assert busy["text"] == "Agents 1/2" and busy["class"] == "busy", busy
        assert "completed: 3" in busy["tooltip"], busy
        failed = agents_status(json.dumps({"paired": True, "slots": 2, "running": 0, "failed": 1}))
        assert failed["class"] == "failed", failed
        pairing = agents_status(json.dumps({"paired": False, "pairingCode": "ABC123", "slots": 2}))
        assert pairing["text"] == "Pairing: ABC123" and pairing["class"] == "pairing", pairing
        garbage = agents_status("not json")
        assert garbage["text"] == "Agents: ?", garbage
        off = json.loads(machine.succeed("PANDA_AGENTD_STATUS=/nonexistent panda-agents-status"))
        assert off["class"] == "off", off
        # The real daemon's file (unpaired: the default relay is a placeholder).
        machine.wait_for_file("/run/panda-agentd/status.json")
        real = json.loads(machine.succeed("panda-agents-status"))
        assert real["class"] in ("pairing", "idle"), real
        bars = json.loads(machine.succeed("cat /etc/xdg/waybar/config"))
        assert [b["name"] for b in bars] == ["top", "dock"], bars
        machine.succeed("grep -q swaybg /etc/xdg/labwc/autostart")
  '';

  # The system panda-install copies in the install test: the real prebuilt
  # headless target plus the test backdoor, so the driver can talk to it
  # after it boots from the installed disk.
  installTarget =
    (self.lib.mkPandaSystem {
      profile = "headless";
      modules = [
        ../nixos/installer/target.nix
        "${pkgs.path}/nixos/modules/testing/test-instrumentation.nix"
      ];
    }).config.system.build.toplevel;
  panda-install = pkgs.callPackage ../nixos/installer/package.nix {
    flakeUrl = "github:AICodeKing/pandaos";
    systems.headless = installTarget;
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
    unitChecks = desktopChecks;
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
        noFirstboot
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

        with subtest("snapshots: btrbk skips cleanly without a /.snapshots mount"):
            # The btrfs case runs in the install test, on the installed disk.
            machine.succeed("systemctl start btrbk-panda.service")
            cond = machine.succeed("systemctl show -p ConditionResult --value btrbk-panda.service").strip()
            assert cond == "no", f"btrbk-panda ran on a non-btrfs VM: ConditionResult={cond}"
      '';
  };

  security = pkgs.testers.runNixOSTest {
    name = "security";
    nodes.machine =
      { pkgs, ... }:
      {
        imports = [
          self.nixosModules.default
          self.nixosModules.headless
          (vlanNode "machine")
        ];
        environment.systemPackages = [ pkgs.netcat ];
      };
    nodes.peer =
      { pkgs, ... }:
      {
        imports = [ (vlanNode "peer") ];
        environment.systemPackages = [
          pkgs.netcat
          pkgs.openssh
        ];
      };
    testScript =
      { nodes, ... }:
      let
        sshd = "${nodes.machine.services.openssh.package}/bin/sshd";
        nc = "${pkgs.netcat}/bin/nc";
      in
      prelude
      + ''
        start_all()
        machine.wait_for_unit("multi-user.target")
        machine.wait_for_unit("sshd.service")
        machine.wait_for_unit("sshguard.service")
        machine.wait_for_open_port(22)
        peer.wait_for_unit("multi-user.target")

        with subtest("panda has no usable password; first boot asks for one"):
            h = machine.succeed("getent shadow panda | cut -d: -f2").strip()
            assert not h.startswith("$"), f"panda has a password hash: {h!r}"
            machine.wait_for_unit("panda-firstboot.service")

        with subtest("sshd refuses password auth"):
            cfg = machine.succeed("${sshd} -T -f /etc/ssh/sshd_config").lower().splitlines()
            for want in ("passwordauthentication no", "kbdinteractiveauthentication no",
                         "permitrootlogin no", "maxauthtries 3"):
                assert want in cfg, f"sshd -T is missing {want!r}"
            rc, out = peer.execute(
                "ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10 panda@machine true 2>&1"
            )
            assert rc != 0 and "Permission denied (publickey)" in out, f"ssh: rc={rc} {out!r}"

        with subtest("only ssh is reachable from another node"):
            for line in machine.succeed("ss -ltnH").splitlines():
                local = line.split()[3]
                addr, port = local.rsplit(":", 1)
                if addr.startswith("127.") or addr in ("[::1]",) or "%lo" in addr:
                    continue
                assert port == "22", f"unexpected non-loopback TCP listener: {line}"
            machine.succeed("systemd-run --unit=probe-8080 ${nc} -lk 8080")
            machine.wait_for_open_port(8080)
            peer.succeed("nc -z -w 5 machine 22")
            peer.fail("nc -z -w 5 machine 8080")
            machine.succeed("systemctl stop probe-8080.service")
            machine.succeed("nft list table inet nixos-fw >/dev/null")

        with subtest("kernel sysctls are applied"):
            want = {
                "kernel.kptr_restrict": ("2",),
                "kernel.dmesg_restrict": ("1",),
                # 2 is the kernel's sticky "off" default; 1 is what we set.
                "kernel.unprivileged_bpf_disabled": ("1", "2"),
                "net.core.bpf_jit_harden": ("2",),
                "net.ipv4.conf.all.rp_filter": ("1",),
                "net.ipv4.conf.all.accept_redirects": ("0",),
                "net.ipv4.conf.all.send_redirects": ("0",),
                "net.ipv6.conf.all.accept_redirects": ("0",),
                "net.ipv4.conf.all.accept_source_route": ("0",),
                "fs.protected_symlinks": ("1",),
                "fs.protected_regular": ("2",),
            }
            for key, ok in want.items():
                got = machine.succeed(f"sysctl -n {key}").strip()
                assert got in ok, f"{key} = {got}, want {ok}"
      '';
  };

  browser = pkgs.testers.runNixOSTest {
    name = "browser";
    nodes.machine =
      { pkgs, ... }:
      {
        imports = [
          self.nixosModules.default
          self.nixosModules.headless
          (vlanNode "machine")
          noFirstboot
        ];
        services.panda-browser.enable = true;
        virtualisation.memorySize = 2048;
        environment.systemPackages = [
          pkgs.curl
          pkgs.jq
        ];
      };
    nodes.peer =
      { pkgs, ... }:
      {
        imports = [ (vlanNode "peer") ];
        environment.systemPackages = [ pkgs.curl ];
      };
    testScript = prelude + ''
      start_all()
      machine.wait_for_unit("panda-browser.service")
      machine.wait_for_open_port(9222)
      out = machine.succeed("curl -sf http://127.0.0.1:9222/json/version")
      assert "webSocketDebuggerUrl" in out, out
      machine.succeed("jq -e '.cdp == \"http://127.0.0.1:9222\"' /etc/panda/browser.json")
      # Not listening on any non-loopback address:
      machine.fail("ss -ltnH 'sport = :9222' | grep -v -e '127.0.0.1' -e '\\[::1\\]'")
      # Not reachable from another node:
      peer.wait_for_unit("multi-user.target")
      peer.fail("curl -sf --max-time 5 http://machine:9222/json/version")
      # Fresh profile per start:
      machine.succeed("touch /run/panda-browser/profile/stale-marker")
      machine.succeed("systemctl restart panda-browser.service")
      machine.wait_for_open_port(9222)
      machine.fail("test -e /run/panda-browser/profile/stale-marker")
      machine.succeed("curl -sf http://127.0.0.1:9222/json/version")
    '';
  };

  # Non-interactive panda-install onto an empty disk, then boot that disk.
  # Same shape as nixpkgs' nixos/tests/installer.nix: `target` shares the
  # installer's state dir and boots its second disk through OVMF.
  install = pkgs.testers.runNixOSTest {
    name = "install";
    nodes.installer = {
      virtualisation = {
        useEFIBoot = true; # panda-install refuses non-UEFI boots
        memorySize = 2048;
        emptyDiskImages = [ 10240 ]; # /dev/vdb; panda-install wants >= 8 GiB
      };
      environment.systemPackages = [
        panda-install
        pkgs.mkpasswd
      ];
      nix.settings.substituters = lib.mkForce [ ];
    };
    nodes.target = {
      virtualisation = {
        useBootLoader = true;
        useEFIBoot = true;
        useDefaultFilesystems = false;
        efi.keepVariables = false;
        # The installer's empty disk, now holding PandaOS.
        diskImage = "./empty0.qcow2";
        fileSystems."/" = {
          device = "/dev/disk/by-label/this-is-not-real-and-will-never-be-used";
          fsType = "ext4";
        };
      };
    };
    testScript = prelude + ''
      installer.start()
      installer.wait_for_unit("multi-user.target")
      pw_hash = installer.succeed("mkpasswd -m sha-512 -S pandatestsalt panda-test-pw").strip()

      with subtest("non-interactive install"):
          start = time.monotonic()
          out = installer.succeed(
              "panda-install --disk /dev/vdb --profile headless --hostname pandatest --user panda"
              f" --password-hash '{pw_hash}' --timezone Europe/Berlin --yes 2>&1"
          )
          install_sec = round(time.monotonic() - start, 1)
          print(out)
          assert "Done." in out, out
          emit({"test": "install", "installSec": install_sec})

      with subtest("disk layout and /etc/pandaos"):
          assert installer.succeed("blkid -s LABEL -o value /dev/vdb1").strip() == "PANDABOOT"
          assert installer.succeed("blkid -s LABEL -o value /dev/vdb2").strip() == "PANDAOS"
          installer.succeed("mkdir -p /tmp/top && mount -o subvol=/ /dev/vdb2 /tmp/top")
          subvols = installer.succeed("btrfs subvolume list /tmp/top")
          for sv in ["@", "@nix", "@home", "@mem", "@log", "@snapshots"]:
              assert re.search(rf"path {re.escape(sv)}$", subvols, re.M), f"missing subvolume {sv}: {subvols}"
          for f in ["flake.nix", "local.nix", "hardware-configuration.nix"]:
              installer.succeed(f"test -s /tmp/top/@/etc/pandaos/{f}")
          flake = installer.succeed("cat /tmp/top/@/etc/pandaos/flake.nix")
          assert 'profile = "headless"' in flake and "pandaos.lib.mkPandaSystem" in flake, flake
          local = installer.succeed("cat /tmp/top/@/etc/pandaos/local.nix")
          assert 'networking.hostName = "pandatest"' in local and pw_hash in local, local
          installer.succeed("test -L /tmp/top/@nix/var/nix/profiles/system")
          installer.succeed("umount /tmp/top")
          installer.succeed("mkdir -p /tmp/esp && mount /dev/vdb1 /tmp/esp && test -e /tmp/esp/loader/loader.conf && umount /tmp/esp")
          installer.shutdown()

      # Same machine, now booting the installed disk.
      target.state_dir = installer.state_dir
      target.start()
      target.wait_for_unit("multi-user.target")

      with subtest("installed system boots with the chosen settings"):
          assert target.succeed("hostname").strip() == "pandatest"
          assert target.succeed("getent shadow panda | cut -d: -f2").strip() == pw_hash
          assert "Europe/Berlin" in target.succeed("readlink /etc/localtime")
          for mnt in ["/", "/nix", "/home", "/mem", "/var/log", "/.snapshots"]:
              target.succeed(f"findmnt -no FSTYPE {mnt} | grep -qx btrfs")
          target.succeed("test -s /etc/pandaos/flake.nix")
          target.wait_for_unit("panda-agentd.service")
          target.wait_for_unit("panda-mem-init.service")

      with subtest("snapshots: btrbk on the installed btrfs"):
          target.succeed("systemctl start btrbk-panda.service")
          snaps = target.succeed("ls /.snapshots")
          assert "mem." in snaps and "home." in snaps, f"no btrbk snapshots: {snaps!r}"
          target.succeed("panda-rollback-mem 2>&1 | grep -q mem.")

      target.shutdown()
    '';
  };
}
