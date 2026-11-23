# T3 Code Connect host (services.t3code-host, t3-connect) on a box that is
# not signed in.
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
          # A Node SEA binary is corrupted by patchelf/strip; --version proves it is intact.
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
          html = machine.succeed(f"curl -fsS http://127.0.0.1:{port}/")
          assert "<html" in html.lower() or "<!doctype html" in html.lower(), html[:200]
          env = json.loads(machine.succeed(f"curl -fsS http://127.0.0.1:{port}/.well-known/t3/environment"))
          print(env)

          out = machine.succeed("t3-connect pair --ttl 5m --label ci-test 2>/dev/null")
          print(out)
          assert re.search(r"https?://\S+|[Tt]oken", out), f"no pairing URL/token on stdout: {out}"

          journal = machine.succeed(f"journalctl -o cat -u {unit}")
          for pat in (r"Token: [A-Za-z0-9]", r"token=[A-Za-z0-9]"):
              m = re.search(pat, journal)
              assert m is None, f"unredacted secret in the journal: {m.group(0)}..."

          machine.succeed("panda-health --settle 10")

          machine.succeed("t3-connect stop")
          machine.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
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
    '';
}
