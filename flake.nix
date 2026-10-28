{
  description = "PandaOS: a lightweight, agent-focused Linux built on NixOS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      # Add "aarch64-linux" here to build for ARM too.
      systems = [ "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;

      mkSystem =
        system: modules:
        nixpkgs.lib.nixosSystem {
          modules = [
            { nixpkgs.hostPlatform = system; }
            self.nixosModules.default
          ] ++ modules;
        };

      isoModules = [
        "${nixpkgs}/nixos/modules/installer/cd-dvd/installation-cd-minimal.nix"
        ./nixos/modules/iso.nix
      ];
    in
    {
      nixosModules = {
        default = {
          imports = [
            ./nixos/modules/base.nix
            ./components/agentd/module.nix
            ./components/mem/module.nix
          ];
        };
        headless = ./nixos/profiles/headless.nix;
        desktop = ./nixos/profiles/desktop.nix;
      };

      nixosConfigurations = {
        headless = mkSystem "x86_64-linux" [
          self.nixosModules.headless
          ./nixos/modules/generic-disk.nix
        ];
        desktop = mkSystem "x86_64-linux" [
          self.nixosModules.desktop
          ./nixos/modules/generic-disk.nix
        ];
      };

      packages = forAllSystems (system: {
        iso-headless =
          (mkSystem system ([ self.nixosModules.headless ] ++ isoModules)).config.system.build.isoImage;
        iso-desktop =
          (mkSystem system ([ self.nixosModules.desktop ] ++ isoModules)).config.system.build.isoImage;
        vm-headless =
          (mkSystem system [
            self.nixosModules.headless
            ./nixos/modules/generic-disk.nix
          ]).config.system.build.vm;
      });

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
