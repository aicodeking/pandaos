# PandaOS VM tests, exposed as `checks.x86_64-linux.*` by the flake.
#
# Metrics: each test prints `METRIC {json}` in the build log (survives a failed
# run) and writes the same object to `$out/metrics.json` (what you get from
# `nix build .#checks.x86_64-linux.<name>`). bench/collect.py reads either.
{ pkgs, self }:
let
  inherit (pkgs) lib;
  budgets = lib.importJSON ./budgets.json;
  t = import ./lib.nix { inherit lib; };
  inherit (t)
    nodeName
    vlanNode
    noFirstboot
    prelude
    ;

  # Boot tests measure idle RAM only; closure sizes come from the real
  # installable systems in closures.nix, not from these instrumented VMs.
  mkBootTest =
    {
      name,
      profile,
      script,
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
        prelude
        + ''
          ram_budget = ${toString budgets.${profile}.idleRamMB}
          machine.wait_for_unit("multi-user.target")
          ${script}
        '';
    };

  # Unpaired box, relay unreachable (the default relay is a placeholder).
  agentdUnpairedChecks = ''
    with subtest("agentd: pairing code 0640 root:wheel, not in status.json; lastError while the relay is down"):
        machine.wait_for_file("/run/panda-agentd/pairing-code")
        perms = machine.succeed("stat -c '%a %U %G' /run/panda-agentd/pairing-code").strip()
        assert perms == "640 root wheel", f"pairing-code is {perms}"
        machine.fail("runuser -u nobody -- cat /run/panda-agentd/pairing-code")
        wait_last_error(machine, present=True)
        st = status_json(machine)
        code = machine.succeed("cat /run/panda-agentd/pairing-code").strip()
        raw = machine.succeed("cat /run/panda-agentd/status.json")
        assert "pairingCode" not in st and code not in raw, f"status.json leaks the code: {raw}"
        assert st["pairing"] is True and st["paired"] is False, st
        assert st.get("lastErrorAt"), f"lastError without lastErrorAt: {st}"
        print(f"lastError while relay is down: {st['lastError']}")
  '';

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

  # The relay goes away: lastError is set; it comes back: lastError clears.
  # fake-relay keeps tokens in memory, so the restart also forces a re-pair.
  outageCheck = ''
    with subtest("relay outage sets lastError, recovery clears it"):
        relay.succeed("systemctl stop fake-relay.service")
        wait_last_error(box, present=True)
        print(f"lastError during outage: {status_json(box)['lastError']}")
        relay.succeed("systemctl start fake-relay.service")
        wait_last_error(box, present=False, timeout=180)
        box.wait_until_succeeds("grep -q '\"paired\": true' /run/panda-agentd/status.json", timeout=60)
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
          boot.kernelModules = lib.optional slow "sch_netem";
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
          # The default 1 GiB test disk has ~890 MiB free; keep the guard on, just lower.
          minFreeMb = 128;
        };
        # Slow network: hold agentd back until netem is in place, so pairing
        # itself runs over the bad link.
        systemd.services.panda-agentd.wantedBy = lib.mkIf slow (lib.mkForce [ ]);
        boot.kernelModules = lib.optional slow "sch_netem";
      };
      testScript =
        let
          # netem only shapes egress, so each side delays its half: 150 ms out
          # of each node makes a 300 ms RTT, and 1% loss each way ~ 2% per round trip.
          startAgent = lib.optionalString slow ''
            for node in (box, relay):
                node.succeed("tc qdisc add dev eth1 root netem delay 150ms loss 1% rate 1mbit")
            ping = box.succeed("ping -c 10 -i 0.3 -W 2 relay")
            rtt_match = re.search(r"= [0-9.]+/([0-9.]+)/", ping)
            assert rtt_match, f"no rtt summary in ping output: {ping}"
            rtt_ms = float(rtt_match.group(1))
            print(f"netem RTT box<->relay: {rtt_ms} ms")
            assert 290 <= rtt_ms <= 450, f"netem RTT {rtt_ms} ms, want ~300 ms"
            start = time.monotonic()
            box.succeed("systemctl start panda-agentd.service")
          '';
          rttMetric = lib.optionalString slow ", \"rttMs\": rtt_ms";
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
          emit({"test": "${name}", "roundtripSec": elapsed${rttMetric}})
          assert result["exitCode"] == 0, f"job failed: {result}"
          assert result["stdout"].strip() == "hi", f"unexpected stdout: {result}"
          ${limit}
          with subtest("agentd status after pairing: paired, no code anywhere, no error"):
              box.fail("test -e /run/panda-agentd/pairing-code")
              wait_last_error(box, present=False)
              status = status_json(box)
              assert status["paired"] is True and status["pairing"] is False, f"status.json: {status}"
              assert "pairingCode" not in status, f"status.json has a pairing code: {status}"
          ${lib.optionalString (!slow) (cancelCheck + outageCheck)}
        '';
    };

  # Desktop shell scripts, run against fixture status files.
  desktopChecks = ''
    machine.wait_for_unit("greetd.service")
    with subtest("desktop: panda-menu and panda-agents-status"):
        machine.succeed("test -x \"$(command -v panda-menu)\"")


        def agents_status(fixture):
            machine.succeed(f"mkdir -p /tmp/fixture && printf %s {shlex.quote(fixture)} > /tmp/fixture/status.json")
            return json.loads(machine.succeed("PANDA_AGENTD_RUN_DIR=/tmp/fixture panda-agents-status"))


        busy = agents_status(json.dumps({"paired": True, "slots": 2, "running": 1, "completed": 3,
                                         "failed": 0, "relay": "https://relay.example", "version": "0.2.0"}))
        assert busy["text"] == "Agents 1/2" and busy["class"] == "busy", busy
        assert "completed: 3" in busy["tooltip"], busy
        failed = agents_status(json.dumps({"paired": True, "slots": 2, "running": 0, "failed": 1,
                                           "lastError": "relay unreachable"}))
        assert failed["class"] == "failed", failed
        # status.json never carries the code (it is in a 0640 file), only `pairing`.
        pairing = agents_status(json.dumps({"paired": False, "pairing": True, "slots": 2}))
        assert pairing["text"].startswith("Pairing") and pairing["class"] == "pairing", pairing
        garbage = agents_status("not json")
        assert garbage["text"] == "Agents: ?", garbage
        off = json.loads(machine.succeed("PANDA_AGENTD_RUN_DIR=/nonexistent panda-agents-status"))
        assert off["class"] == "off", off
        # agentd is off by default: no status file, so the module says "off".
        real = json.loads(machine.succeed("panda-agents-status"))
        assert real["class"] == "off", real
        bars = json.loads(machine.succeed("cat /etc/xdg/waybar/config"))
        assert [b["name"] for b in bars] == ["top", "dock"], bars
        machine.succeed("grep -q swaybg /etc/xdg/labwc/autostart")
    with subtest("desktop: panda-connect-status (fixtures, then the real unsigned box)"):
        machine.succeed("sh ${../components/desktop/test-connect-status.sh} ${../components/desktop/bin/panda-connect-status}")
        real = json.loads(machine.succeed("panda-connect-status"))
        assert real["text"] == "PandaCode: sign in" and real["class"] == "off", real
        top = json.loads(machine.succeed("cat /etc/xdg/waybar/config"))[0]
        assert "custom/connect" in top["modules-left"], top
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
  # agentd is off by default; this test enables it to cover the unpaired
  # daemon, so its idle RAM includes agentd (an upper bound for the default).
  boot-headless = mkBootTest {
    name = "boot-headless";
    profile = "headless";
    extraConfig.services.panda-agentd.enable = true;
    script = ''
      machine.wait_for_unit("panda-agentd.service")
      machine.wait_for_unit("panda-mem.service")
      ${agentdUnpairedChecks}
      ram, ram_minus_avail = idle_ram_mb(machine)
      emit({"test": "boot-headless", "profile": "headless", "idleRamMB": ram,
            "memTotalMinusAvailableMB": ram_minus_avail})
      assert ram <= ram_budget, f"idle RAM {ram} MB > budget {ram_budget} MB"
    '';
  };

  # idleRamMB (budgeted) is measured inside a logged-in labwc session, which
  # greetd starts via initial_session (autologin, test only). Logging out then
  # brings up tuigreet, measured as greeterIdleRamMB (reported only).
  boot-desktop = mkBootTest {
    name = "boot-desktop";
    profile = "desktop";
    extraConfig = {
      virtualisation.memorySize = 2048;
      # A virtio GPU for wlroots' DRM backend, rendered in software (pixman).
      virtualisation.qemu.options = [
        "-vga none"
        "-device virtio-gpu-pci"
      ];
      environment.sessionVariables.WLR_RENDERER = lib.mkDefault "pixman";
      # test-connect-status.sh runs the module with plain sh and needs jq.
      environment.systemPackages = [ pkgs.jq ];
      services.greetd.settings.initial_session = {
        command = "labwc";
        user = "panda";
      };
    };
    script = ''
      ${desktopChecks}
      with subtest("logged-in labwc session comes up and stays up"):
          machine.wait_until_succeeds("pgrep -u panda -x labwc", timeout=120)
          machine.wait_until_succeeds("test -S /run/user/$(id -u panda)/wayland-0", timeout=60)
          # Not -x: the nixpkgs wrapper names the process .waybar-wrapped.
          rc, _ = machine.execute("timeout 60 sh -c 'until pgrep -u panda waybar; do sleep 2; done'")
          if rc != 0:
              print(machine.execute("journalctl -b --no-pager -n 80 _UID=$(id -u panda)")[1])
              raise AssertionError("waybar did not start in the labwc session")
      session_ram, session_minus_avail = idle_ram_mb(machine)
      machine.screenshot("session")
      machine.succeed("pgrep -u panda -x labwc")

      with subtest("logout returns to the greeter"):
          machine.succeed("loginctl terminate-user panda")
          machine.wait_until_fails("pgrep -u panda -x labwc", timeout=60)
          machine.wait_until_succeeds("pgrep -x tuigreet", timeout=60)
      greeter_ram, greeter_minus_avail = idle_ram_mb(machine)

      emit({"test": "boot-desktop", "profile": "desktop", "idleRamMB": session_ram,
            "memTotalMinusAvailableMB": session_minus_avail, "greeterIdleRamMB": greeter_ram,
            "greeterMemTotalMinusAvailableMB": greeter_minus_avail})
      assert session_ram <= ram_budget, f"idle session RAM {session_ram} MB > budget {ram_budget} MB"
    '';
  };

  agentd-roundtrip = mkRoundtrip {
    name = "agentd-roundtrip";
    slow = false;
  };

  slow-network = mkRoundtrip {
    name = "slow-network";
    slow = true;
  };

  closures = import ./closures.nix { inherit pkgs self; };
  mem-security = import ./mem-security.nix { inherit pkgs self t; };
  update-rollback = import ./update-rollback.nix { inherit pkgs self t; };
  iso-boot = import ./iso-boot.nix { inherit pkgs self t; };
  pandacode-host = import ./pandacode-host.nix { inherit pkgs self t; };

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
        machine.wait_for_unit("panda-mem.service")
        sock = "-socket /run/panda-mem/sock"
        machine.wait_for_file("/run/panda-mem/sock")
        # As a client user, through the single-writer socket.
        machine.succeed(f"runuser -u panda -- panda-mem write {sock} -kind semantic -title 'ci probe' -body 'pandaos-ci-marker body'")
        hits = machine.succeed(f"runuser -u panda -- panda-mem search {sock} pandaos-ci-marker")
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
        pkgs.btrfs-progs # inspecting the result; panda-install brings its own
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
          target.wait_for_unit("panda-mem.service")
          # Not signed in: the PandaCode host is installed but idle, agentd is off.
          st = json.loads(target.succeed("panda-connect status --json"))
          assert st["signedIn"] is False, st
          target.fail("systemctl is-active panda-agentd.service")

      with subtest("snapshots: btrbk on the installed btrfs"):
          target.succeed("systemctl start btrbk-panda.service")
          snaps = target.succeed("ls /.snapshots")
          assert "mem." in snaps and "home." in snaps, f"no btrbk snapshots: {snaps!r}"
          snap = sorted(s for s in snaps.split() if s.startswith("mem."))[-1]
          rc, usage = target.execute("panda-rollback-mem 2>&1")
          assert rc == 2 and snap in usage, f"panda-rollback-mem usage: rc={rc} {usage!r}"

      with subtest("panda-rollback-mem restores /mem from a snapshot"):
          target.succeed("touch /mem/written-after-snapshot")
          target.succeed(f"panda-rollback-mem {snap}")
          target.fail("test -e /mem/written-after-snapshot")
          target.succeed("test -d /mem/.git")
          assert "mem-pre-rollback." in target.succeed("ls /.snapshots")

      target.shutdown()
    '';
  };
}
