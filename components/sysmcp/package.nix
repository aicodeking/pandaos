{ lib, buildGoModule, makeWrapper, coreutils, procps, iproute2, btrfs-progs }:

buildGoModule {
  pname = "panda-sys";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  # systemctl, journalctl, sudo and panda-update come from the system PATH.
  postInstall = ''
    wrapProgram $out/bin/panda-sys --prefix PATH : ${lib.makeBinPath [ coreutils procps iproute2 btrfs-progs ]}
  '';
  meta = {
    description = "Read-mostly box status and ops tools for agents over MCP stdio";
    mainProgram = "panda-sys";
    license = lib.licenses.mit;
  };
}
