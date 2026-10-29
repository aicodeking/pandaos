# End-to-end daily update through the REAL flake path, offline:
#   a local git repo with a `release` branch stands in for
#   github:AICodeKing/pandaos/release; /etc/pandaos is the flake panda-install
#   writes (input `pandaos`, `pandaos.lib.mkPandaSystem`), locked to the first
#   release commit. Pushing a new commit to `release` and starting
#   nixos-upgrade.service runs the unmodified switchCommand
#   (`nixos-rebuild switch --refresh --flake /etc/pandaos#pandaos
#   --update-input pandaos`): fetch the branch, bump flake.lock, build,
#   nix-env --set, switch-to-configuration, then panda-health.
#
# One substitution: the release repo's flake is a stand-in whose
# lib.mkPandaSystem returns a prebuilt toplevel (a store path string with
# context) instead of evaluating PandaOS's modules, because evaluating and
# building NixOS inside the test VM needs the whole build closure offline and
# several GB of RAM. So NOT covered: evaluating PandaOS inside the VM (CI
# evaluates and builds the same configurations outside it), GitHub, and
# substituting from a cache.
{ pkgs, self, t }:
let
  inherit (pkgs) lib;
in
pkgs.testers.runNixOSTest {
  name = "update-e2e";
  nodes.machine =
    { config, ... }:
    {
      imports = [
        self.nixosModules.default
        self.nixosModules.headless
        (t.nodeName "machine")
        t.noFirstboot
      ];
      virtualisation.memorySize = 2048;
      # Flake sources are copied into the store.
      virtualisation.writableStore = true;
      # Offline: nothing to substitute from.
      nix.settings.substituters = lib.mkForce [ ];
      # Only the test starts updates.
      systemd.timers.nixos-upgrade.wantedBy = lib.mkForce [ ];
      # The release generations (specialisations) and nixos-rebuild are in
      # the VM's store and Nix DB, so the build is a no-op.
      virtualisation.additionalPaths = [
        config.system.build.toplevel
        config.system.build.nixos-rebuild
      ];
      environment.etc."pandaos-test-gen".text = lib.mkDefault "base";
      specialisation.r2.configuration.environment.etc."pandaos-test-gen".text = "release-2";
      specialisation.r3.configuration.environment.etc."pandaos-test-gen".text = "release-3";
    };

  testScript =
    { nodes, ... }:
    let
      m = nodes.machine;
      gen = name: m.specialisation.${name}.configuration.system.build.toplevel;
      # The release branch's flake: same interface as PandaOS's flake.nix.
      releaseFlake = pkgs.writeText "release-flake.nix" ''
        {
          outputs =
            { self }:
            let
              line = f: builtins.head (builtins.match "([^\n]*)\n?" (builtins.readFile f));
              storePath = p: builtins.appendContext p { ''${p} = { path = true; }; };
            in
            {
              lib.mkPandaSystem =
                { profile, modules ? [ ], system ? "x86_64-linux" }:
                {
                  config.system.build = {
                    toplevel = storePath (line ./target);
                    nixos-rebuild = storePath "${m.system.build.nixos-rebuild}";
                  };
                };
            };
        }
      '';
      # What panda-install writes, with the input pointing at the local repo.
      localFlake = pkgs.writeText "pandaos-flake.nix" ''
        {
          inputs.pandaos.url = "git+file:///var/lib/pandaos-release?ref=release";
          outputs =
            { pandaos, ... }:
            {
              nixosConfigurations.pandaos = pandaos.lib.mkPandaSystem {
                profile = "headless";
                modules = [ ./local.nix ];
              };
            };
        }
      '';
    in
    t.prelude
    + ''
      rel = "/var/lib/pandaos-release"
      git = f"git -C {rel} -c user.name=ci -c user.email=ci@example.invalid"


      def current():
          return machine.succeed("readlink -f /run/current-system").strip()


      def release(target, msg):
          machine.succeed(f"echo {target} > {rel}/target", f"{git} add -A", f"{git} commit -qm '{msg}'")
          return machine.succeed(f"{git} rev-parse HEAD").strip()


      def locked_rev():
          lock = json.loads(machine.succeed("cat /etc/pandaos/flake.lock"))
          return lock["nodes"]["pandaos"]["locked"]["rev"]


      def upgrade():
          since = machine.succeed("date +%s").strip()
          rc, out = machine.execute("systemctl start nixos-upgrade.service 2>&1")
          log = machine.succeed(f"journalctl -o cat --since @{since} -u nixos-upgrade.service")
          print(log)
          return rc, log


      machine.wait_for_unit("multi-user.target")
      base = current()

      with subtest("release repo and /etc/pandaos, locked to the installed release"):
          machine.succeed(f"mkdir -p {rel} && git -C {rel} init -q -b release")
          machine.succeed(f"cp ${releaseFlake} {rel}/flake.nix")
          r1 = release(base, "release 1: the installed system")
          machine.succeed(
              "mkdir -p /etc/pandaos",
              "cp ${localFlake} /etc/pandaos/flake.nix",
              "echo '{ }' > /etc/pandaos/local.nix",
              "nix flake lock /etc/pandaos",
          )
          assert locked_rev() == r1, (locked_rev(), r1)

      with subtest("a new release commit: nixos-upgrade bumps the lock, switches, and is healthy"):
          r2 = release("${gen "r2"}", "release 2")
          rc, log = upgrade()
          assert rc == 0, log
          assert locked_rev() == r2, (locked_rev(), r2)
          assert current() == "${gen "r2"}", current()
          assert machine.succeed("cat /etc/pandaos-test-gen").strip() == "release-2"
          assert machine.succeed("readlink -f /nix/var/nix/profiles/system").strip() == current()
          assert "is healthy" in log, log

      with subtest("no new release: the update is a no-op"):
          rc, log = upgrade()
          assert rc == 0 and "system unchanged" in log, log
          assert locked_rev() == r2

      with subtest("the next release is picked up the same way"):
          r3 = release("${gen "r3"}", "release 3")
          rc, log = upgrade()
          assert rc == 0, log
          assert locked_rev() == r3 and current() == "${gen "r3"}", (locked_rev(), current())
          assert "is healthy" in log, log
    '';
}
