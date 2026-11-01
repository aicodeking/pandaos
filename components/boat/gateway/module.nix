# boat edge: Caddy terminates TLS, boat-gateway routes by host, sshd's
# `jump` user forwards ssh into VMs. Imported by ../module.nix.
#
#   api.<domain>                 -> boat-api (127.0.0.1:apiPort); /internal/* is never public
#   desktop.<domain>/<vmid>/     -> boat-gateway -> VM desktop port (signed link -> cookie)
#   <port>-<vmid>.vm.<domain>    -> boat-gateway -> VM ip:port (from /run/boat/routes.json)
#   ssh -J jump@ssh.<domain>:2222 dev@<vm ip>   (permitopen per key, written by boat-api)
#
# boat-gateway re-reads routes.json on change, so Caddy is never reloaded for
# VM or port changes. TLS for *.vm.<domain>: a wildcard via DNS-01 when
# gateway.dnsProvider is set, else per-host HTTP-01 certs on demand, allowed
# only for hosts boat-gateway confirms (its ask endpoint).
{ config, lib, pkgs, ... }:
let
  boat = config.services.boat;
  cfg = boat.gateway;
  inherit (lib) mkOption types;
  d = boat.domain;
  listen = "127.0.0.1:8742";
  askListen = "127.0.0.1:8743";

  jumpKeys = pkgs.writeShellScript "boat-jump-keys" ''
    PATH=${pkgs.gnugrep}/bin
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
        Caddy `dns` directive body for the DNS-01 wildcard certificate of
        `*.vm.<domain>`. Null: per-host HTTP-01 certificates on demand instead
        (first request to a new port URL waits for issuance; Let's Encrypt
        rate limits apply, see docs/boat.md).
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
      services.caddy = {
        enable = true;
        package = cfg.caddyPackage;
        email = boat.acmeEmail;
        inherit (cfg) environmentFile;
        globalConfig = lib.optionalString (cfg.dnsProvider == null) ''
          on_demand_tls {
            ask http://${askListen}/ask
          }
        '';
        virtualHosts = {
          "api.${d}".extraConfig = ''
            handle /internal/* {
              respond 404
            }
            reverse_proxy 127.0.0.1:${toString boat.apiPort}
          '';
          "desktop.${d}".extraConfig = ''
            reverse_proxy ${listen}
          '';
          "*.vm.${d}".extraConfig = ''
            tls {
              ${if cfg.dnsProvider == null then "on_demand" else "dns ${cfg.dnsProvider}"}
            }
            reverse_proxy ${listen}
          '';
        };
      };
      networking.firewall.allowedTCPPorts = [ 80 443 ];
      # HTTP/3.
      networking.firewall.allowedUDPPorts = [ 443 ];

      systemd.services.boat-gateway = {
        description = "boat HTTP router (VM ports + desktop links)";
        wantedBy = [ "multi-user.target" ];
        after = [ "boat-secrets.service" ];
        requires = [ "boat-secrets.service" ];
        before = [ "caddy.service" ];
        serviceConfig = {
          ExecStart = lib.escapeShellArgs [
            (lib.getExe cfg.package)
            "-domain" d
            "-listen" listen
            "-ask-listen" askListen
            "-routes" "/run/boat/routes.json"
            "-desktop-key" "${boat.dataDir}/secrets/desktop.key"
          ];
          User = "boat";
          Group = "boat";
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
          AuthorizedKeysCommand /etc/boat/jump-keys ${boat.dataDir}/ssh/jump_authorized_keys
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
