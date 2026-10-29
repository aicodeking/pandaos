# PandaCode's release AppImage (a Tauri app that bundles its GTK/WebKit libs).
# Bump: change version, then hash = SRI of the release's
# PandaCode-Linux-x86_64.AppImage.sha256 (`nix hash convert --to sri sha256:<hex>`).
{ lib, appimageTools, fetchurl }:
let
  version = "0.3.75";
in
appimageTools.wrapType2 {
  pname = "pandacode";
  inherit version;
  src = fetchurl {
    url = "https://github.com/aicodeking/pandacode/releases/download/v${version}/PandaCode-Linux-x86_64.AppImage";
    # sha256 3231f46c23b0a4eba9f26b7a93cffe5f6d389ee66618f2e35513c75914f72db4
    hash = "sha256-MjH0bCOwpOup8mt6k8/+X204nuZmGPLjVRPHWRT3LbQ=";
  };
  meta = {
    description = "PandaCode, run here as a headless PandaCode Connect host";
    homepage = "https://github.com/aicodeking/pandacode";
    # MIT plus a fork-attribution clause; free, so no allowUnfree is needed.
    license = {
      fullName = "PandaCode License (MIT with attribution)";
      url = "https://github.com/aicodeking/pandacode/blob/main/LICENSE";
      free = true;
      redistributable = true;
    };
    mainProgram = "pandacode";
    platforms = [ "x86_64-linux" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
