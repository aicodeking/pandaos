# Host-side, no VM: closure sizes of what panda-install actually puts on disk
# (nixosConfigurations.*, no test instrumentation), checked against
# budgets.json, plus `updateBytes` for a panda-mem-only update.
#
# updateBytes is the NAR size of the store paths in the new system that the
# old one lacks, i.e. what a machine on `old` substitutes to switch. Real
# downloads are compressed NARs, so this is an upper bound on the transfer. The
# update simulated here is "PandaOS ships a new panda-mem"; a nixpkgs bump would
# pull far more and is not measured.
{ pkgs, self }:
let
  inherit (pkgs) lib;
  toplevel = name: self.nixosConfigurations.${name}.config.system.build.toplevel;
  info = top: pkgs.closureInfo { rootPaths = [ top ]; };

  headless = self.nixosConfigurations.headless;
  updatedHeadless =
    (self.lib.mkPandaSystem {
      profile = "headless";
      modules = [
        ../nixos/installer/target.nix
        {
          services.panda-mem.package = lib.mkForce (
            headless.config.services.panda-mem.package.overrideAttrs (old: {
              postInstall = (old.postInstall or "") + ''
                mkdir -p $out/share/panda-mem
                echo "simulated update" > $out/share/panda-mem/update-marker
              '';
            })
          );
        }
      ];
    }).config.system.build.toplevel;
in
pkgs.runCommand "pandaos-closures" { nativeBuildInputs = [ pkgs.python3 ]; } ''
  mkdir -p $out
  python3 ${./closures.py} --budgets ${./budgets.json} --out $out/metrics.json \
    --system headless=${info (toplevel "headless")} \
    --system desktop=${info (toplevel "desktop")} \
    --update headless=${info (toplevel "headless")}:${info updatedHeadless}
''
