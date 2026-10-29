# Boot assessment (nixos/modules/boot.nix) on real UEFI + systemd-boot:
#   boot 1: the installed generation boots healthy -> panda-boot-check makes
#           it the known-good entry (EFI var LoaderEntryDefault).
#   then:   a bad generation is installed with `switch-to-configuration boot`
#           -> extraInstallCommands sets it as the one-shot entry.
#   boot 2: (driver reboot) the bad generation is tried; panda-mem never comes
#           up, panda-boot-check fails and schedules a reboot by itself.
#   boot 3: (guest-initiated reboot) the firmware falls back to the known-good
#           entry; LoaderEntryDefault is still the good one.
# Not covered: a kernel/initrd that panics (panic=10 + boot.panic_on_fail take
# the same fallback path, but a panicking generation is slow and flaky in CI).
{ pkgs, self, t }:
let
  inherit (pkgs) lib;
  loaderGuid = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f";
in
pkgs.testers.runNixOSTest {
  name = "boot-fallback";
  nodes.machine =
    { config, ... }:
    {
      imports = [
        self.nixosModules.default
        self.nixosModules.headless
        (t.nodeName "machine")
        t.noFirstboot
      ];
      virtualisation = {
        useBootLoader = true;
        useEFIBoot = true;
      };
      boot.loader.systemd-boot.enable = true;
      boot.loader.efi.canTouchEfiVariables = true;
      # Register the closure (the bad specialisation included) in the VM's Nix
      # DB so nix-env can point the system profile at it.
      virtualisation.additionalPaths = [ config.system.build.toplevel ];

      environment.etc."pandaos-test-gen".text = lib.mkDefault "good";
      # Installed below as generation 2. Boots fine but fails panda-health.
      specialisation.bad.configuration = {
        environment.etc."pandaos-test-gen".text = "bad";
        systemd.services.panda-mem.serviceConfig.ExecStart = lib.mkForce "${pkgs.coreutils}/bin/false";
      };
    };

  testScript =
    { nodes, ... }:
    let
      bad = nodes.machine.specialisation.bad.configuration.system.build.toplevel;
    in
    t.prelude
    + ''
      def efivar(name):
          """systemd-boot EFI variable as a string, "" if unset."""
          out = machine.succeed(
              f"f=/sys/firmware/efi/efivars/{name}-${loaderGuid}; "
              "if [ -e $f ]; then tail -c +5 $f | tr -d '\\000'; fi"
          )
          return out.strip()


      def gen():
          return machine.succeed("cat /etc/pandaos-test-gen").strip()


      machine.start(allow_reboot=True)

      with subtest("boot 1: healthy boot becomes the known-good entry"):
          machine.wait_for_unit("multi-user.target")
          machine.wait_for_unit("panda-boot-check.service", timeout=300)
          good_entry = efivar("LoaderEntrySelected")
          assert good_entry.startswith("nixos-generation-"), f"LoaderEntrySelected={good_entry!r}"
          assert efivar("LoaderEntryDefault") == good_entry, efivar("LoaderEntryDefault")
          assert gen() == "good"

      with subtest("install a bad generation: it becomes a one-shot trial"):
          machine.succeed("nix-env -p /nix/var/nix/profiles/system --set ${bad}")
          out = machine.succeed("${bad}/bin/switch-to-configuration boot 2>&1")
          print(out)
          trial = machine.succeed("sed -n 's/^default //p' /boot/loader/loader.conf").strip()
          assert trial != good_entry, f"loader.conf default did not move: {trial}"
          assert efivar("LoaderEntryOneShot") == trial, f"one-shot={efivar('LoaderEntryOneShot')!r}, want {trial}"
          assert efivar("LoaderEntryDefault") == good_entry, "install must not move the known-good default"

      with subtest("boot 2: the trial boots, fails the health check, schedules a reboot"):
          machine.reboot()
          assert efivar("LoaderEntrySelected") == trial, efivar("LoaderEntrySelected")
          assert gen() == "bad"
          machine.wait_until_succeeds(
              "journalctl -b -o cat -u panda-boot-check.service | grep -q 'is unhealthy, rebooting into'",
              timeout=400,
          )
          print(machine.succeed("journalctl -b -o cat -u panda-boot-check.service"))
          machine.succeed("test -e /run/systemd/shutdown/scheduled")
          assert efivar("LoaderEntryDefault") == good_entry, "a failed trial must not become the default"

      with subtest("boot 3: the guest rebooted itself into the known-good entry"):
          # Like machine.reboot(), but the guest's own `shutdown -r +1` does it.
          machine.connected = False
          machine.wait_for_unit("multi-user.target")
          assert efivar("LoaderEntrySelected") == good_entry, efivar("LoaderEntrySelected")
          assert efivar("LoaderEntryDefault") == good_entry, efivar("LoaderEntryDefault")
          assert efivar("LoaderEntryOneShot") == "", "one-shot entry not consumed"
          assert gen() == "good"
          machine.wait_for_unit("panda-boot-check.service", timeout=300)
    '';
}
