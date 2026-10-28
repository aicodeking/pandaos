{ lib, buildGoModule, gitMinimal, makeWrapper }:

buildGoModule {
  pname = "panda-mem";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [ gitMinimal ];
  postInstall = ''
    wrapProgram $out/bin/panda-mem --prefix PATH : ${lib.makeBinPath [ gitMinimal ]}
  '';
  meta = {
    description = "Shared git-backed agent memory over MCP stdio";
    mainProgram = "panda-mem";
    license = lib.licenses.mit;
  };
}
