#!/bin/sh
# Install the boat CLI from a pandaos GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/aicodeking/pandaos/main/components/boat/cli/install.sh | sh
#
# Env:
#   BOAT_VERSION       release tag (default: latest)
#   BOAT_INSTALL_DIR   target dir (default: ~/.local/bin; /usr/local/bin needs sudo)
#   BOAT_REPO          GitHub owner/repo (default: aicodeking/pandaos)
#   BOAT_RELEASE_BASE  download base URL instead of GitHub (mirrors, tests)
#
# The release carries boat_<os>_<arch> and boat_SHA256SUMS; the binary is
# installed only if its SHA-256 matches. Later upgrades: `boat upgrade`.
set -eu

repo=${BOAT_REPO:-aicodeking/pandaos}
version=${BOAT_VERSION:-latest}
dir=${BOAT_INSTALL_DIR:-$HOME/.local/bin}

die() { echo "install.sh: $*" >&2; exit 1; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s) (build from source: cd components/boat/cli && go build -o boat .)" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac
asset=boat_${os}_${arch}

if [ -n "${BOAT_RELEASE_BASE:-}" ]; then
  base=${BOAT_RELEASE_BASE%/}
elif [ "$version" = latest ]; then
  base=https://github.com/$repo/releases/latest/download
else
  base=https://github.com/$repo/releases/download/$version
fi

if command -v curl >/dev/null 2>&1; then
  get() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  get() { wget -q -O "$2" "$1"; }
else
  die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

get "$base/boat_SHA256SUMS" "$tmp/sums" || die "download $base/boat_SHA256SUMS failed"
get "$base/$asset" "$tmp/boat" || die "download $base/$asset failed"
want=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$tmp/sums")
[ -n "$want" ] || die "$asset is not listed in boat_SHA256SUMS"
got=$(sha "$tmp/boat")
[ "$got" = "$want" ] || die "checksum mismatch for $asset (got $got, want $want)"

mkdir -p "$dir"
chmod 0755 "$tmp/boat"
mv -f "$tmp/boat" "$dir/boat" || die "cannot write $dir (set BOAT_INSTALL_DIR or use sudo)"
echo "installed $dir/boat ($version, $asset)"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on PATH; add: export PATH=\"$dir:\$PATH\"" ;;
esac
"$dir/boat" version || true
