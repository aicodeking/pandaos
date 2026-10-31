# The PandaOS desktop system (mkPandaSystem profile "desktop" + target.nix)
# as a compressed qcow2 for cubench, built like bench/compare/pandaos-image.nix:
#
#   BENCH_SSH_PUBKEY="$(cat key.pub)" nix build --impure -L \
#     --expr "import ./bench/cubench/pandaos-desktop-image.nix { flake = builtins.getFlake \"git+file://$PWD\"; }"
#
# The shipped desktop already has Mousepad, PCManFM, the AT-SPI bus and
# panda-cu in attach mode (the `--tools panda` column calls it; the generic
# tools never do). On top of it, for the benchmark only:
#   - autologin of `panda` into the labwc session, no idle lock;
#   - Firefox with the same policies as the other OSes, also set as
#     panda-cu's browser; python3 for the task form server;
#   - ext4 root + ESP disk layout, wizard off, ssh key for `panda`, as in compare.
{ flake, pubkey ? builtins.getEnv "BENCH_SSH_PUBKEY", system ? "x86_64-linux" }:
assert pubkey != "" || throw "set BENCH_SSH_PUBKEY (and pass --impure)";
let
  inherit (flake.inputs) nixpkgs;
  sys = flake.lib.mkPandaSystem {
    profile = "desktop";
    inherit system;
    modules = [
      "${flake}/nixos/installer/target.nix"
      ({ lib, pkgs, ... }: {
        fileSystems = lib.mkForce {
          "/" = {
            device = "/dev/disk/by-label/nixos";
            fsType = "ext4";
            autoResize = true;
          };
          "/boot" = {
            device = "/dev/disk/by-label/ESP";
            fsType = "vfat";
            options = [ "fmask=0077" "dmask=0077" ];
          };
        };
        boot.growPartition = true;
        pandaos.firstboot.enable = false;
        pandaos.desktop.autologinUser = "panda";
        pandaos.desktop.autoLock = false;
        services.panda-cu.browser = pkgs.firefox;
        users.users.panda.openssh.authorizedKeys.keys = [ pubkey ];

        programs.dconf.enable = true;
        programs.firefox = {
          enable = true;
          policies = {
            DisableTelemetry = true;
            DisableFirefoxStudies = true;
            DontCheckDefaultBrowser = true;
            OverrideFirstRunPage = "";
            OverridePostUpdatePage = "";
            NoDefaultBookmarks = true;
            UserMessaging = {
              WhatsNew = false;
              ExtensionRecommendations = false;
              FeatureRecommendations = false;
              UrlbarInterventions = false;
              SkipOnboarding = true;
              MoreFromMozilla = false;
            };
            Preferences = {
              "browser.sessionstore.resume_from_crash" = { Value = false; Status = "default"; };
              "browser.aboutwelcome.enabled" = { Value = false; Status = "default"; };
              "datareporting.policy.dataSubmissionPolicyBypassNotification" = { Value = true; Status = "default"; };
            };
          };
        };
        environment.systemPackages = [ pkgs.python3 ];
      })
    ];
  };
in
import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
  inherit (sys) config pkgs;
  inherit (nixpkgs) lib;
  format = "qcow2-compressed";
  partitionTableType = "efi";
  diskSize = "auto";
  additionalSpace = "2048M";
  copyChannel = false;
}
