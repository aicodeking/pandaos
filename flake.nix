{
  description = "PandaOS: a lightweight, agent-focused Linux built on NixOS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;

      # Add "aarch64-linux" here to build for ARM too.
      systems = [ "x86_64-linux" ];
      forAllSystems = lib.genAttrs systems;

      flakeUrl = "github:AICodeKing/pandaos";

      mkNixos =
        system: modules:
        lib.nixosSystem {
          modules = [
            { nixpkgs.hostPlatform = lib.mkDefault system; }
            self.nixosModules.default
          ]
          ++ modules;
        };

      # An installed PandaOS: profile ("headless" or "desktop") on the
      # generic-disk layout, plus extra modules. The /etc/pandaos flake that
      # panda-install writes calls this with hardware-configuration.nix and
      # local.nix.
      mkPandaSystem =
        {
          profile,
          modules ? [ ],
          system ? "x86_64-linux",
        }:
        mkNixos system (
          [
            self.nixosModules.${profile}
            ./nixos/modules/generic-disk.nix
          ]
          ++ modules
        );

      # What panda-install copies to disk offline (see nixos/installer).
      installable =
        system: profile:
        (mkPandaSystem {
          inherit profile system;
          modules = [ ./nixos/installer/target.nix ];
        }).config.system.build.toplevel;

      # Live ISO running `profile`, able to install each of `targets` offline.
      mkIso =
        system: profile: targets:
        (mkNixos system [
          self.nixosModules.${profile}
          ./nixos/modules/iso.nix
          {
            pandaos.installer = {
              inherit flakeUrl;
              systems = lib.genAttrs targets (installable system);
            };
          }
        ]).config.system.build.isoImage;
    in
    {
      lib = { inherit mkPandaSystem; };

      nixosModules = {
        default = {
          imports = [
            ./nixos/modules/base.nix
            ./components/agentd/module.nix
            ./components/mem/module.nix
            ./components/browser/module.nix
          ];
        };
        headless = ./nixos/profiles/headless.nix;
        desktop = ./nixos/profiles/desktop.nix;
      };

      nixosConfigurations = {
        headless = mkPandaSystem {
          profile = "headless";
          modules = [ ./nixos/installer/target.nix ];
        };
        desktop = mkPandaSystem {
          profile = "desktop";
          modules = [ ./nixos/installer/target.nix ];
        };
      };

      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          iso-headless = mkIso system "headless" [ "headless" ];
          iso-desktop = mkIso system "desktop" [
            "headless"
            "desktop"
          ];
          vm-headless = (mkPandaSystem { profile = "headless"; inherit system; }).config.system.build.vm;
          panda-install = pkgs.callPackage ./nixos/installer/package.nix {
            inherit flakeUrl;
            systems = lib.genAttrs [ "headless" "desktop" ] (installable system);
          };
        }
      );

      checks = forAllSystems (
        system:
        import ./tests {
          pkgs = nixpkgs.legacyPackages.${system};
          inherit self;
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
