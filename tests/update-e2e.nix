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
# Signatures: release commits are SSH-signed with a key generated at build
# time whose public half is pandaos.updates.allowedSignersFile. An unsigned
# commit, one signed by another key, and a move of the branch back to an older
# signed release are each refused, with flake.lock and the system unchanged.
# Staged mode's unit (panda-update-stage) builds the next release without
# switching or touching flake.lock.
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
  # Test-only signing keys: `good` is allowed, `evil` is not.
  keys = pkgs.runCommand "update-e2e-release-keys" { nativeBuildInputs = [ pkgs.openssh ]; } ''
    mkdir $out
    ssh-keygen -q -t ed25519 -N "" -C release -f $out/good
    ssh-keygen -q -t ed25519 -N "" -C evil -f $out/evil
    echo "release@pandaos.test $(cat $out/good.pub)" >$out/allowed_signers
  '';
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
      pandaos.updates.allowedSignersFile = "${keys}/allowed_signers";
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


      def release(target, msg, key="good"):
          # key: "good" (allowed), "evil" (not allowed) or None (unsigned).
          sign = f"-c gpg.format=ssh -c user.signingkey=/root/{key} commit -S" if key else "commit"
          machine.succeed(f"echo {target} > {rel}/target", f"{git} add -A", f"{git} {sign} -qm '{msg}'")
          return machine.succeed(f"{git} rev-parse HEAD").strip()


      def reset_release(rev):
          machine.succeed(f"{git} reset -q --hard {rev}")


      def refused(why, lock_rev, system):
          rc, log = upgrade()
          assert rc != 0, f"accepted a release that should be refused ({why}): {log}"
          assert "REFUSING" in log and why in log, log
          assert locked_rev() == lock_rev, (locked_rev(), lock_rev)
          assert current() == system, current()
          assert machine.succeed("cut -d' ' -f1 /var/lib/pandaos/update-verify").strip() == "refused"
          status = machine.succeed("panda-update status")
          assert "last release check: refused" in status, status


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
          for k in ["good", "evil"]:
              machine.succeed(f"install -m 0600 ${keys}/{k} /root/{k}")
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

      with subtest("an unsigned release commit is refused"):
          release("${gen "r2"}", "unsigned", key=None)
          refused("has no SSH signature", r1, base)

      with subtest("a release signed by a key not in allowedSigners is refused"):
          reset_release(r1)
          release("${gen "r2"}", "wrong key", key="evil")
          refused("not from a key in pandaos.updates.allowedSigners", r1, base)
          reset_release(r1)

      with subtest("a new signed release commit: nixos-upgrade verifies, bumps the lock, switches, and is healthy"):
          r2 = release("${gen "r2"}", "release 2")
          rc, log = upgrade()
          assert rc == 0, log
          assert locked_rev() == r2, (locked_rev(), r2)
          assert current() == "${gen "r2"}", current()
          assert machine.succeed("cat /etc/pandaos-test-gen").strip() == "release-2"
          assert machine.succeed("readlink -f /nix/var/nix/profiles/system").strip() == current()
          assert "is healthy" in log, log
          assert "is signed by release@pandaos.test" in log, log

      with subtest("no new release: the update is a no-op"):
          rc, log = upgrade()
          assert rc == 0 and "system unchanged" in log, log
          assert locked_rev() == r2

      with subtest("staged: the next release is verified and built, not switched to"):
          r3 = release("${gen "r3"}", "release 3")
          lock_before = machine.succeed("cat /etc/pandaos/flake.lock")
          machine.succeed("systemctl start panda-update-stage.service")
          assert machine.succeed("cat /etc/pandaos/flake.lock") == lock_before
          assert current() == "${gen "r2"}", current()
          staged = machine.succeed("cat /var/lib/pandaos/update-staged").split()
          assert staged[0] == r3, staged
          assert "staged: " + r3 in machine.succeed("panda-update status")

      with subtest("the next release is picked up the same way"):
          rc, log = upgrade()
          assert rc == 0, log
          assert locked_rev() == r3 and current() == "${gen "r3"}", (locked_rev(), current())
          assert "is healthy" in log, log
          machine.fail("test -e /var/lib/pandaos/update-staged")

      with subtest("moving the branch back to an older signed release is refused"):
          reset_release(r2)
          refused("does not descend from the running release", r3, "${gen "r3"}")
    '';
}
