# panda-update-verify FLAKE_DIR INPUT ALLOWED_SIGNERS CACHE_GIT_DIR STATE_FILE
#
# Bumps INPUT in FLAKE_DIR/flake.lock, then accepts the new lock only if:
#   1. the locked commit is SSH-signed by a key in ALLOWED_SIGNERS
#      (git verify-commit with gpg.minTrustLevel=fully: a valid signature by a
#      key that is NOT in the file is refused, not just "unknown"),
#   2. it descends from the previously locked commit (no downgrade to an older
#      signed release, no force-pushed history),
#   3. the tree Nix fetched (narHash in the lock) is that commit's tree, so the
#      host serving the tarball cannot swap content under a signed commit.
# Otherwise it puts the old flake.lock back and exits non-zero, so the caller
# keeps running the current system. Writes the outcome to STATE_FILE.
set -euo pipefail

flake_dir=$1 input=$2 signers=$3 cache=$4 state=$5
lock=$flake_dir/flake.lock
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -f "$lock" ]; then cp -p "$lock" "$tmp/flake.lock.orig"; fi

record() {
  mkdir -p "$(dirname "$state")"
  echo "$1 $(date -u +%Y-%m-%dT%H:%M:%SZ) $2" >"$state.tmp" && mv "$state.tmp" "$state"
}
refuse() {
  echo "panda-update: REFUSING release ${new:-?}: $1. Staying on the current system." >&2
  if [ -f "$tmp/flake.lock.orig" ]; then cp -p "$tmp/flake.lock.orig" "$lock"; else rm -f "$lock"; fi
  record refused "${new:-?}: $1"
  exit 1
}
field() { jq -r --arg i "$input" ".nodes[\$i].$1 // empty" "$lock"; }

old=""
if [ -f "$lock" ]; then old=$(field locked.rev); fi

# Checked first, every run: with no trust anchor this box is not being
# patched, and status (panda-update status, panda-sys sys_updates) must say so
# daily, not only when a release happens to move.
grep -Eq '^[^#[:space:]]' "$signers" 2>/dev/null ||
  refuse "updates are disabled: pandaos.updates.allowedSigners is empty, so no release can be trusted (set the release-signing key; see docs/updates-and-rollback.md)"

nix flake update --refresh "$input" --flake "$flake_dir"

new=$(field locked.rev)
[ -n "$new" ] || refuse "the lock has no commit for input '$input'"
if [ "$new" = "$old" ]; then
  # The same revision can still name different fetched content or a different source.
  jq -e --arg i "$input" --slurpfile before "$tmp/flake.lock.orig" \
    '.nodes[$i] == $before[0].nodes[$i]' "$lock" >/dev/null ||
    refuse "input '$input' changed without a new release commit"
  echo "panda-update: no new release (still $new)"
  exit 0
fi
echo "panda-update: release moved $old -> $new; verifying its signature"

type=$(field locked.type)
case "$type" in
  github) url="https://$(field locked.host | grep . || echo github.com)/$(field locked.owner)/$(field locked.repo).git" ;;
  git) url=$(field locked.url) ;;
  *) refuse "input type '$type' cannot be verified (use github: or git+ URLs)" ;;
esac
ref=$(field original.ref)
ref=${ref:-HEAD}

if [ ! -d "$cache" ]; then git init -q --bare "$cache"; fi
g() { git --git-dir="$cache" "$@"; }
g fetch -q --no-tags --force "$url" "$ref:refs/pandaos/release" ||
  refuse "could not fetch $ref from $url to check the signature"
g cat-file -e "$new^{commit}" 2>/dev/null || refuse "commit is not on $ref of $url"

g cat-file commit "$new" | grep -q '^gpgsig -----BEGIN SSH SIGNATURE-----' ||
  refuse "the commit has no SSH signature"
if ! out=$(g -c gpg.ssh.allowedSignersFile="$signers" -c gpg.minTrustLevel=fully \
  verify-commit "$new" 2>&1); then
  echo "$out" >&2
  refuse "signature is not from a key in pandaos.updates.allowedSigners"
fi
[ "$(g -c gpg.ssh.allowedSignersFile="$signers" log -1 --format=%G? "$new")" = G ] ||
  refuse "signature is not good"

if [ -n "$old" ] && ! { g cat-file -e "$old^{commit}" 2>/dev/null && g merge-base --is-ancestor "$old" "$new"; }; then
  refuse "it does not descend from the running release $old (downgrade or rewritten history)"
fi

want=$(field locked.narHash)
GIT_INDEX_FILE=$tmp/index g read-tree "$new"
mkdir "$tmp/tree"
GIT_INDEX_FILE=$tmp/index g --work-tree="$tmp/tree" checkout-index -a
got=$(nix hash path --algo sha256 "$tmp/tree")
[ "$got" = "$want" ] || refuse "fetched content $want is not the signed commit's tree $got"

signer=$(g -c gpg.ssh.allowedSignersFile="$signers" log -1 --format=%GS "$new")
echo "panda-update: release $new is signed by $signer"
record verified "$new signed by $signer"
