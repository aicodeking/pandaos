# Server: headless + boat (persistent Firecracker microVMs for agents) on a
# dedicated machine. See docs/boat.md for DNS, TLS and sizing.
#
# Set in /etc/pandaos/local.nix (panda-install --profile server writes them):
#   services.boat.domain = "boat.example.com";
#   services.boat.acmeEmail = "ops@example.com";
# Until then the domain is boat.localhost, which Caddy serves with its own
# local CA (no ACME requests for a name nobody owns).
#
# Hosting tenants you do not trust? Also set services.boat.untrustedTenants = true,
# and reboot: SMT off, abuse limits on, KSM and warmPool refused (PROTOCOL.md).
# Updates then default to `staged` (fetched, verified and built daily, applied
# by `sudo panda-update run`); set pandaos.updates.mode = "automatic" to undo.
#
# Already on via the base modules: nftables firewall, key-only ssh, sshguard,
# daily signed-release updates with rollback (updates.nix).
{ config, lib, ... }:
{
  imports = [ ./headless.nix ];

  services.boat = {
    enable = true;
    domain = lib.mkDefault "boat.localhost";
  };
  # An untrusted-tenant host changes only when its operator says so.
  pandaos.updates.mode = lib.mkIf config.services.boat.untrustedTenants (lib.mkDefault "staged");
  # Firewall: the gateway opens 80/443 (tcp) + 443 (udp, HTTP/3); the openssh
  # module opens 22 and the jump port 2222 (services.boat.sshGateway.port).

  # VM data disk: `panda-install --data-disk DEV` formats it btrfs (label
  # PANDADATA), writes /etc/pandaos/data-disk, and pins the mount by UUID in
  # /etc/pandaos/disk.nix without nofail (from the first nixos-rebuild on).
  # The prebuilt system copied at install time mounts by label with nofail,
  # so a box without a second disk still boots; there /var/lib/boat is a
  # btrfs subvolume on the system disk. boatd and boat-api refuse to start
  # when /etc/pandaos/data-disk exists but the disk is not mounted, so they
  # never write VM data to the bare mountpoint.
  fileSystems."/var/lib/boat" = {
    device = lib.mkDefault "/dev/disk/by-label/PANDADATA";
    fsType = "btrfs";
    options = lib.mkDefault [
      "compress=zstd:1"
      "noatime"
      "nofail"
      "x-systemd.device-timeout=30s"
    ];
  };

  # Host metrics on 127.0.0.1:9100 only: they name units, mounts and tenant
  # load. Scrape through an ssh tunnel, or set listenAddress to an admin
  # (WireGuard) address and open the port on that interface only.
  # boat-reboot-check writes boat_reboot_pending to the textfile collector.
  services.prometheus.exporters.node = {
    enable = true;
    listenAddress = lib.mkDefault "127.0.0.1";
    enabledCollectors = [ "systemd" "textfile" ];
    extraFlags = [ "--collector.textfile.directory=/var/lib/boat-metrics" ];
  };
}
