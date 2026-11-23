# T3 Code Connect host (services.t3code-host, t3-connect) on a box that is
# not signed in. `machine` runs it beside pandacode-host; `t3only` runs it with
# pandacode-host disabled, so services.t3code-host creates the `agent` user and
# its home itself (ownAgent) and that whole evaluation + runtime path is booted.
#
# Not covered (needs the T3 Code Connect cloud and a Cloudflare tunnel, which
# CI can neither reach nor own): a completed `t3-connect sign-in`, `connect` or
# `publish-only` mode serving through cloudflared, approving a real device and
# unlinking it. Here only the headless sign-in failing cleanly offline, and the
# loopback-only `local` mode, which needs no network.
{ pkgs, self, t }:
pkgs.testers.runNixOSTest {
  name = "t3code-host";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "machine")
      t.noFirstboot
    ];
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ pkgs.curl ];
  };
  nodes.t3only = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "t3only")
      t.noFirstboot
    ];
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ pkgs.curl ];
    services.pandacode-host.enable = pkgs.lib.mkForce false;
  };
  testScript =
    { nodes, ... }:
    let
      host = nodes.machine.services.t3code-host;
      home = nodes.machine.users.users.${host.user}.home;
    in
    t.prelude
    + ''
      marker = "${host.enabledMarker}"
      port = ${toString host.port}
      unit = "t3code-host.service"


      def status():
          # Must work without root: run it as the host user.
          return json.loads(machine.succeed("runuser -u ${host.user} -- t3-connect status --json"))


      def show(prop, u=unit):
          return machine.succeed(f"systemctl show -p {prop} --value {u}").strip()


      def assert_off():
          st = status()
          assert st["enabled"] is False and st["service"] == "inactive", st
          machine.fail(f"test -e {marker}")


      machine.wait_for_unit("multi-user.target")

      with subtest("real t3 and cloudflared binaries execute as ${host.user}"):
          # package.nix sets dontStrip, which keeps the Node SEA blob appended to
          # the binary, while autoPatchelfHook patches its dynamic deps; --version
          # proves the binary survived packaging.
          out = machine.succeed("runuser -u ${host.user} -- env HOME=${home} ${host.package}/bin/t3 --version 2>&1")
          print(out)
          out = machine.succeed("runuser -u ${host.user} -- env HOME=${home} ${host.cloudflaredPackage}/bin/cloudflared --version 2>&1")
          print(out)
          assert "2026.10.4" in out, out

      with subtest("disabled by default: status, skipped start, no restarts, healthy"):
          st = status()
          assert st["enabled"] is False and st["signedIn"] is False, st
          assert st["service"] == "inactive", st
          assert st["host"] == "${host.host}" == "127.0.0.1" and st["port"] == port == 3773, st
          machine.fail(f"test -e {marker}")
          # Starting it by hand is a no-op: the unit's condition skips it.
          machine.succeed(f"systemctl start {unit}")
          time.sleep(10)
          assert show("ConditionResult") == "no", show("ConditionResult")
          assert show("ActiveState") == "inactive", show("ActiveState")
          assert int(show("NRestarts")) == 0, show("NRestarts")
          machine.succeed("panda-health --settle 10")

      with subtest("t3-connect start: real t3 serve, loopback only, HTTP, redacted, pairs, in agents.slice"):
          machine.succeed("t3-connect start")
          machine.wait_for_unit(unit)
          machine.succeed(f"test -e {marker}")
          st = status()
          assert st["enabled"] is True and st["mode"] == "local" and st["service"] == "active", st

          assert show("Slice") == "agents.slice", show("Slice")
          assert show("Delegate") == "yes", show("Delegate")
          cg = show("ControlGroup")
          machine.succeed(f"test -d /sys/fs/cgroup{cg}/host")

          machine.wait_for_open_port(port, addr="127.0.0.1", timeout=120)
          listen = machine.succeed(f"ss -ltnH 'sport = :{port}'").strip()
          print(listen)
          assert listen, "nothing listens on the T3 port"
          for line in listen.splitlines():
              local = line.split()[3]
              assert local == f"127.0.0.1:{port}", f"non-loopback listener: {line}"
          # t3 serve binds the TCP socket before migrations finish and answers 503
          # (rejectRequestsUntilServing) until HTTP serving activates.
          machine.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=60)
          html = machine.succeed(f"curl -fsS http://127.0.0.1:{port}/")
          assert "<html" in html.lower() or "<!doctype html" in html.lower(), html[:200]
          env = json.loads(machine.succeed(f"curl -fsS http://127.0.0.1:{port}/.well-known/t3/environment"))
          print(env)
          # Only now: during startup/migrations the version probe may get a 503.
          st = status()
          assert st.get("runningVersion") == "${host.package.version}" == "0.1.11", st

          out = machine.succeed("t3-connect pair --ttl 5m --label ci-test 2>/dev/null")
          print(out)
          assert re.search(r"https?://\S+|[Tt]oken", out), f"no pairing URL/token on stdout: {out}"

          journal = machine.succeed(f"journalctl -o cat -u {unit}")
          # t3 serve prints its pairing token at startup: redaction must have
          # fired on it, so the negative checks below cannot pass vacuously.
          assert "[redacted]" in journal, journal
          for pat in (r"Token: [A-Za-z0-9]", r"token=[A-Za-z0-9]"):
              m = re.search(pat, journal)
              assert m is None, f"unredacted secret in the journal: {m.group(0)}..."

          machine.succeed("panda-health --settle 10")

          # A failed sign-in while local runs: the sign-in unit stops the host
          # (Conflicts=), fails offline, and its ExecStopPost brings the host
          # back in local mode.
          rc, out = machine.execute(
              "timeout 300 systemd-run --quiet --pipe --wait --collect t3-connect sign-in 2>&1"
          )
          print(out)
          assert rc != 0 and rc != 124, f"rc={rc}: {out}"
          machine.wait_for_unit(unit)
          machine.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=120)
          st = status()
          assert st["enabled"] is True and st["mode"] == "local" and st["service"] == "active", st
          machine.succeed(f"test -e {marker}")

          # stop cancels a pending sign-in too, and nothing restarts the host after.
          machine.succeed("timeout 30 t3-connect sign-in --no-wait 2>&1")
          machine.succeed("t3-connect stop")
          machine.wait_until_fails("systemctl is-active t3code-sign-in.service", timeout=60)
          machine.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
          time.sleep(5)
          assert show("ActiveState", "t3code-sign-in.service") in ("inactive", "failed")
          assert show("ActiveState") in ("inactive", "failed"), show("ActiveState")
          assert_off()

      # The sign-in runs as t3code-sign-in.service with no terminal: here with
      # no network, so it must fail cleanly instead of waiting on a prompt
      # (e.g. cloudflared's interactive install confirmation).
      with subtest("headless t3-connect sign-in offline: clean failure, no prompt, no marker"):
          rc, out = machine.execute(
              "timeout 300 systemd-run --quiet --pipe --wait --collect t3-connect sign-in 2>&1"
          )
          print(out)
          assert rc != 0 and rc != 124, f"rc={rc}: {out}"
          journal = machine.succeed("journalctl -o cat -u t3code-sign-in.service")
          print(journal)
          assert "Download and install version" not in journal, journal
          assert "Download and install version" not in out, out
          assert_off()
          machine.succeed("panda-health --settle 10")

          # --no-wait: returns at once, the unit goes on alone.
          out = machine.succeed("timeout 30 t3-connect sign-in --no-wait 2>&1")
          assert "journalctl -u t3code-sign-in" in out, out
          machine.wait_until_fails("systemctl is-active t3code-sign-in.service", timeout=300)
          assert show("ActiveState", "t3code-sign-in.service") in ("inactive", "failed")
          assert_off()

      # T3-only box: services.pandacode-host disabled, so services.t3code-host
      # owns the `agent` user, its home and panda-mem membership.
      t3only.wait_for_unit("multi-user.target")

      with subtest("t3only (pandacode-host disabled): agent user, home, panda-mem, local host in agents.slice"):
          t3only.succeed("id ${host.user}")
          perm = t3only.succeed("stat -c '%a %U:%G' ${home}").strip()
          assert perm == "700 ${host.user}:${host.user}", perm
          groups = t3only.succeed("id -nG ${host.user}").split()
          assert "panda-mem" in groups, groups

          t3only.succeed("t3-connect start")
          t3only.wait_for_unit(unit)
          t3only.succeed(f"test -e {marker}")
          assert t3only.succeed(f"systemctl show -p Slice --value {unit}").strip() == "agents.slice"
          assert t3only.succeed(f"systemctl show -p Delegate --value {unit}").strip() == "yes"
          cg = t3only.succeed(f"systemctl show -p ControlGroup --value {unit}").strip()
          t3only.succeed(f"test -d /sys/fs/cgroup{cg}/host")
          t3only.wait_for_open_port(port, addr="127.0.0.1", timeout=120)
          t3only.wait_until_succeeds(f"curl -fsS -o /dev/null http://127.0.0.1:{port}/", timeout=60)
          code = t3only.succeed(f"curl -sS -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{port}/").strip()
          assert code == "200", code
          t3only.succeed("panda-health --settle 10")

          t3only.succeed("t3-connect stop")
          t3only.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
          assert t3only.succeed(f"systemctl show -p ActiveState --value {unit}").strip() == "inactive"
          t3only.fail(f"test -e {marker}")
    '';
}
