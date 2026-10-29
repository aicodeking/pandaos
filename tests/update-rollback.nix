# Unattended update: switch, health check, rollback, hold. Runs the real
# nixos-upgrade unit (its conditions, panda-update-run wrapper, panda-health
# and rollback) with ONE substitution: pandaos.updates.switchCommand switches
# to a prebuilt generation named in /var/lib/panda-test-next, the way
# nixos-rebuild does (set the system profile, then switch-to-configuration).
#
# Also: a shutdown in the middle of an update starts no rollback (the guard
# in panda-update-run), and the next boot is healthy.
#
# The real flake path (release branch, `--update-input pandaos`, the lock
# bump, nixos-rebuild) is tests/update-e2e.nix. Not covered anywhere:
# evaluating PandaOS's modules inside the VM, substituting from a cache, and
# the flake.lock restore after a failed update.
# The two generations are specialisations of the test node, so they are in
# the VM's store and Nix DB already.
{ pkgs, self, t }:
let
  inherit (pkgs) lib;
in
pkgs.testers.runNixOSTest {
  name = "update-rollback";
  nodes.machine =
    { config, pkgs, ... }:
    {
      imports = [
        self.nixosModules.default
        self.nixosModules.headless
        (t.nodeName "machine")
        t.noFirstboot
      ];
      # Off by default; the bad generation below breaks it.
      services.panda-agentd.enable = true;
      # nixos-upgrade's ConditionPathExists; the test switchCommand never reads it.
      environment.etc."pandaos/flake.nix".text = "# update-rollback test stub\n";
      # Replaces only `nixos-rebuild switch ...` inside panda-update-run; the
      # wrapper's health check and rollback still run around it.
      pandaos.updates.switchCommand =
        "next=$(cat /var/lib/panda-test-next) && echo \"test switchCommand: $next\""
        + " && nix-env -p /nix/var/nix/profiles/system --set \"$next\""
        + " && \"$next/bin/switch-to-configuration\" switch"
        # Shutdown-guard subtest: stay "in the middle of the update" after
        # activation, before the health check.
        + " && if [ -e /var/lib/panda-test-sleep ]; then sleep 600; fi";
      # Only the test starts updates.
      systemd.timers.nixos-upgrade.wantedBy = lib.mkForce [ ];
      # Register the whole closure (both specialisations included) in the VM's
      # Nix DB, so nix-env can point the system profile at them.
      virtualisation.additionalPaths = [ config.system.build.toplevel ];

      specialisation.good.configuration = {
        environment.etc."pandaos-test-gen".text = "good";
      };
      # Healthy, just a different generation, for the shutdown subtest.
      specialisation.slow.configuration = {
        environment.etc."pandaos-test-gen".text = "slow";
      };
      # Breaks agentd in a way switch-to-configuration itself may not notice
      # (Restart=always keeps the unit "activating"), so panda-health must.
      specialisation.bad.configuration = {
        environment.etc."pandaos-test-gen".text = "bad";
        systemd.services.panda-agentd.serviceConfig.ExecStart = lib.mkForce "${pkgs.coreutils}/bin/false";
      };
    };

  testScript =
    { nodes, ... }:
    let
      gen = name: nodes.machine.specialisation.${name}.configuration.system.build.toplevel;
    in
    t.prelude
    + ''
      good = "${gen "good"}"
      bad = "${gen "bad"}"
      slow = "${gen "slow"}"


      def current():
          return machine.succeed("readlink -f /run/current-system").strip()


      def update_to(target):
          machine.succeed(f"echo {target} > /var/lib/panda-test-next")
          return machine.execute("systemctl start nixos-upgrade.service 2>&1")


      def upgrade_log():
          return machine.succeed("journalctl -o cat -u nixos-upgrade.service")


      machine.wait_for_unit("multi-user.target")
      machine.wait_for_unit("panda-agentd.service")
      base = current()
      # Generation 1 = the booted system, so a rollback has somewhere to go.
      machine.succeed(f"nix-env -p /nix/var/nix/profiles/system --set {base}")

      with subtest("good update: switches and passes the health check"):
          start = time.monotonic()
          rc, out = update_to(good)
          assert rc == 0, f"good update failed: {out}\n{upgrade_log()}"
          assert current() == good, f"not switched: {current()} != {good}"
          assert machine.succeed("cat /etc/pandaos-test-gen").strip() == "good"
          assert "is healthy" in upgrade_log(), upgrade_log()
          emit({"test": "update-rollback", "goodUpdateSec": round(time.monotonic() - start, 1)})

      with subtest("bad update (agentd broken): health check rolls back and logs it"):
          rc, out = update_to(bad)
          log = upgrade_log()
          print(log)
          assert rc != 0, "an update that broke agentd reported success"
          assert current() == good, f"not rolled back: {current()} != {good}"
          assert "panda-agentd.service is" in log and "rolling back to" in log and "rolled back to" in log, log
          assert machine.succeed("cat /etc/pandaos-test-gen").strip() == "good"
          gens = machine.succeed("nix-env -p /nix/var/nix/profiles/system --list-generations")
          assert machine.succeed("readlink -f /nix/var/nix/profiles/system").strip() == good, gens
          machine.wait_for_unit("panda-agentd.service")

      with subtest("update-hold: the update is skipped"):
          machine.succeed("panda-update hold")
          machine.succeed("test -e /var/lib/pandaos/update-hold")
          assert "on hold" in machine.succeed("panda-update status")
          machine.fail("panda-update run")
          rc, out = update_to(bad)
          assert rc == 0, f"a held update should be skipped, not fail: {out}"
          cond = machine.succeed("systemctl show -p ConditionResult --value nixos-upgrade.service").strip()
          assert cond == "no", f"nixos-upgrade ran while on hold: ConditionResult={cond}"
          assert current() == good, f"held update still switched: {current()}"
          machine.succeed("panda-update unhold")
          machine.fail("test -e /var/lib/pandaos/update-hold")

      with subtest("shutdown during an update: no rollback is started, the next boot is healthy"):
          since = machine.succeed("date +%s").strip()
          machine.succeed("touch /var/lib/panda-test-sleep")
          machine.succeed(f"echo {slow} > /var/lib/panda-test-next")
          machine.succeed("systemctl start --no-block nixos-upgrade.service")
          machine.wait_until_succeeds(f'test "$(readlink -f /run/current-system)" = {slow}', timeout=180)
          machine.wait_until_succeeds("pgrep -fx 'sleep 600'", timeout=60)
          machine.shutdown()  # systemd TERMs nixos-upgrade mid-update
          machine.start()
          machine.wait_for_unit("multi-user.target")
          machine.succeed("rm /var/lib/panda-test-sleep")
          log = machine.succeed(f"journalctl -o cat --since @{since} -u nixos-upgrade.service")
          print(log)
          assert "interrupted by shutdown" in log, log
          assert "rolling back to" not in log and "rolled back to" not in log, log
          machine.wait_for_unit("panda-agentd.service")
          machine.succeed("panda-health --wait 120")
    '';
}
