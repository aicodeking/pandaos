# The boat server install, the way an operator does it: boot the iso-server
# modules from a virtual CD under UEFI, run panda-install --profile server with
# every server flag onto two blank disks, boot the installed disk, and check it.
#
# As in iso-boot.nix, the image is packages.iso-server's modules plus nixpkgs'
# test-instrumentation (the driver's backdoor) and fast zstd, so this proves
# that configuration boots and installs, not that the released file is
# byte-identical. The installed system also carries the backdoor.
#
# Not covered: local.nix taking effect. It applies at the first nixos-rebuild,
# which needs the network; the matching settings are only evaluated here.
{ pkgs, self, t }:
let
  inherit (pkgs) lib;
  system = pkgs.stdenv.hostPlatform.system;
  instrumentation = "${pkgs.path}/nixos/modules/testing/test-instrumentation.nix";
  domain = "boat.example.test";
  email = "ops@example.test";
  prefix = "2001:db8:1:2:b0a7::/80";

  target =
    (self.lib.mkPandaSystem {
      profile = "server";
      modules = [
        ../nixos/installer/target.nix
        instrumentation
      ];
    }).config.system.build.toplevel;

  # What local.nix asks for must evaluate (module assertions included).
  withLocalNix =
    (self.lib.mkPandaSystem {
      profile = "server";
      modules = [
        {
          services.boat = {
            inherit domain;
            acmeEmail = email;
            ipv6Prefix = prefix;
            gateway.allowOnDemandTLS = true;
          };
          fileSystems."/var/lib/boat" = {
            device = lib.mkForce "/dev/disk/by-uuid/00000000-0000-4000-8000-000000000000";
            options = lib.mkForce [ "compress=zstd:1" "noatime" ];
          };
        }
      ];
    }).config.system.build.toplevel.drvPath;

  isoConfig =
    (import "${pkgs.path}/nixos/lib/eval-config.nix" {
      inherit system;
      modules = [
        self.nixosModules.default
        self.nixosModules.headless
        ../nixos/modules/iso.nix
        {
          pandaos.installer.systems.server = builtins.seq withLocalNix target;
          pandaos.installer.defaultProfile = "server";
        }
        instrumentation
        { isoImage.squashfsCompression = lib.mkForce "zstd -Xcompression-level 3"; }
      ];
    }).config;
  iso = "${isoConfig.system.build.isoImage}/iso/${isoConfig.image.fileName or isoConfig.isoImage.isoName}";

  qemu = "${pkgs.qemu_test}/bin/qemu-kvm -cpu max -smp 2 -bios ${pkgs.OVMF.fd}/FV/OVMF.fd";
  img = "${pkgs.qemu_test}/bin/qemu-img";
in
pkgs.testers.runNixOSTest {
  name = "boat-install";
  nodes = { };
  testScript = t.prelude + ''
    import subprocess

    workdir = os.getcwd()
    disks = ""
    for name, size in [("sys", "12G"), ("data", "9G")]:
        path = f"{workdir}/{name}.qcow2"
        subprocess.run(["${img}", "create", "-q", "-f", "qcow2", path, size], check=True)
        disks += f" -drive file={path},if=virtio,format=qcow2"

    live = create_machine("${qemu} -m 3072 -cdrom ${iso}" + disks, name="live")
    live.start()
    live.wait_for_unit("multi-user.target")

    with subtest("live system: UEFI, squashfs root, panda-install offers server"):
        live.succeed("test -d /sys/firmware/efi")
        live.succeed("findmnt -no FSTYPE /nix/.ro-store | grep -qx squashfs")
        assert "--data-disk" in live.succeed("panda-install --help 2>&1")

    with subtest("server flags are refused for another profile, and bad values for this one"):
        common = "--disk /dev/vda --password-hash '$6$x$y' --yes"
        out = live.fail(f"panda-install {common} --profile server --domain 'bad domain' --acme-email ${email} 2>&1")
        assert "bad domain" in out, out
        out = live.fail(f"panda-install {common} --profile server --acme-email ${email} 2>&1")
        assert "--domain is required" in out, out
        out = live.fail(f"panda-install {common} --profile server --domain ${domain} --acme-email ${email} --data-disk /dev/vda 2>&1")
        assert "must differ" in out, out

    with subtest("install"):
        pw_hash = live.succeed("mkpasswd -m sha-512 -S boatinstallsalt boat-test-pw").strip()
        start = time.monotonic()
        out = live.succeed(
            "panda-install --disk /dev/vda --data-disk /dev/vdb --profile server"
            " --domain ${domain} --acme-email ${email} --ipv6-prefix ${prefix}"
            f" --hostname boathost --user panda --password-hash '{pw_hash}' --yes 2>&1"
        )
        print(out)
        assert "Done." in out and "nixos-rebuild switch" in out, out
        install_sec = round(time.monotonic() - start, 1)
        assert live.succeed("blkid -s LABEL -o value /dev/vdb").strip() == "PANDADATA"
        live.shutdown()

    # No CD now: firmware boots the system disk's EFI/BOOT fallback.
    machine = create_machine("${qemu} -m 2048" + disks, name="installed")
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("/etc/pandaos records the choices"):
        assert machine.succeed("hostname").strip() == "boathost"
        local = machine.succeed("cat /etc/pandaos/local.nix")
        for want in [
            'services.boat.domain = "${domain}"',
            'services.boat.acmeEmail = "${email}"',
            'services.boat.ipv6Prefix = "${prefix}"',
            "services.boat.gateway.allowOnDemandTLS = true",
        ]:
            assert want in local, f"{want} not in {local}"
        assert 'profile = "server"' in machine.succeed("cat /etc/pandaos/flake.nix")
        uuid = machine.succeed("blkid -s UUID -o value /dev/vdb").strip()
        disk = machine.succeed("cat /etc/pandaos/disk.nix")
        assert uuid in disk and "/var/lib/boat" in disk and "nofail" not in disk, disk
        assert uuid in machine.succeed("cat /etc/pandaos/data-disk")

    with subtest("VM data is on the data disk, btrfs"):
        fs = machine.succeed("findmnt -no SOURCE,FSTYPE /var/lib/boat")
        assert "/dev/vdb" in fs and "btrfs" in fs, fs
        machine.wait_for_unit("boat-secrets.service")
        machine.succeed("test -s /var/lib/boat/secrets/boat-master-key.cred")
        machine.succeed("test -s /var/lib/boat/secrets/boat-desktop-key.cred")
        # Sealed: no plaintext key anywhere under the data dir.
        machine.fail("find /var/lib/boat -name '*.key' | grep .")

    with subtest("services and health"):
        for unit in ["boat-api", "boat-gateway", "caddy"]:
            machine.wait_for_unit(f"{unit}.service")
        machine.succeed("panda-health --wait 120")
        if machine.execute("test -c /dev/kvm")[0] == 0:
            machine.wait_for_unit("boatd.service")
            machine.wait_for_file("/run/boat/boatd.sock")
        else:
            machine.wait_until_succeeds("journalctl -u boatd.service | grep -q '/dev/kvm is missing'")

    with subtest("first admin key, over TLS, on the real binaries"):
        # The prebuilt system has the placeholder domain until the first rebuild.
        h = "api.boat.localhost"
        curl = f"curl -sk --resolve {h}:443:127.0.0.1 "
        machine.wait_until_succeeds(curl + f"-o /dev/null -w '%{{http_code}}' https://{h}/healthz | grep -x 200", timeout=120)
        token = json.loads(machine.succeed("boat-admin init -user me -team ops 2>/dev/null"))["token"]
        assert token.startswith("boat_"), token
        machine.wait_for_unit("boat-api.service")
        machine.wait_until_succeeds(curl + f"-o /dev/null -w '%{{http_code}}' https://{h}/healthz | grep -x 200", timeout=60)
        me = json.loads(machine.succeed(curl + f"-H 'Authorization: Bearer {token}' https://{h}/v1/me"))
        assert "ops" in json.dumps(me), me
        # The bare token, for scripts: root only.
        assert machine.succeed("cat /root/boat-owner-token").strip() == token
        assert machine.succeed("stat -c %a /root/boat-owner-token").strip() == "600"

    with subtest("no local control planes: Caddy admin API, LLMNR, mDNS"):
        # Caddy's admin API took unauthenticated config changes from any local user.
        machine.fail("curl -sS --max-time 5 http://127.0.0.1:2019/config/")
        machine.fail("ss -Hltn 'sport = :2019' | grep -q .")
        machine.fail("ss -Hlnu '( sport = :5353 or sport = :5355 )' | grep -q .")
        machine.fail("ss -Hlnt 'sport = :5355' | grep -q .")
        # routes.json (every VM's IP and team) is 0640 root:boat-routes: only its readers.
        for u in ["boat-gateway", "boat"]:
            assert "boat-routes" in machine.succeed(f"id -nG {u}").split(), u
        assert "boat-routes" not in machine.succeed("id -nG panda").split()

    emit({"test": "boat-install", "installSec": install_sec})
    machine.shutdown()
  '';
}
