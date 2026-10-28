# Optional PandaOS binary cache.
#
# Honest status: there is no PandaOS cache yet. cache.nixos.org has all of
# nixpkgs, but not PandaOS's own packages (panda-agentd, panda-mem, ...), so
# today an update that changes them builds them locally (a few minutes of
# Go builds on a small box). The project OWNER has to create a cache (Cachix
# or any Nix binary cache) and have CI push release builds to it. Then set
# both options below, here or in the installer's local.nix.
{ config, lib, ... }:
let
  cfg = config.pandaos.binaryCache;
in
{
  options.pandaos.binaryCache = {
    url = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "https://pandaos.cachix.org";
      description = "Extra binary cache for PandaOS packages, tried before cache.nixos.org.";
    };
    publicKey = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "pandaos.cachix.org-1:AAAA...=";
      description = "Signing key of {option}`pandaos.binaryCache.url`.";
    };
  };

  config = lib.mkIf (cfg.url != null) {
    assertions = [
      {
        assertion = cfg.publicKey != null;
        message = "pandaos.binaryCache.url is set but pandaos.binaryCache.publicKey is not; Nix would reject every path from it.";
      }
    ];
    # Both lists merge with the NixOS defaults (cache.nixos.org stays last).
    nix.settings = {
      substituters = [ cfg.url ];
      trusted-public-keys = lib.optional (cfg.publicKey != null) cfg.publicKey;
    };
  };
}
