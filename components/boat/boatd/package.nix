{ lib, buildGoModule, makeWrapper, coreutils, iproute2, nftables, procps, conntrack-tools, btrfs-progs, util-linux }:

buildGoModule {
  pname = "boatd";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  # firecracker/jailer paths come from /etc/boat/boatd.json; nsenter (util-linux)
  # is only used without the jailer.
  postInstall = ''
    wrapProgram $out/bin/boatd --prefix PATH : ${lib.makeBinPath [ coreutils iproute2 nftables procps conntrack-tools btrfs-progs util-linux ]}
  '';
  meta = {
    description = "boat VM manager: Firecracker microVMs with snapshots, forks, NAT and a guest agent";
    mainProgram = "boatd";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
}
