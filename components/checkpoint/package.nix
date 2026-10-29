{ lib, buildGoModule, btrfs-progs, coreutils, diffutils, makeWrapper }:

buildGoModule {
  pname = "panda-ckpt";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [ diffutils ];
  postInstall = ''
    wrapProgram $out/bin/panda-ckpt --prefix PATH : ${lib.makeBinPath [ btrfs-progs coreutils diffutils ]}
  '';
  meta = {
    description = "Instant btrfs copy-on-write checkpoints and forks of agent workspaces, CLI + MCP stdio";
    mainProgram = "panda-ckpt";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
  };
}
