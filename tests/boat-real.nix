# The real thing under the real module: real boatd, boat-api, jailer, Firecracker
# and guest image, started by components/boat/module.nix on NixOS, with VMs that
# must survive what an operator's updates do to them:
#   - `switch` to a new generation (new boatd, boat-api, config): nothing touches
#     the VM, and boat-api comes back;
#   - restarting boatd: snapshot and resume, same boot_id, processes alive;
#   - a graceful reboot, as a maintenance reboot does: same.
#
# It needs /dev/kvm inside the test VM (nested) and the guest image, which is
# built with docker outside Nix, so it is not in `nix flake check`; boat-e2e runs
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
  # Everything inside the VM is very slow here: see bootTimeoutS.
  globalTimeout = 7000;
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
        # Firecracker runs three virtualisation levels down here (runner, this
        # test VM, the guest), where a guest boot takes many minutes.
        boatdSettings.bootTimeoutS = 1500;
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
      vm_boots = {}


      def fc_pids():
          return machine.succeed("pgrep -x firecracker || true").split()


      def in_vm(cmd, timeout=900):
          return machine.succeed(f"boat exec real -- bash -c {shlex.quote(cmd)}", timeout=timeout).strip()


      def assert_vm_kept(label):
          # Same kernel instance, same process, same file: it was not rebooted or re-created.
          machine.wait_until_succeeds("boat list --json | jq -e 'select(.name == \"real\" and .state == \"running\")'", timeout=1700)
          assert in_vm("cat /proc/sys/kernel/random/boot_id") == vm_boots["boot_id"], f"{label}: the VM rebooted"
          assert in_vm("cat /home/dev/marker") == "still-here", f"{label}: file lost"
          in_vm(f"kill -0 {vm_boots['pid']}")


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

      with subtest("a real VM boots under the jailer"):
          start = time.monotonic()
          vm = json.loads(machine.succeed("boat new --name real --json", timeout=1700))
          boot_sec = round(time.monotonic() - start, 1)
          assert vm["state"] == "running", vm
          assert fc_pids(), "no firecracker process"
          # The module's limits apply to it: in boatd's cgroup tree, OOM killer's first pick.
          pid = fc_pids()[0]
          assert machine.succeed(f"cat /proc/{pid}/oom_score_adj").strip() == "500"
          assert "boat-vms" in machine.succeed(f"cat /proc/{pid}/cgroup"), "not under boat-vms"
          uid = machine.succeed(f"stat -c %u /proc/{pid}").strip()
          assert uid != "0", "firecracker runs as root"
          in_vm("echo still-here >/home/dev/marker")
          vm_boots["pid"] = in_vm("setsid nohup sleep 100000 >/dev/null 2>&1 </dev/null & echo $!")
          vm_boots["boot_id"] = in_vm("cat /proc/sys/kernel/random/boot_id")
          assert_vm_kept("baseline")

      with subtest("switch to the next generation with the VM running"):
          fc_before = fc_pids()
          boatd_before = machine.succeed("systemctl show -P MainPID boatd.service").strip()
          machine.succeed("/run/current-system/specialisation/next/bin/switch-to-configuration test >&2", timeout=300)
          assert machine.succeed("systemctl show -P MainPID boatd.service").strip() == boatd_before, "the switch restarted boatd"
          assert fc_pids() == fc_before, "the switch touched the VM's process"
          machine.wait_for_unit("boat-api.service")
          machine.wait_for_open_port(8740)
          assert_vm_kept("after switch")
          machine.succeed("${health} --wait 120")
          machine.succeed("systemctl start boat-reboot-check.service")
          prom = machine.succeed("cat /var/lib/boat-metrics/boat.prom")
          assert "boat_reboot_pending 1" in prom, prom

      with subtest("restart boatd onto the new binary: snapshot and resume"):
          start = time.monotonic()
          machine.succeed("systemctl restart boatd.service", timeout=1200)
          assert fc_pids() != fc_before, "firecracker was not replaced"
          assert_vm_kept("after boatd restart")
          restart_sec = round(time.monotonic() - start, 1)
          machine.succeed("systemctl start boat-reboot-check.service")
          assert "boat_reboot_pending 0" in machine.succeed("cat /var/lib/boat-metrics/boat.prom")

      with subtest("graceful reboot, as a maintenance reboot does"):
          start = time.monotonic()
          machine.shutdown()
          machine.start()
          machine.wait_for_unit("boatd.service")
          machine.wait_for_unit("boat-api.service")
          machine.wait_for_open_port(8740)
          assert_vm_kept("after reboot")
          reboot_sec = round(time.monotonic() - start, 1)
          machine.succeed("boat rm real --json >&2", timeout=300)
          assert not fc_pids(), "firecracker left behind"

      emit({"test": "boat-real", "vmBootSec": boot_sec, "boatdRestartResumeSec": restart_sec, "rebootResumeSec": reboot_sec})
      machine.shutdown()
    '';
}
