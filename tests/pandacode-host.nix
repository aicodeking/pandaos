# PandaCode Connect host on a box that is not signed in.
#
# Not covered (needs GitHub and the PandaCode Connect relay, neither of which
# CI can reach or own): `panda-connect sign-in` itself (the GitHub device flow,
# approving it in PandaCode), a signed-in host serving, and real tasks started
# from another device. Those stay manual; see docs/troubleshooting.md.
{ pkgs, self, t }:
pkgs.testers.runNixOSTest {
  name = "pandacode-host";
  nodes.machine = {
    imports = [
      self.nixosModules.default
      self.nixosModules.headless
      (t.nodeName "machine")
      t.noFirstboot
    ];
    # PandaCode starts GTK/WebKit even for one-shot flags.
    virtualisation.memorySize = 2048;
  };
  testScript =
    { nodes, ... }:
    let
      host = nodes.machine.services.pandacode-host;
      pandacode = pkgs.lib.getExe host.package;
      xvfbRun = pkgs.lib.getExe pkgs.xvfb-run;
    in
    t.prelude
    + ''
      marker = "${host.signedInMarker}"


      def status():
          return json.loads(machine.succeed("panda-connect status --json"))


      def restarts():
          return int(machine.succeed("systemctl show -p NRestarts --value pandacode-host.service").strip())


      machine.wait_for_unit("multi-user.target")

      with subtest("the AppImage runs: pandacode serve --help under xvfb-run, as the host user"):
          rc, out = machine.execute(
              "runuser -u ${host.user} -- env HOME=${nodes.machine.users.users.${host.user}.home} ${xvfbRun} -a ${pandacode} serve --help 2>&1"
          )
          print(out)
          # serve prints its usage and exits 2 for --help.
          assert rc == 2 and "Usage: pandacode serve" in out, f"rc={rc}"

      with subtest("not signed in: status, no restart storm in 60 s, healthy"):
          st = status()
          assert st["signedIn"] is False and st["service"] == "inactive", st
          assert st["version"] == "${pkgs.lib.getVersion host.package}", st
          machine.fail(f"test -e {marker}")
          # Starting it by hand is a no-op too: ConditionPathExists skips it.
          machine.succeed("systemctl start pandacode-host.service")
          time.sleep(60)
          state = [machine.succeed(f"systemctl show -p {p} --value pandacode-host.service").strip()
                   for p in ("ActiveState", "ConditionResult")]
          assert state == ["inactive", "no"], state
          assert restarts() == 0, f"NRestarts={restarts()}"
          machine.succeed("panda-health --settle 10")

      with subtest("full access on/off"):
          machine.succeed("panda-connect full-access on")
          assert machine.succeed("cat /var/lib/pandacode-host/full-access").strip() == "on"
          machine.succeed("panda-connect full-access off")
          assert machine.succeed("cat /var/lib/pandacode-host/full-access").strip() == "off"
          # Still signed out, so nothing was started.
          assert status()["service"] == "inactive"

      # A sign-in lost outside panda-connect (marker left behind, no PandaCode
      # sign-in): `serve` exits non-zero; the unit drops the marker instead of
      # restarting forever, so panda-health (and updates) stay healthy.
      with subtest("stale marker: the unit forgets it instead of crash-looping"):
          machine.succeed(f"echo machine > {marker}")
          machine.succeed("systemctl start pandacode-host.service || true")
          machine.wait_until_fails(f"test -e {marker}", timeout=180)
          print(machine.succeed("journalctl -o cat -u pandacode-host.service"))
          time.sleep(30)
          n = restarts()
          assert n <= 2, f"NRestarts={n}"
          machine.fail("systemctl is-active pandacode-host.service")
          # The full-access ExecStartPre cleans up after itself.
          machine.fail("journalctl -u pandacode-host.service | grep -q 'left-over process'")
          assert status()["signedIn"] is False
          machine.succeed("panda-health --settle 10")
    '';
}
