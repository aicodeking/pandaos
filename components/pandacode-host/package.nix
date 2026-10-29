# PandaCode's release AppImage (a Tauri app that bundles its GTK/WebKit libs).
# Bump: change version, then hash = SRI of the release's
# PandaCode-Linux-x86_64.AppImage.sha256 (`nix hash convert --to sri sha256:<hex>`).
#
# The AppImage bundles WebKitGTK, GTK and almost all of their deps, so the
# FHS env only needs the libraries it leaves to the host (excludelist), not
# appimageTools' default set. See slim-notes.md for the inventory; recheck
# with readelf on version bumps.
{
  lib,
  pkgs,
  appimageTools,
  buildFHSEnv,
  callPackage,
  emptyDirectory,
  fetchurl,
  writeShellScript,
}:
let
  version = "0.3.75";
  # false = appimageTools' default FHS library set (+1 GB, known to work).
  useSlim = true;

  pname = "pandacode";
  src = fetchurl {
    url = "https://github.com/aicodeking/pandacode/releases/download/v${version}/PandaCode-Linux-x86_64.AppImage";
    # sha256 3231f46c23b0a4eba9f26b7a93cffe5f6d389ee66618f2e35513c75914f72db4
    hash = "sha256-MjH0bCOwpOup8mt6k8/+X204nuZmGPLjVRPHWRT3LbQ=";
  };
  # The same derivation wrapType2 extracts internally.
  extracted = appimageTools.extract { inherit pname version src; };

  # buildFHSEnv always adds the full glibcLocales (222 MiB). NixOS services
  # get LOCALE_ARCHIVE from systemd.globalEnvironment and login shells from
  # sessionVariables, both outside the FHS env, so ship an empty one.
  # ponytail: reaches into buildFHSEnv's internal callPackage; if a nixpkgs
  # bump breaks eval, drop `appimageTools'` back to plain appimageTools.
  appimageTools' = appimageTools.override {
    buildFHSEnv = buildFHSEnv.override {
      callPackage = fn: args:
        callPackage fn (args // { pkgs = pkgs // { glibcLocales = emptyDirectory; }; });
    };
  };
in
(if useSlim then appimageTools' else appimageTools).wrapType2 ({
  inherit pname version src;
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
} // lib.optionalAttrs useSlim {
  # What `appimage-exec.sh -w` does, without its extraction deps (binutils, 34 MB).
  runScript = writeShellScript "pandacode-apprun" ''
    export APPDIR=${extracted} APPIMAGE_SILENT_INSTALL=1
    exec ${extracted}/AppRun "$@"
  '';
  # Overrides the default targetPkgs/multiPkgs. buildFHSEnv always adds
  # glibc (libc, libm, libresolv, ld-linux) and gcc.cc.lib (libstdc++, libgcc_s).
  targetPkgs = pkgs: with pkgs; [
    libx11 # libX11, libX11-xcb
    libxcb
    libGL # libGL, libEGL (glvnd)
    libdrm
    libgbm
    wayland # libwayland-client/-cursor/-egl/-server (NEEDED, unused on X11)
    expat
    fontconfig
    freetype
    fribidi
    harfbuzz
    zlib
    gmp # libgnutls, libhogweed
    libgpg-error # libgcrypt
    e2fsprogs # libcom_err (bundled krb5)
  ];
  multiPkgs = _: [ ];
})
