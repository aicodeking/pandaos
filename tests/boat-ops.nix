# boat operations on a host (stub boatd and boat-api; real units, restic, btrfs):
#
# - Update: `switch` between two prebuilt generations while "VMs" run. boatd and
#   its children keep running (restartIfChanged = false), boat-api moves to the
#   new version, panda-health passes, the pending-reboot metric turns on, and a
#   boatd restart then snapshots and resumes.
#   The boatd restart is the maintenance window's (maintenance.reboot, action
#   auto): only boatd changed, so it restarts boatd instead of rebooting, and
#   `panda-update status` showed the pending change first.
# - Hardening: boat-api has its MemoryMax and no OOM protection; node_exporter
#   listens on 127.0.0.1 only.
#   This proves the unit behaviour. Real VMs through a boatd stop and restart
#   are in gateway/e2e-edge.sh, on a runner where Firecracker works.
# - Backup and restore: a real restic repository, restored into a scratch dir;
#   nested VM subvolumes included, images excluded, no plaintext key in the
#   backup, and the master key recoverable the documented way.
{ pkgs, self, t }:
let
  stubBoatd =
    version:
    pkgs.writeShellScriptBin "boatd" ''
      export PATH=${pkgs.lib.makeBinPath [ pkgs.coreutils ]}
      d=/var/lib/boat/stub
      mkdir -p $d
      echo "${version}" >$d/boatd-version
      # A "VM": a child that must outlive a generation switch.
      if [ -e $d/snapshotted ]; then echo "${version}" >$d/resumed-by; fi
      sleep infinity &
      vm=$!
      echo "$vm" >$d/vm.pid
      trap 'echo "${version}" >$d/snapshotted; kill $vm; exit 0' TERM
      wait $vm
    '';
  stubApi =
    version:
    pkgs.writeShellScriptBin "boat-api" ''
      echo "${version}" >/var/lib/boat/api/version
      exec ${pkgs.python3}/bin/python3 -m http.server --bind 127.0.0.1 8740 --directory /etc/boat
    '';
  stubCli = pkgs.writeShellScriptBin "boat" "echo stub";
  pw = pkgs.writeText "restic-pass" "test-repository-password";
in
pkgs.testers.runNixOSTest {
  name = "boat-ops";
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
        memorySize = 2048;
        emptyDiskImages = [ 4096 ];
        fileSystems."/var/lib/boat" = {
          device = "/dev/vdb";
          fsType = "btrfs";
          autoFormat = true;
        };
      };
      environment.systemPackages = [ pkgs.btrfs-progs pkgs.restic pkgs.e2fsprogs ];
      services.boat = {
        requireKvm = false;
        boatdPackage = lib.mkDefault (stubBoatd "v1");
        apiPackage = lib.mkDefault (stubApi "v1");
        cliPackage = stubCli;
        # The test starts the window's unit itself; the timer never fires.
        maintenance = {
          reboot = true;
          onCalendar = "2099-01-01";
        };
        backup = {
          enable = true;
          repository = "/var/backup/restic";
          passwordFile = "${pw}";
        };
      };
      specialisation.v2.configuration.services.boat = {
        boatdPackage = stubBoatd "v2";
        apiPackage = stubApi "v2";
      };
    };

  testScript =
    { nodes, ... }:
    let
      health = "${nodes.machine.system.build.panda-health}/bin/panda-health";
    in
    t.prelude
    + ''
      machine.wait_for_unit("multi-user.target")
      for unit in ["boatd", "boat-api", "boat-gateway", "caddy"]:
          machine.wait_for_unit(f"{unit}.service")
      machine.wait_for_file("/var/lib/boat/stub/vm.pid")
      read = lambda f: machine.succeed(f"cat {f}").strip()
      main_pid = lambda: machine.succeed("systemctl show -P MainPID boatd.service").strip()

      with subtest("hardening: boat-api memory cap, node_exporter on loopback"):
          show = lambda u, p: machine.succeed(f"systemctl show -P {p} {u}").strip()
          assert show("boat-api", "MemoryMax") == str(2 * 1024**3), show("boat-api", "MemoryMax")
          assert int(show("boat-api", "OOMScoreAdjust")) > -900, show("boat-api", "OOMScoreAdjust")
          machine.wait_for_unit("prometheus-node-exporter.service")
          machine.wait_until_succeeds("ss -Hltn 'sport = :9100' | grep -q 127.0.0.1:9100")
          listen = machine.succeed("ss -Hltn 'sport = :9100'")
          assert "0.0.0.0:9100" not in listen and "[::]:9100" not in listen and "*:9100" not in listen, listen

      with subtest("no guest image declared: boat-images says how to install one, boat-admin does"):
          machine.wait_for_unit("boat-images.service")
          machine.succeed("journalctl -u boat-images.service | grep -q 'boat-admin images import'")
          machine.succeed(
              "head -c 1M /dev/urandom >/tmp/vmlinux",
              "truncate -s 16M /tmp/rootfs.ext4 && mkfs.ext4 -q /tmp/rootfs.ext4",
              "${pkgs.zstd}/bin/zstd -q /tmp/rootfs.ext4 -o /tmp/rootfs.ext4.zst",
              "head -c 1M /dev/zero >/tmp/not-ext4",
          )
          # Not an ext4 image: refused, nothing installed.
          machine.fail("boat-admin images import /tmp/vmlinux /tmp/not-ext4")
          machine.fail("test -e /var/lib/boat/images/ubuntu-24.04/rootfs.ext4")
          out = machine.succeed("boat-admin images import /tmp/vmlinux /tmp/rootfs.ext4.zst 2>&1")
          assert "installed image ubuntu-24.04" in out and "localImages" not in out, out
          machine.succeed(
              "cmp /tmp/vmlinux /var/lib/boat/images/ubuntu-24.04/vmlinux",
              "cmp /tmp/rootfs.ext4 /var/lib/boat/images/ubuntu-24.04/rootfs.ext4",
              "test $(stat -c %a /var/lib/boat/images/ubuntu-24.04/rootfs.ext4) = 644",
          )
          # Another id: installed, and boat-admin says how to declare it.
          out = machine.succeed("boat-admin images import --id extra /tmp/vmlinux /tmp/rootfs.ext4 2>&1")
          assert 'services.boat.localImages = [ "extra" ]' in out, out
          listing = machine.succeed("boat-admin images list")
          assert "ubuntu-24.04" in listing and "extra" in listing and "undeclared" in listing, listing
          machine.succeed("rm -rf /var/lib/boat/images/extra")

      with subtest("switch to the next generation with VMs running"):
          assert read("/var/lib/boat/api/version") == "v1"
          boatd_before, vm_before = main_pid(), read("/var/lib/boat/stub/vm.pid")
          machine.succeed("systemctl start boat-reboot-check.service")
          assert "boat_reboot_pending 0" in read("/var/lib/boat-metrics/boat.prom")

          machine.succeed("/run/current-system/specialisation/v2/bin/switch-to-configuration test >&2")

          assert main_pid() == boatd_before, "the switch restarted boatd"
          machine.succeed(f"kill -0 {vm_before}")
          assert read("/var/lib/boat/stub/boatd-version") == "v1"
          machine.fail("test -e /var/lib/boat/stub/snapshotted")
          machine.wait_until_succeeds("grep -qx v2 /var/lib/boat/api/version")
          machine.succeed("${health} --wait 60")

      with subtest("the reboot check reports what is still old"):
          machine.succeed("systemctl start boat-reboot-check.service")
          prom = read("/var/lib/boat-metrics/boat.prom")
          assert "boat_reboot_pending 1" in prom and "boatd-or-firecracker" in prom, prom
          assert "boatd" in read("/run/boat/reboot-pending")

      with subtest("the maintenance window applies it with a boatd restart: snapshot, then resume on the new version"):
          status = machine.succeed("panda-update status")
          assert "reboot pending: boatd-or-firecracker" in status, status
          assert "boat_maintenance_window_enabled 1" in read("/var/lib/boat-metrics/boat.prom")
          boot_id = read("/proc/sys/kernel/random/boot_id")
          machine.succeed("systemctl start boat-maintenance-reboot.service")
          assert read("/proc/sys/kernel/random/boot_id") == boot_id, "the window rebooted for a boatd-only change"
          assert 'action="restart-boatd"' in read("/var/lib/boat-metrics/boat-maintenance.prom")
          assert read("/var/lib/boat/stub/snapshotted") == "v1"
          machine.wait_until_succeeds("grep -qx v2 /var/lib/boat/stub/resumed-by")
          machine.fail(f"kill -0 {vm_before}")
          machine.succeed("systemctl start boat-reboot-check.service")
          assert "boat_reboot_pending 0" in read("/var/lib/boat-metrics/boat.prom")

      with subtest("backup: real restic, nested subvolumes, no leftovers"):
          machine.succeed(
              "btrfs subvolume create /var/lib/boat/vmsub",
              "echo vm-disk-data >/var/lib/boat/vmsub/rootfs.ext4",
              "mkdir -p /var/lib/boat/images/x && echo image >/var/lib/boat/images/x/vmlinux",
              "echo api-db >/var/lib/boat/api/store.json",
              "systemctl start restic-backups-boat.service",
          )
          machine.wait_until_fails("test -e /var/lib/boat/.backup", timeout=60)

      with subtest("restore into a scratch dir"):
          env = "RESTIC_REPOSITORY=/var/backup/restic RESTIC_PASSWORD_FILE=${pw}"
          machine.succeed(f"{env} restic restore latest --target /tmp/r >&2")
          r = "/tmp/r/var/lib/boat/.backup"
          assert read(f"{r}/sub/vmsub/rootfs.ext4") == "vm-disk-data"
          assert read(f"{r}/top/api/store.json") == "api-db"
          machine.fail(f"test -e {r}/top/images/x/vmlinux")
          # Keys in the backup are sealed only; none is usable without this host.
          machine.fail("find /tmp/r -name '*.key' | grep .")
          machine.succeed(f"test -s {r}/top/secrets/boat-master-key.cred")
          machine.succeed(f"test -s {r}/top/secrets/boat-desktop-key.cred")

      with subtest("master key recovery as documented, and the sealed copy is host-bound"):
          live = "systemd-creds decrypt --name=boat-master-key"
          machine.succeed(
              f"{live} {r}/top/secrets/boat-master-key.cred - >/tmp/from-backup",
              f"{live} /var/lib/boat/secrets/boat-master-key.cred - >/tmp/live",
              "cmp /tmp/from-backup /tmp/live",
              # Recovery onto a new host: seal the offline copy with that host's key.
              "systemd-creds encrypt --with-key=auto --name=boat-master-key /tmp/live /tmp/resealed.cred",
              f"{live} /tmp/resealed.cred - | cmp - /tmp/live",
          )
          # Without this host's secret the credential does not open.
          machine.succeed("mv /var/lib/systemd/credential.secret /var/lib/systemd/credential.secret.away")
          machine.fail(f"{live} {r}/top/secrets/boat-master-key.cred - >/dev/null")
          machine.succeed("mv /var/lib/systemd/credential.secret.away /var/lib/systemd/credential.secret")

      emit({"test": "boat-ops", "pass": True})
      machine.shutdown()
    '';
}
