# Evaluation-only checks of the boat host hardening (no VM): the server
# profile's options, units and assertions, evaluated the way an installed box
# evaluates them (self.lib.mkPandaSystem). A failing check aborts evaluation
# with the list of failures; success builds a small report.
#
#   - boat-api has a MemoryMax and no -900 OOMScoreAdjust;
#   - boat-api gets -vm-api-url (guests get BOAT_URL + token, not only BOAT_ID);
#   - node_exporter listens on 127.0.0.1;
#   - untrustedTenants: vmDomain/desktopDomain on the API's registrable domain
#     and a missing gateway.dnsProvider are errors, each with an escape hatch;
#   - untrustedTenants defaults updates to `staged`; signed releases are
#     required, and nixos-rebuild no longer bumps the input unverified;
#   - the maintenance window unit exists only when enabled.
{ pkgs, self }:
let
  inherit (pkgs) lib;
  eval =
    module:
    (self.lib.mkPandaSystem {
      profile = "server";
      modules = [ module ];
    }).config;

  # Only this module's assertions (an eval without hardware config fails
  # unrelated ones, e.g. the root file system).
  boatFailures =
    config:
    map (a: a.message) (
      lib.filter (a: !a.assertion && lib.hasInfix "services.boat" a.message) config.assertions
    );
  failsWith = needle: config: lib.any (lib.hasInfix needle) (boatFailures config);

  base = eval { };
  untrustedSameSite = eval {
    services.boat = {
      untrustedTenants = true;
      domain = "boat.example.com";
    };
  };
  untrustedAccepted = eval {
    services.boat = {
      untrustedTenants = true;
      domain = "boat.example.com";
      untrustedTenantsAcceptRisks = [
        "shared-registrable-domain"
        "on-demand-tls"
      ];
      gateway.allowOnDemandTLS = true;
    };
  };
  untrustedClean = eval {
    services.boat = {
      untrustedTenants = true;
      domain = "boat.example.com";
      vmDomain = "boatvms.example.net";
      desktopDomain = "boatdesk.example.org";
      gateway.dnsProvider = "cloudflare {env.CF_API_TOKEN}";
    };
  };
  trustedSameSite = eval {
    services.boat = {
      domain = "boat.example.com";
      gateway.allowOnDemandTLS = true;
    };
  };
  withMaintenance = eval {
    services.boat.maintenance.reboot = true;
    pandaos.updates.allowedSigners = [ "release@pandaos ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKey" ];
  };
  custom = eval {
    services.boat = {
      vmApiUrl = null;
      apiMemoryMax = "512M";
    };
  };

  api = c: c.systemd.services.boat-api.serviceConfig;

  checks = {
    "boat-api MemoryMax defaults to 2G" = (api base).MemoryMax == "2G";
    "boat-api MemoryMax follows apiMemoryMax" = (api custom).MemoryMax == "512M";
    "boat-api is not OOM-protected at -900" = ((api base).OOMScoreAdjust or 0) > -900;
    "boat-api gets -vm-api-url from the public URL" =
      lib.hasInfix "-vm-api-url" (api base).ExecStart
      && lib.hasInfix "https://api.boat.localhost" (api base).ExecStart;
    "vmApiUrl = null drops -vm-api-url" = !(lib.hasInfix "-vm-api-url" (api custom).ExecStart);
    "node_exporter binds 127.0.0.1" = base.services.prometheus.exporters.node.listenAddress == "127.0.0.1";

    "untrusted + same-site vmDomain is an error" = failsWith "untrustedTenants refuses tenant content" untrustedSameSite;
    "same-site error names the escape hatch" = failsWith ''"shared-registrable-domain"'' untrustedSameSite;
    "untrusted without dnsProvider is an error" = failsWith "needs services.boat.gateway.dnsProvider" untrustedSameSite;
    "untrusted: accepted risks evaluate" = boatFailures untrustedAccepted == [ ];
    "untrusted: separate domains + DNS-01 evaluate" = boatFailures untrustedClean == [ ];
    "trusted same-site stays a warning" =
      boatFailures trustedSameSite == [ ]
      && lib.any (lib.hasInfix "shares a registrable domain") trustedSameSite.warnings;

    "updates: automatic by default" = base.pandaos.updates.mode == "automatic";
    "updates: untrustedTenants defaults to staged" = untrustedClean.pandaos.updates.mode == "staged";
    "updates: staged mode has no auto-switch timer" =
      untrustedClean.systemd.timers.nixos-upgrade.wantedBy == [ ]
      && untrustedClean.systemd.timers ? panda-update-stage;
    "updates: signed releases required by default" = base.pandaos.updates.requireSignedRelease;
    "updates: switch verifies before nixos-rebuild" =
      lib.hasInfix "panda-update-verify" base.pandaos.updates.switchCommand;
    "updates: nixos-rebuild does not bump the input unverified" =
      !(lib.elem "--update-input" base.system.autoUpgrade.flags);
    "updates: empty allowedSigners warns" =
      lib.any (lib.hasInfix "allowedSigners is empty") base.warnings
      && !(lib.any (lib.hasInfix "allowedSigners is empty") withMaintenance.warnings);

    "maintenance window off by default" = !(base.systemd.timers ? boat-maintenance-reboot);
    "maintenance window on when enabled" =
      withMaintenance.systemd.timers ? boat-maintenance-reboot
      && withMaintenance.services.boat.maintenance.action == "auto";
  };

  failed = lib.attrNames (lib.filterAttrs (_: ok: !ok) checks);
in
if failed != [ ] then
  throw "boat-eval: failed checks:\n  ${lib.concatStringsSep "\n  " failed}"
else
  pkgs.writeText "boat-eval-report" (
    lib.concatMapStrings (n: "ok  ${n}\n") (lib.attrNames checks)
  )
