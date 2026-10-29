# shellcheck shell=bash
# panda-agents-install [--check] CLI...: add agent CLIs to this box.
# Merges them into $dir/agents.nix (imported by local.nix) and rebuilds with
# $PANDA_AGENTS_UPDATE; restores agents.nix if that fails. --check only
# validates the names. module.nix sets PANDA_AGENTS_*; tests set them too.
set -euo pipefail
dir=${PANDA_AGENTS_DIR:-/etc/pandaos}
bindir=${PANDA_AGENTS_BIN:-/run/current-system/sw/bin}
known=${PANDA_AGENTS_KNOWN:?} # "name:binary ..."
unpackaged=${PANDA_AGENTS_UNPACKAGED-}
update=${PANDA_AGENTS_UPDATE:?}
file=$dir/agents.nix

declare -A bin=()
for kv in $known; do
  bin[${kv%%:*}]=${kv#*:}
done
names=$(printf '%s\n' "${!bin[@]}" | sort | tr '\n' ' ')

usage() {
  echo "usage: panda-agents-install [--check] CLI..." >&2
  echo "CLIs: ${names% }" >&2
  exit 2
}

is_known() {
  [[ $1 =~ ^[a-z0-9-]+$ ]] && [ -n "${bin[$1]+x}" ]
}

check=
if [ "${1-}" = --check ]; then
  check=1
  shift
fi
[ $# -gt 0 ] || usage
for n in "$@"; do
  is_known "$n" && continue
  if [[ " $unpackaged " == *" $n "* ]]; then
    echo "panda-agents-install: $n is not packaged in this PandaOS's nixpkgs yet; install it by hand for now" >&2
  else
    echo "panda-agents-install: unknown CLI '$n'" >&2
  fi
  usage
done
[ -z "$check" ] || exit 0

if [ ! -f "$dir/flake.nix" ]; then
  echo "panda-agents-install: no $dir/flake.nix, so this is not an installed PandaOS." >&2
  echo "Set pandaos.agents.clis = [ $(printf '"%s" ' "$@")]; in your NixOS config instead." >&2
  exit 1
fi
if ! grep -qs 'agents\.nix' "$dir/local.nix"; then
  echo "panda-agents-install: $dir/local.nix does not import ./agents.nix. Add to its imports:" >&2
  echo '  ++ lib.optional (builtins.pathExists ./agents.nix) ./agents.nix' >&2
  exit 1
fi
if [ ! -w "$dir" ]; then
  echo "panda-agents-install: run as root (sudo)" >&2
  exit 1
fi

# Selection = what agents.nix already has (if still known) + the arguments.
old=$(cat "$file" 2>/dev/null || true)
mapfile -t want < <({
  grep -o '"[a-z0-9-]*"' <<<"$old" | tr -d '"' || true
  printf '%s\n' "$@"
} | sort -u)
list='' missing=''
for n in "${want[@]}"; do
  is_known "$n" || continue
  list+=" \"$n\""
  [ -x "$bindir/${bin[$n]}" ] || missing+=" $n"
done
new="# Agent CLIs for this box, written by panda-agents-install.
# Edit it or rerun the command; apply edits with: sudo panda-update run
{ pandaos.agents.clis = [$list ]; }"

if [ "$new" = "$old" ] && [ -z "$missing" ]; then
  echo "Already installed:$list"
  exit 0
fi

tmp=$(mktemp "$dir/.agents.nix.XXXXXX")
printf '%s\n' "$new" >"$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$file"
echo "Installing:$list (needs a network: downloads, then rebuilds)"
read -ra cmd <<<"$update"
if "${cmd[@]}"; then
  exit 0
fi
# Keep agents.nix matching the running system, or the next daily update
# would apply it unasked.
if [ -n "$old" ]; then
  printf '%s\n' "$old" >"$file"
else
  rm -f "$file"
fi
echo "panda-agents-install: apply failed; $file restored. Retry when online." >&2
exit 1
