# pandacode-host: slimming the closure

PandaCode v0.3.75, `PandaCode-Linux-x86_64.AppImage`, sha256
`3231f46c…72db4` (unchanged). 96 MB compressed; 276 MB extracted
(`pandacode-0.3.75-extracted`, NarSize 288.6 MB).

## Inventory

Built by Tauri plus linuxdeploy (`linuxdeploy-plugin-gtk`) on Ubuntu 22.04
(ICU 70, glib 2.72, GCC 12). Layout:

- `AppRun`: a bash script that sources `apprun-hooks/linuxdeploy-plugin-gtk.sh`
  and then execs `AppRun.wrapped`. The hook sets `GTK_*`, `GDK_PIXBUF_MODULE_FILE`,
  `GIO_EXTRA_MODULES`, `GSETTINGS_SCHEMA_DIR` and `GDK_BACKEND=x11` to point
  at the bundle. `AppRun.wrapped` is the AppImageKit AppRun: it sets
  `LD_LIBRARY_PATH=$APPDIR/usr/lib…` and does a **chdir to `$APPDIR/usr`**.
- `usr/bin/pandacode` (35 MB, RUNPATH `$ORIGIN/../lib`) and `xdg-open`.
- `usr/lib` (241 MB, about 200 sonames). It bundles all of WebKitGTK 4.1:
  `libwebkit2gtk-4.1` (86 MB), `libjavascriptcoregtk-4.1` (33 MB),
  `libicudata.so.70` (29 MB), librsvg, gtk-3, gdk-3, cairo, pango, glib/gio,
  libsoup-3, gnutls/nettle, the gstreamer libs, libsecret, krb5, libxml2,
  sqlite, libsystemd/udev, the X11 extension libs (Xext, Xrender, Xi,
  Xcomposite, …) and xcb-randr/render/shm. It also bundles the gdk-pixbuf
  loaders, GTK immodules, print backends, typelibs, `gio/modules/libgiognutls.so`,
  `x86_64-linux-gnu/webkit2gtk-4.1/{WebKitWebProcess,WebKitNetworkProcess,injected-bundle}`
  and the glib schemas in `usr/share`.
- Tauri patches `libwebkit2gtk` so that `/usr` reads as `././`. It finds its
  helper processes at `././/lib/x86_64-linux-gnu/webkit2gtk-4.1`, which is
  relative to the cwd set by AppRun.wrapped. **So the app must keep running
  through `AppRun`.** autoPatchelf with a plain makeWrapper would break
  WebKit's helper lookup. For that reason the slim build keeps
  appimageTools' bwrap FHS runtime and only changes what goes into it.

### NEEDED sonames that no bundled lib provides

Method: `readelf -d` over every ELF in the extract (in a Debian amd64
container). Each package below is in the slim FHS env for the reason given:

| soname | needed by | nixpkgs | NarSize |
|---|---|---|---|
| libc, libm, libresolv, ld-linux-x86-64 | everything; libresolv for krb5 | glibc (buildFHSEnv base) | 35 MB |
| libstdc++.so.6, libgcc_s.so.1 | pandacode, ICU, JSC, WebKit | gcc.cc.lib (base) | 10 MB |
| libX11.so.6, libX11-xcb.so.1 | gtk/gdk, X ext libs, gstgl | libx11 | 2.7 MB |
| libxcb.so.1 | cairo, xcb-randr/render/shm, gstgl | libxcb | 1.4 MB |
| libGL.so.1, libEGL.so.1 | gstgl; libepoxy/WebKit dlopen them | libGL (glvnd) | 2.6 MB |
| libdrm.so.2 | webkit2gtk, gstgl | libdrm | 0.6 MB |
| libgbm.so.1 | webkit2gtk, gstgl | libgbm | small |
| libwayland-{client,cursor,egl,server}.so.0 | gdk-3, webkit2gtk, im-wayland | wayland | 0.3 MB |
| libexpat.so.1 | webkit2gtk | expat | 0.3 MB |
| libfontconfig.so.1 | cairo, gtk, pango, webkit | fontconfig (lib) | 0.4 MB |
| libfreetype.so.6 | cairo, pangoft2, webkit | freetype | 2.2 MB |
| libfribidi.so.0 | gtk, gdk, pango | fribidi | small |
| libharfbuzz.so.0 | gtk, pango, webkit, harfbuzz-icu | harfbuzz (+glib 15.7 MB, graphite2) | 3.8 MB |
| libz.so.1 | cairo, cups, dw, png, … | zlib | small |
| libgmp.so.10 | gnutls, hogweed | gmp | 0.8 MB |
| libgpg-error.so.0 | gcrypt | libgpg-error | 0.9 MB |
| libcom_err.so.2 | bundled krb5/gssapi | e2fsprogs | 4.9 MB (+1.4 MB bin) |

That is the complete list of unresolved NEEDED entries. `ldd` on every bundled
ELF, run inside the slim FHS env with AppRun's `LD_LIBRARY_PATH`, reports
**0 missing**, the same as the default env. Libraries that are dlopen'd only
are mesa's GL/GBM backends, which come from `/run/opengl-driver`. The default
env ships no mesa either, so this does not change.

Not needed by the host: OpenSSL (pandacode does not link it), nss, curl,
dbus, gtk3 and friends (all bundled). The AppRun hook calls `dbus-send` and
`gsettings` only with `2>/dev/null || printf ''`, so it works when they
are missing.

## What package.nix does

1. `wrapType2` overrides `targetPkgs` with the list above and sets `multiPkgs = _: []`,
   which replaces appimageTools' `defaultFhsEnvArgs` (Steam-chroot set).
2. `runScript` is a 2-line `writeShellScript` that does the same as
   `appimage-exec.sh -w`: it exports `APPDIR` and `APPIMAGE_SILENT_INSTALL=1`
   and then execs `AppRun`. appimage-exec brings in binutils, libarchive,
   squashfs and pv, which are needed only to extract (binutils alone is 34 MB).
   `APPIMAGE_DEBUG_EXEC` is not supported. It is a debugging aid only.
3. `buildFHSEnv`'s base set includes the full `glibcLocales` (222 MiB). We give
   it `emptyDirectory` instead, through `buildFHSEnv.override { callPackage }`.
   On NixOS, `systemd.globalEnvironment.LOCALE_ARCHIVE` (services) and
   `environment.sessionVariables` (`panda-connect` from a shell) point outside
   the FHS env, and bwrap passes the environment and /nix/store through.
   Without them, glibc still has built-in C.UTF-8.
4. The extracted AppImage derivation is the same one that wrapType2 builds
   (one `-extracted` path in the closure).

## Closure (measured)

`nix-build` of both variants against the pinned nixpkgs
`718f670e` (nixos/nix amd64 container). Every path except the trivial
bwrap/rootfs/script derivations was substituted from cache.nixos.org, so
`nix path-info -s` equals the narinfo NarSize.

| variant | package closure |
|---|---|
| default wrapType2 (`useSlim = false`) | 1818.6 MB |
| slim (`useSlim = true`) | **466.8 MB** (288.6 MB of it is the app) |

Largest paths kept in slim (MB): extracted app 288.6, glibc 35.1, glib 16.1,
hwdata 10.3 (libdrm → libpciaccess), gcc-lib 10.3, openssl 9.3 (via a buildFHSEnv base package),
bash-interactive 7.4, e2fsprogs 5.0, systemd-minimal-libs 4.3, shadow 4.8.
Most of the non-app 178 MB (glibc, gcc-lib, bash, coreutils, openssl, systemd
libs, glib, ncurses, pam) is already in any NixOS system closure.

Largest paths dropped (378 paths, about 1.35 GB): glibc-locales 222 MiB,
python3 127, flite 60, systemd (full) 59, perl 55, gtk+3 41, icu4c 38,
ffmpeg-headless 33, binutils 33, freepats 31, gtk4 30 (zenity), x265 23,
iso-codes 22, pipewire 16, gst-plugins-bad 16, librsvg 15, mbedtls 12,
xkeyboard-config 10, bluez 10, cups 8, …

System estimate: the headless system went from 1.64 to 2.83 GB with the
default env. The slim env should bring it to about **1.95 GB** (±0.1): the
1.64 GB base, plus the 289 MB app, plus a few tens of MB of libs that are not
already shared. I did not build the system toplevel, so measure it with
`nix path-info -Sh` on the headless toplevel.

## Checks run (amd64 Docker on macOS, `--privileged` for bwrap)

- Both variants: `xvfb-run -a pandacode serve --help` gives exit 2 and
  byte-identical output (md5 match).
- Both variants: unsigned `xvfb-run -a pandacode serve` gives exit 1 and
  `This server is not signed in to PandaCode Connect…`.
- Both variants: the GUI path (`xvfb-run -a pandacode`) initialises GTK and
  WebKit and then stops at `Could not create default EGL display` (there is
  no mesa in the container). This is the same in both variants.
- `ldd` sweep: 0 unresolved in both.

These checks do not replace the CI `pandacode-host` NixOS VM test.

## Risks

- A `serve` code path that dlopens a lib only after sign-in (for example
  libsecret keyring access, which is bundled, or a gio/gst plugin) could need
  a host lib that the static scan does not see. Symptom: a runtime
  `cannot open shared object` error in the journal. Fix: add that package to
  `targetPkgs`.
- The glibcLocales stub relies on buildFHSEnv's internal
  `callPackage ./buildFHSEnv.nix`. A nixpkgs refactor could either break eval
  (loud) or silently stop applying the stub (+222 MiB, harmless).
- The `runScript` override bypasses appimage-exec. If a future appimageTools
  adds env setup there, mirror it.
- Fonts, CA certs, `/etc/machine-id` and `/run/opengl-driver` come from the
  host /etc and /run in both variants, so nothing changes there.
- Version bumps: rerun the readelf sweep. A rebuilt bundle (for example on
  Ubuntu 24.04) can change the excludelist set.

## Fallback

In `package.nix`, set `useSlim = false;` to get the old plain
`appimageTools.wrapType2` (default FHS set, full locales, appimage-exec).
There is no module option.
