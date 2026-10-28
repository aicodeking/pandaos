# Boot the headless live ISO from a virtual CD-ROM (BIOS boot via its
# bootloader, squashfs root) and check the live system and panda-install.
#
# The image is built from exactly the modules of packages.iso-headless (base +
# headless + iso.nix, installing nixosConfigurations.headless) plus nixpkgs'
# test-instrumentation.nix, so the test driver can run commands in it. It also
# uses fast zstd instead of level 19 to save CI minutes. So this proves the
# image boots, not that the released artifact is byte-identical; CI's `iso`
# job builds and sizes the real one.
{ pkgs, self, t }:
let
  inherit (pkgs) lib;
  isoConfig =
    (import "${pkgs.path}/nixos/lib/eval-config.nix" {
      inherit (pkgs.stdenv.hostPlatform) system;
      modules = [
        self.nixosModules.default
        self.nixosModules.headless
        ../nixos/modules/iso.nix
        {
          pandaos.installer.systems.headless =
            self.nixosConfigurations.headless.config.system.build.toplevel;
        }
        "${pkgs.path}/nixos/modules/testing/test-instrumentation.nix"
        { isoImage.squashfsCompression = lib.mkForce "zstd -Xcompression-level 3"; }
      ];
    }).config;
  iso = "${isoConfig.system.build.isoImage}/iso/${isoConfig.image.fileName or isoConfig.isoImage.isoName}";
  startCommand = "${pkgs.qemu_test}/bin/qemu-kvm -cpu max -m 1536 -cdrom ${iso}";
in
pkgs.testers.runNixOSTest {
  name = "iso-boot";
  nodes = { };
  testScript = t.prelude + ''
    machine = create_machine("${startCommand}")
    machine.start()
    start = time.monotonic()
    machine.wait_for_unit("multi-user.target")
    boot_sec = round(time.monotonic() - start, 1)

    with subtest("live system: squashfs root from the CD"):
        machine.succeed("findmnt -no FSTYPE /nix/.ro-store | grep -qx squashfs")
        machine.succeed("test -e /dev/disk/by-label/PANDAOS-LIVE")

    with subtest("panda-install is on the ISO and runs"):
        out = machine.succeed("panda-install --help 2>&1")
        assert "usage" in out.lower() and "--disk" in out, out

    emit({"test": "iso-boot", "isoBootSec": boot_sec})
    machine.shutdown()
  '';
}
