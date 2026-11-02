# Real boatd, boat-api, jailer and Firecracker, started by
# components/boat/module.nix on NixOS, through a real generation switch.
#
# Firecracker runs three virtualisation levels down here (runner, this test VM,
# the guest). It starts, but the guest agent never answered, not even in 25 min
# (runs 41325437352, 41328694293), so nothing here may depend on the guest. This
# checks the host side: the jail, cgroup and OOM settings on the real process,
# and that a `switch` to a new boatd and boat-api leaves that process and boatd
# alone. That the guest itself survives a boatd stop and a new binary is checked
# by gateway/e2e-edge.sh on a runner, where guests boot, but not under NixOS.
#
# It needs /dev/kvm in the test VM and the guest image, built with docker outside
# Nix, so it is not in `nix flake check`; boat-e2e runs
#   nix build --impure -f tests/standalone.nix boat-real --argstr guestDir DIR
# with vmlinux and rootfs-slim.ext4 in DIR. Without nested KVM it fails, not skips.
{
  pkgs,
  self,
  t,
  guest,
}:
let
  # A different store path for the next generation, as a real update would have.
  # The linker ignores -X for a symbol that does not exist.
  next = pkg: pkg.overrideAttrs (o: { ldflags = (o.ldflags or [ ]) ++ [ "-X main.nextGeneration=1" ]; });
  boatd = pkgs.callPackage ../components/boat/boatd/package.nix { };
  api = pkgs.callPackage ../components/boat/api/package.nix { };
in
pkgs.testers.runNixOSTest {
  name = "boat-real";
  nodes.machine =
    { lib, ... }:
    {
      imports = [
        self.nixosModules.default
        self.nixosModules.server
      ];
      pandaos.firstboot.enable = false;
      networking.hostName = lib.mkForce "machine";
      virtualisation = {
        memorySize = 5120;
        cores = 4;
        diskSize = 8192;
        emptyDiskImages = [ 16384 ];
        fileSystems."/var/lib/boat" = {
          device = "/dev/vdb";
          fsType = "btrfs";
          autoFormat = true;
        };
      };
      environment.variables.BOAT_CONFIG = "/root/boat-cli.json";
      environment.systemPackages = [ pkgs.jq ];
      services.boat = {
        boatdPackage = boatd;
        apiPackage = api;
        sizes.t = {
          vcpu = 1;
          mem_mib = 1024;
          disk_gib = 6;
        };
        defaultSize = "t";
        # The guest does not come up at this nesting depth (see above): don't wait.
        boatdSettings.bootTimeoutS = 60;
      };
      specialisation.next.configuration.services.boat = {
        boatdPackage = lib.mkForce (next boatd);
        apiPackage = lib.mkForce (next api);
        quotas.maxVMs = 11;
      };
    };

  testScript =
    { nodes, ... }:
    let
      health = "${nodes.machine.system.build.panda-health}/bin/panda-health";
    in
    t.prelude
    + ''
      def fc_pids():
          return machine.succeed("pgrep -x firecracker || true").split()


      machine.wait_for_unit("multi-user.target")

      with subtest("real boatd on the module: KVM, images, first key"):
          machine.succeed("test -c /dev/kvm")
          machine.wait_for_unit("boatd.service")
          machine.wait_for_unit("boat-api.service")
          machine.wait_for_file("/run/boat/boatd.sock")
          machine.succeed(
              "mkdir -p /var/lib/boat/images/ubuntu-24.04",
              "cp --sparse=always ${guest}/vmlinux /var/lib/boat/images/ubuntu-24.04/vmlinux",
              "cp --sparse=always ${guest}/rootfs-slim.ext4 /var/lib/boat/images/ubuntu-24.04/rootfs.ext4",
              # As boat-images leaves them; files from the Nix store are read-only.
              "chmod 0644 /var/lib/boat/images/ubuntu-24.04/vmlinux /var/lib/boat/images/ubuntu-24.04/rootfs.ext4",
          )
          token = json.loads(machine.succeed("boat-admin init -user me -team ops 2>/dev/null"))["token"]
          machine.wait_for_open_port(8740)
          machine.succeed(f"boat login --url http://127.0.0.1:8740 --key {token} >&2")

      with subtest("the module's settings hold for a real Firecracker under the jailer"):
          vm = json.loads(machine.succeed("boat new --name real --json", timeout=600))
          assert vm["state"] == "running", vm
          pids = fc_pids()
          assert len(pids) == 1, pids
          pid = pids[0]
          assert machine.succeed(f"cat /proc/{pid}/oom_score_adj").strip() == "500"
          assert "boat-vms" in machine.succeed(f"cat /proc/{pid}/cgroup"), "not under boat-vms"
          assert machine.succeed(f"stat -c %u /proc/{pid}").strip() != "0", "firecracker runs as root"
          own, host = machine.succeed(f"readlink /proc/{pid}/ns/mnt /proc/1/ns/mnt").split()
          assert own != host, "firecracker shares the host's mount namespace"

      with subtest("switch to the next generation: boatd and Firecracker untouched, boat-api updated"):
          boatd_before = machine.succeed("systemctl show -P MainPID boatd.service").strip()
          api_before = machine.succeed("systemctl show -P MainPID boat-api.service").strip()
          # No unit blocks this switch: boatd is not restarted and boat-api stops in
          # under 1 s. The time goes to the activation script itself (/etc, users,
          # daemon-reload) in a nested VM that also runs a spinning L3 guest. Run
          # 41470010397: attempt 1 took 205 s (16 s to /etc, 100 s from /etc to the
          # unit restarts); attempt 2 needed 74 s just to reach /etc and passed the
          # old 300 s limit inside that same /etc-to-restart step.
          machine.succeed("/run/current-system/specialisation/next/bin/switch-to-configuration test >&2", timeout=900)
          assert machine.succeed("systemctl show -P MainPID boatd.service").strip() == boatd_before, "the switch restarted boatd"
          assert fc_pids() == pids, "the switch touched the VM's process"
          machine.wait_for_unit("boat-api.service")
          machine.wait_for_open_port(8740)
          assert machine.succeed("systemctl show -P MainPID boat-api.service").strip() != api_before, "boat-api kept the old binary"
          # The new boat-api still talks to the old boatd.
          machine.succeed("boat list --json | jq -e 'select(.name == \"real\" and .state == \"running\")'")
          machine.succeed("${health} --wait 120")
          machine.succeed("systemctl start boat-reboot-check.service")
          prom = machine.succeed("cat /var/lib/boat-metrics/boat.prom")
          assert "boat_reboot_pending 1" in prom and "boatd-or-firecracker" in prom, prom

      with subtest("rm leaves nothing behind"):
          machine.succeed("boat rm real --json >&2", timeout=300)
          assert not fc_pids(), "firecracker left behind"

      emit({"test": "boat-real", "pass": True})
      machine.shutdown()
    '';
}
