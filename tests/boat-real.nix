# Real boatd, boat-api, jailer and Firecracker, started by
# components/boat/module.nix on NixOS. Two tests:
#
# boat-real (this file's derivation): a NixOS VM through a real generation
# switch. Firecracker runs three virtualisation levels down there (runner, the
# test VM, the guest). It starts, but the guest agent never answered, not even
# in 25 min (runs 41325437352, 41328694293), so nothing there depends on the
# guest. It checks the host side: the jail, cgroup and OOM settings on the real
# process, and that a `switch` to a new boatd and boat-api leaves that process
# and boatd alone.
#
# boat-real.guest: the same server configuration as a systemd-nspawn container
# on the runner itself, with the runner's /dev/kvm, so the guest is one level
# down as in boat-e2e, where guests boot. Its boatd creates a real guest from
# the real image and drives exec, stop/resume, fork, snapshot, delete, and a
# boatd restart with the VMs running. The driver must run as real root outside
# the Nix sandbox (the jailer mknods /dev/kvm in each chroot, which a build's
# user namespace refuses), so boat-e2e builds `boat-real.guest.driver` and runs
# it with sudo, with a btrfs filesystem mounted at /mnt/boat-real-data. What it
# does not prove: it is not a booted kernel of ours (the runner's Ubuntu kernel,
# boot.isContainer, no bootloader/initrd/udev), the network is nspawn's private
# one (no uplink, so no guest egress), /var/lib/boat is a bind mount rather than
# the PANDADATA disk, zram is off, and there is no `switch` (nspawn containers
# in NixOS tests refuse specialisations; the VM test covers it).
#
# Both need the guest image, built with docker outside Nix, so they are not in
# `nix flake check`; boat-e2e runs
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

  # Python for both test scripts: put the guest image where boatd looks for it,
  # as boat-images leaves it. Interpolate it at the script's top level.
  installImage = ''
    def install_image(m):
        m.succeed(
            "mkdir -p /var/lib/boat/images/ubuntu-24.04",
            "cp --sparse=always ${guest}/vmlinux /var/lib/boat/images/ubuntu-24.04/vmlinux",
            "cp --sparse=always ${guest}/rootfs-slim.ext4 /var/lib/boat/images/ubuntu-24.04/rootfs.ext4",
            # Files from the Nix store are read-only.
            "chmod 0644 /var/lib/boat/images/ubuntu-24.04/vmlinux /var/lib/boat/images/ubuntu-24.04/rootfs.ext4",
        )
  '';

  vmTest = pkgs.testers.runNixOSTest {
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


        ${installImage}


        machine.wait_for_unit("multi-user.target")

        with subtest("real boatd on the module: KVM, images, first key"):
            machine.succeed("test -c /dev/kvm")
            machine.wait_for_unit("boatd.service")
            machine.wait_for_unit("boat-api.service")
            machine.wait_for_file("/run/boat/boatd.sock")
            install_image(machine)
            token = json.loads(machine.succeed("boat-admin init -user me -team ops 2>/dev/null"))["token"]
            machine.wait_for_open_port(8740)
            machine.succeed(f"boat login --url http://127.0.0.1:8740 --key {token} >&2")

        with subtest("the module's settings hold for a real Firecracker under the jailer"):
            vm = json.loads(machine.succeed("boat new --name real --json", timeout=600))
            # The guest's serial console and Firecracker's own log, for when it does not come up.
            print(machine.execute("tail -n 60 /var/lib/boat/vms/*/firecracker.log")[1])
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
  };

  # On the runner: a btrfs filesystem (the real one is the PANDADATA disk).
  hostDataDir = "/mnt/boat-real-data";

  guestTest = pkgs.testers.runNixOSTest {
    name = "boat-real-guest";
    containers.server =
      { config, lib, ... }:
      {
        imports = [
          self.nixosModules.default
          self.nixosModules.server
        ];
        pandaos.firstboot.enable = false;
        networking.hostName = lib.mkForce "server";
        # nspawn bind-mounts the host's btrfs there; no PANDADATA device in a container.
        fileSystems."/var/lib/boat".enable = false;
        zramSwap.enable = lib.mkForce false; # no zram device in a container
        services.boat = {
          boatdPackage = boatd;
          apiPackage = api;
          sizes.t = {
            vcpu = 1;
            mem_mib = 1024;
            disk_gib = 6;
          };
          defaultSize = "t";
        };
        # The test framework's options without --keep-unit/--register=no, which
        # assume the Nix sandbox: here nspawn gets its own scope, so systemd
        # delegates the cgroup controllers boatd needs for the VMs' limits, and
        # the --property device rules apply. /proc and /sys are nspawn's own.
        virtualisation.systemd-nspawn.options = lib.mkForce [
          "--private-network"
          "--machine=${config.system.name}"
          "--bind-ro=/nix/store:/nix/store"
          "--private-users=no"
          "--register=no"
          "--notify-ready=yes"
          "--bind=/dev/kvm"
          # The jailer mknods kvm, net/tun (and userfaultfd) in each chroot: all misc devices.
          "--property=DeviceAllow=char-misc rwm"
          "--bind=${hostDataDir}:/var/lib/boat"
        ];
      };

    testScript = t.prelude + ''
      def fc_pids():
          return server.succeed("pgrep -x firecracker || true").split()


      def vm(name) -> dict:
          for line in server.succeed("boat list --json").splitlines():
              v = json.loads(line)
              if v["name"] == name:
                  return v
          raise AssertionError(f"no VM {name}")


      def run(name, cmd, timeout=300):
          return server.succeed(f"boat exec {name} -- bash -c {shlex.quote(cmd)}", timeout=timeout).strip()


      def secs(f):
          t0 = time.monotonic()
          out = f()
          return out, round(time.monotonic() - t0, 2)


      def diag():
          # What the guest printed (serial console) and what Firecracker logged.
          print(server.execute(
              "for f in /var/lib/boat/vms/*/firecracker.log; do echo \"--- $f\"; tail -n 80 \"$f\"; done; "
              "journalctl -u boatd.service -u boat-api.service --no-pager | tail -n 120"
          )[1])


      ${installImage}


      m: dict = {"test": "boat-real-guest"}
      try:
          server.wait_for_unit("multi-user.target")

          with subtest("server profile's boatd on the runner's /dev/kvm"):
              server.succeed("test -c /dev/kvm")
              assert server.succeed("findmnt -no FSTYPE /var/lib/boat").strip() == "btrfs"
              server.wait_for_unit("boatd.service")
              server.wait_for_unit("boat-api.service")
              server.wait_for_file("/run/boat/boatd.sock")
              install_image(server)
              token = json.loads(server.succeed("boat-admin init -user me -team ops 2>/dev/null"))["token"]
              server.wait_for_open_port(8740)
              server.succeed(f"boat login --url http://127.0.0.1:8740 --key {token} >&2")

          with subtest("new: the real image boots under the jailer and its agent answers"):
              a, m["create_s"] = secs(lambda: json.loads(server.succeed("boat new --name a --json", timeout=600)))
              assert a["state"] == "running" and a["agent_ready"], a
              pids = fc_pids()
              assert len(pids) == 1, pids
              pid = pids[0]
              assert server.succeed(f"cat /proc/{pid}/oom_score_adj").strip() == "500"
              assert "boat-vms" in server.succeed(f"cat /proc/{pid}/cgroup"), "not under boat-vms"
              assert server.succeed(f"stat -c %u /proc/{pid}").strip() != "0", "firecracker runs as root"

          with subtest("exec"):
              assert run("a", "hostname") == "a"
              assert run("a", "uname -s") == "Linux"
              run("a", "echo marker-a >/home/dev/marker")
              assert run("a", "cat /home/dev/marker") == "marker-a"

          with subtest("stop -> start resumes: memory and disk kept"):
              boot_a = run("a", "cat /proc/sys/kernel/random/boot_id")
              st, m["stop_s"] = secs(lambda: json.loads(server.succeed("boat stop a --json", timeout=600)))
              assert st["state"] == "stopped" and vm("a")["has_snapshot"], st
              assert not fc_pids(), "a stopped VM kept its Firecracker"
              st, m["resume_s"] = secs(lambda: json.loads(server.succeed("boat start a --json", timeout=600)))
              assert st["state"] == "running", st
              assert run("a", "cat /home/dev/marker") == "marker-a"
              assert run("a", "cat /proc/sys/kernel/random/boot_id") == boot_a, "start cold-booted"

          with subtest("fork"):
              b, m["fork_s"] = secs(lambda: json.loads(server.succeed("boat fork a --name b --json", timeout=600)))
              assert b["state"] == "running" and b["agent_ready"] and b["ip"] != a["ip"], b
              assert run("b", "cat /home/dev/marker") == "marker-a"
              assert run("b", "hostname") == "b" and run("a", "hostname") == "a"
              assert len(fc_pids()) == 2

          with subtest("snapshot, then new from it"):
              sid = json.loads(server.succeed("boat snapshots a create --name s1 --json", timeout=600))["id"]
              assert sid, sid
              listed = [json.loads(x)["id"] for x in server.succeed("boat snapshots a ls --json").splitlines()]
              assert sid in listed, listed
              run("a", "echo after >/home/dev/later")
              c, m["new_from_snapshot_s"] = secs(
                  lambda: json.loads(server.succeed(f"boat new --snapshot {sid} --name c --json", timeout=600))
              )
              assert c["state"] == "running", c
              assert run("c", "cat /home/dev/marker") == "marker-a"
              run("c", "test ! -e /home/dev/later")
              assert run("c", "hostname") == "c"

          with subtest("boatd restart: the VMs and their state survive"):
              names = ["a", "b", "c"]
              boots = {n: run(n, "cat /proc/sys/kernel/random/boot_id") for n in names}
              for n in names:
                  run(n, f"echo before-restart-{n} >/home/dev/restart")
              old = server.succeed("systemctl show -P MainPID boatd.service").strip()
              _, m["boatd_restart_s"] = secs(lambda: server.succeed("systemctl restart boatd.service", timeout=900))
              server.wait_for_unit("boatd.service")
              assert server.succeed("systemctl show -P MainPID boatd.service").strip() != old, "boatd not restarted"
              # boatd snapshots the VMs on stop and resumes them on start.
              def all_running(_last) -> bool:
                  try:
                      return all(vm(n)["state"] == "running" for n in names)
                  except Exception:  # boat-api or boatd not answering yet
                      return False

              retry(all_running, timeout_seconds=600)
              for n in names:
                  assert run(n, "cat /home/dev/restart") == f"before-restart-{n}", n
                  assert run(n, "cat /proc/sys/kernel/random/boot_id") == boots[n], f"{n} cold-booted"
              assert len(fc_pids()) == 3

          with subtest("rm leaves nothing behind"):
              for n in ["c", "b", "a"]:
                  server.succeed(f"boat rm {n} --json >&2", timeout=300)
              assert server.succeed("boat list --json").strip() == ""
              assert not fc_pids(), "firecracker left behind"
              server.succeed("test -z \"$(ls -A /var/lib/boat/vms)\"")
      except Exception:
          diag()
          raise

      m["pass"] = True
      emit(m)
      server.shutdown()
    '';
  };
in
vmTest // { guest = guestTest; }
