{ lib, buildGoModule }:
buildGoModule {
  pname = "boat-cli";
  version = "0.1.0";
  src = ./.;
  vendorHash = null;
  ldflags = [ "-s" "-w" ];
  postInstall = ''
    mv $out/bin/cli $out/bin/boat
  '';
  meta = {
    description = "Boat CLI: create, fork, prompt and ssh into persistent agent VMs";
    license = lib.licenses.mit;
    mainProgram = "boat";
  };
}
