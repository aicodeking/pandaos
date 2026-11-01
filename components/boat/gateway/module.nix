# boat edge: Caddy terminates TLS, boat-gateway routes by host, sshd's
# `jump` user forwards ssh into VMs. Imported by ../module.nix.
#
#   api.<domain>                  -> boat-api (127.0.0.1:apiPort); /internal/* is never public
#   desktop.<domain>/<vmid>?sig   -> 302 to <vmid>.desktop.<domain> (nothing served or set here)
#   <vmid>.desktop.<domain>       -> boat-gateway -> VM desktop (signed link -> host-only cookie)
#   <port>-<vmid>.<vmDomain>      -> boat-gateway -> VM ip:port; private unless the port is public
#   ssh -J jump@ssh.<domain>:2222 dev@<vm ip>   (permitopen per key, written by boat-api)
#
# Each desktop and each port route is its own origin. boat-gateway re-reads
# routes.json on change, so Caddy is never reloaded for VM or port changes.
# TLS for the two wildcards: DNS-01 when gateway.dnsProvider is set, else
# per-host HTTP-01 certs on demand, only for hosts boat-gateway confirms
# (its ask endpoint).
{ config, lib, pkgs, ... }:
let
  boat = config.services.boat;
  cfg = boat.gateway;
  inherit (lib) mkOption types;
  d = boat.domain;
  inherit (boat) vmDomain desktopDomain;
  tlsBlock = ''
    tls {
      ${if cfg.dnsProvider == null then "on_demand" else "dns ${cfg.dnsProvider}"}
    }
  '';
  isLocal = lib.hasSuffix ".localhost" d;
  # The API's own paths; everything else is the dashboard, when there is one.
  apiPaths = [ "/v1/*" "/healthz" ];
  apiProxy = "reverse_proxy 127.0.0.1:${toString boat.apiPort}";
  dashboardSite =
    if cfg.dashboardDir == null then
      apiProxy
    else
      ''
        root * ${cfg.dashboardDir}
        header {
          Content-Security-Policy "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
          X-Content-Type-Options nosniff
          Referrer-Policy no-referrer
        }
        try_files {path} /index.html
        file_server
      '';
  listen = "127.0.0.1:8742";
  askListen = "127.0.0.1:8743";

  jumpKeys = pkgs.writeShellScript "boat-jump-keys" ''
    PATH=${lib.makeBinPath [ pkgs.gnugrep pkgs.gawk pkgs.jq pkgs.coreutils ]}
    ${builtins.readFile ./jump-keys.sh}
  '';
in
{
  options.services.boat.gateway = {
    enable = lib.mkEnableOption "the boat HTTPS gateway (Caddy + boat-gateway)" // { default = true; };
    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./gateway/package.nix { }";
    };
    caddyPackage = mkOption {
      type = types.package;
      default = pkgs.caddy;
      defaultText = lib.literalExpression "pkgs.caddy";
      example = lib.literalExpression ''
        pkgs.caddy.withPlugins {
          plugins = [ "github.com/caddy-dns/cloudflare@v0.2.1" ];
          hash = "sha256-...";
        }
      '';
      description = "Caddy build; must include the DNS plugin named in {option}`dnsProvider`.";
    };
    dnsProvider = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "cloudflare {env.CF_API_TOKEN}";
      description = ''
        Caddy `dns` directive body for the DNS-01 wildcard certificates of
        `*.<vmDomain>` and `*.<desktopDomain>` (the provider must manage both zones). Null: per-host HTTP-01 certificates on demand instead
        (first request to a new port URL waits for issuance; Let's Encrypt
        rate limits apply, see docs/boat.md).
      '';
    };
    allowOnDemandTLS = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Permit per-host HTTP-01 certificates when {option}`dnsProvider` is null. Every new
        port route or desktop then costs a certificate, so a tenant can burn the domain's
        Let's Encrypt rate limit, and every name lands in Certificate Transparency logs.
        Production should use DNS-01.
      '';
    };
    dashboardDir = mkOption {
      type = types.nullOr types.path;
      default = if builtins.pathExists ../dashboard/index.html then ../dashboard else null;
      defaultText = lib.literalExpression "../dashboard, when it has an index.html";
      description = ''
        Static web dashboard, served read-only from the Nix store at `https://api.<domain>/`
        (same origin as the API, which it calls with the user's key). Null: no dashboard.
      '';
    };
    environmentFile = mkOption {
      type = types.nullOr types.path;
      default = null;
      example = "/var/lib/secrets/caddy-dns.env";
      description = "EnvironmentFile for Caddy (DNS API tokens). Keep it out of the Nix store.";
    };
  };

  config = lib.mkIf boat.enable (lib.mkMerge [
    (lib.mkIf cfg.enable {
      assertions = [
        {
          assertion = cfg.dnsProvider != null || cfg.allowOnDemandTLS || isLocal;
          message = ''
            services.boat.gateway: set dnsProvider (DNS-01, one wildcard certificate; recommended) or, knowing
            that a tenant can then burn your Let's Encrypt rate limit, allowOnDemandTLS = true. See docs/boat.md.'';
        }
      ];
      warnings = lib.optional (cfg.dnsProvider == null && !isLocal) ''
        services.boat.gateway has no dnsProvider, so every port route and desktop gets its own on-demand
        HTTP-01 certificate: a tenant can exhaust ${d}'s Let's Encrypt rate limit (TLS for new routes then
        fails for everyone) and all names are published in CT logs. Use DNS-01 in production; see docs/boat.md.'';

      services.caddy = {
        enable = true;
        package = cfg.caddyPackage;
        email = boat.acmeEmail;
        inherit (cfg) environmentFile;
        # skip_install_trust: Caddy's local CA (only *.localhost) never touches
        # this system's trust store.
        globalConfig = ''
          skip_install_trust
        '' + lib.optionalString (cfg.dnsProvider == null) ''
          on_demand_tls {
            ask http://${askListen}/ask
          }
        '';
        virtualHosts = {
          # route: these handlers run in this order, first match wins.
          "api.${d}".extraConfig = ''
            route {
              handle /internal/* {
                respond 404
              }
              ${lib.concatMapStrings (p: "handle ${p} {\n  ${apiProxy}\n}\n") apiPaths}
              handle {
                ${dashboardSite}
              }
            }
          '';
          ${desktopDomain}.extraConfig = ''
            reverse_proxy ${listen}
          '';
          "*.${desktopDomain}".extraConfig = tlsBlock + ''
            header Referrer-Policy no-referrer
            reverse_proxy ${listen}
          '';
          "*.${vmDomain}".extraConfig = tlsBlock + ''
            reverse_proxy ${listen}
          '';
        };
      };
      users.users.boat-gateway = {
        isSystemUser = true;
        group = "boat-gateway";
        description = "boat HTTP router";
      };
      users.groups.boat-gateway = { };

      networking.firewall.allowedTCPPorts = [ 80 443 ];
      # HTTP/3.
      networking.firewall.allowedUDPPorts = [ 443 ];

      systemd.services.boat-gateway = {
        description = "boat HTTP router (VM ports + desktop links)";
        wantedBy = [ "multi-user.target" ];
        after = [ "boat-secrets.service" ];
        requires = [ "boat-secrets.service" ];
        before = [ "caddy.service" ];
        unitConfig.RequiresMountsFor = boat.dataDir;
        serviceConfig = {
          ExecStart = lib.escapeShellArgs [
            (lib.getExe cfg.package)
            "-domain" d
            "-vm-domain" vmDomain
            "-desktop-domain" desktopDomain
            "-listen" listen
            "-ask-listen" askListen
            "-routes" "/run/boat/routes.json"
            "-desktop-key" "%d/boat-desktop-key"
          ];
          # Faces the internet: its own uid, no access to boatd's socket or to
          # boat-api's data. It reads routes.json and this one credential.
          User = "boat-gateway";
          Group = "boat-gateway";
          LoadCredentialEncrypted = "boat-desktop-key:${boat.dataDir}/secrets/boat-desktop-key.cred";
          Restart = "always";
          RestartSec = 2;
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          NoNewPrivileges = true;
          ProtectKernelTunables = true;
          ProtectKernelModules = true;
          ProtectKernelLogs = true;
          ProtectControlGroups = true;
          ProtectClock = true;
          ProtectHostname = true;
          RestrictNamespaces = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          MemoryDenyWriteExecute = true;
          SystemCallArchitectures = "native";
          SystemCallFilter = [ "@system-service" "~@privileged" ];
          CapabilityBoundingSet = "";
          RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        };
      };
    })

    (lib.mkIf boat.sshGateway.enable {
      # ProxyJump host. The jump user has no password, no shell, no tty, no
      # agent/X11/socket forwarding; only direct-tcpip to the VM addresses its
      # key's permitopen= lists. Port sshGateway.port admits only this user.
      users.users.jump = {
        isSystemUser = true;
        group = "jump";
        description = "boat ssh jump host (ProxyJump into VMs)";
      };
      users.groups.jump = { };

      # sshd requires a root-owned AuthorizedKeysCommand on a path with no
      # group/world-writable parent, which /nix/store (1775) is not: copy to /etc.
      environment.etc."boat/jump-keys" = {
        source = jumpKeys;
        mode = "0555";
      };

      # 22 stays for admins; the openssh module opens both in the firewall.
      services.openssh.ports = [ 22 boat.sshGateway.port ];
      services.openssh.extraConfig = ''
        Match LocalPort ${toString boat.sshGateway.port}
          AllowUsers jump
        Match User jump
          AuthorizedKeysFile none
          AuthorizedKeysCommand /etc/boat/jump-keys ${boat.dataDir}/ssh/jump_authorized_keys /run/boat/routes.json
          AuthorizedKeysCommandUser boat
          AllowTcpForwarding local
          AllowAgentForwarding no
          AllowStreamLocalForwarding no
          X11Forwarding no
          PermitTTY no
          PermitTunnel no
          GatewayPorts no
          ForceCommand ${pkgs.coreutils}/bin/false
      '';
    })
  ]);
}
