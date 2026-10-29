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

      # The input the installed /etc/pandaos flake uses (updates.nix follows it).
      flakeUrl = "github:AICodeKing/pandaos/release";
      revision = self.rev or self.dirtyRev or "unknown";
      version = self.shortRev or self.dirtyShortRev or "dev";

      # flake.lock (v7) that panda-install writes next to the local flake,
      # pinning `pandaos` (and its nixpkgs) to exactly this source, offline.
      # Only for a clean tree: a dirty revision does not exist on GitHub, so
      # then the first online rebuild creates the lock instead.
      github = owner: repo: { type = "github"; inherit owner repo; };
      flakeLock =
        if !(self ? rev) then
          null
        else
          {
            version = 7;
            root = "root";
            nodes = {
              root.inputs.pandaos = "pandaos";
              pandaos = {
                inputs.nixpkgs = "nixpkgs";
                locked = github "AICodeKing" "pandaos" // {
                  inherit (self) rev narHash lastModified;
                };
                original = github "AICodeKing" "pandaos" // { ref = "release"; };
              };
              nixpkgs = {
                locked = github "NixOS" "nixpkgs" // {
                  inherit (self.inputs.nixpkgs) rev narHash lastModified;
                };
                original = github "NixOS" "nixpkgs" // { ref = "nixos-26.05"; };
              };
            };
          };

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
            ./nixos/modules/firmware.nix
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
          ./nixos/modules/firmware.nix
          {
            pandaos.installer = {
              inherit flakeUrl revision;
              inherit flakeLock;
              defaultProfile = profile;
              systems = lib.genAttrs targets (installable system);
            };
            # iso-image.nix sets this (it is what names the .iso file).
            image.baseName = lib.mkForce "pandaos-${version}-${profile}-${system}";
          }
        ]).config.system.build.isoImage;
    in
    {
      lib = { inherit mkPandaSystem; };

      nixosModules = {
        default = {
          system.nixos = {
            distroName = "PandaOS";
            distroId = "pandaos";
          };
          imports = [
            ./nixos/modules/base.nix
            ./components/agentd/module.nix
            ./components/mem/module.nix
            ./components/browser/module.nix
            ./components/pandacode-host/module.nix
            ./components/agent-env/module.nix
            ./components/checkpoint/module.nix
            ./components/sysmcp/module.nix
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
            inherit flakeUrl revision flakeLock;
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
