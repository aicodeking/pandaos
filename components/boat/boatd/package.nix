{ lib, buildGoModule, makeWrapper, coreutils, iproute2, nftables, procps }:

buildGoModule {
  pname = "boatd";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = null;
  subPackages = [ "." ]; # e2e/testagent is a guest-side test helper
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  # firecracker/jailer paths come from /etc/boat/boatd.json.
  postInstall = ''
    wrapProgram $out/bin/boatd --prefix PATH : ${lib.makeBinPath [ coreutils iproute2 nftables procps ]}
  '';
  meta = {
    description = "boat VM manager: Firecracker microVMs with snapshots, forks, NAT and a guest agent";
    mainProgram = "boatd";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
}
