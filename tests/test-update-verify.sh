#!/usr/bin/env bash
# Offline verifier regression: real SSH-signed Git commits, stubbed Nix lock/hash operations.
# Run: bash tests/test-update-verify.sh (requires bash, jq, git and ssh-keygen).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/flake" "$tmp/release"

export NIX_TEST_NEXT_LOCK="$tmp/next.lock" NIX_TEST_CALLS="$tmp/nix.calls"
export NIX_TEST_TREE="$tmp/expected-payload" NIX_TEST_HASH=sha256-test-signed-tree
cat >"$tmp/bin/nix" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"$NIX_TEST_CALLS"
case "$1 $2" in
  'flake update')
    [ "$3" = --refresh ] && [ "$4" = pandaos ] && [ "$5" = --flake ]
    cp "$NIX_TEST_NEXT_LOCK" "$6/flake.lock"
    ;;
  'hash path')
    [ "$3" = --algo ] && [ "$4" = sha256 ]
    cmp "$NIX_TEST_TREE" "$5/payload"
    echo "$NIX_TEST_HASH"
    ;;
  *) echo "unexpected nix call: $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/nix"
export PATH="$tmp/bin:$PATH"
# Isolate the fixture from user Git configuration, signing keys and hooks.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
ssh-keygen -q -t ed25519 -N "" -f "$tmp/good"
ssh-keygen -q -t ed25519 -N "" -f "$tmp/evil"
echo "release@pandaos.test $(cat "$tmp/good.pub")" >"$tmp/signers"
git -C "$tmp/release" init -q -b release
git -C "$tmp/release" config user.name test
git -C "$tmp/release" config user.email test@example.invalid
git -C "$tmp/release" config gpg.format ssh
git -C "$tmp/release" config user.signingkey "$tmp/good"
echo installed >"$tmp/release/payload"
git -C "$tmp/release" add payload
git -C "$tmp/release" commit -q -S -m installed
old=$(git -C "$tmp/release" rev-parse HEAD)
echo updated >"$tmp/release/payload"
cp "$tmp/release/payload" "$NIX_TEST_TREE"
git -C "$tmp/release" commit -qa -S -m updated
new=$(git -C "$tmp/release" rev-parse HEAD)
git -C "$tmp/release" -c user.signingkey="$tmp/evil" commit -q --allow-empty -S -m untrusted
untrusted=$(git -C "$tmp/release" rev-parse HEAD)
git -C "$tmp/release" commit -q --allow-empty --no-gpg-sign -m unsigned
unsigned=$(git -C "$tmp/release" rev-parse HEAD)

jq -n --arg rev "$old" --arg url "file://$tmp/release" --arg hash "$NIX_TEST_HASH" '
  {nodes: {root: {inputs: {pandaos: "pandaos"}}, pandaos: {
    locked: {type: "git", url: $url, rev: $rev, narHash: $hash},
    original: {type: "git", url: $url, ref: "release"}
  }}, root: "root", version: 7}' >"$tmp/base.lock"

prepare() {
  cp "$tmp/base.lock" "$tmp/flake/flake.lock"
  cp "$tmp/base.lock" "$tmp/original.lock"
  rm -f "$tmp/state" "$NIX_TEST_CALLS"
}
verify() {
  bash "$here/../nixos/modules/update-verify.sh" "$tmp/flake" pandaos \
    "$tmp/signers" "$tmp/cache.git" "$tmp/state" >"$tmp/log" 2>&1
}
refused() {
  if verify; then
    echo "FAIL: accepted $1" >&2
    return 1
  fi
  if ! { grep -q 'REFUSING' "$tmp/log" && grep -Fq "$2" "$tmp/log" &&
    grep -q '^refused ' "$tmp/state" && cmp -s "$tmp/original.lock" "$tmp/flake/flake.lock"; }; then
    cat "$tmp/log" >&2
    echo "FAIL: $1 did not record refusal and restore the original lock" >&2
    return 1
  fi
  echo "ok: $1 refused; original lock restored"
}

failures=0
prepare
jq -S . "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
verify
grep -q 'no new release' "$tmp/log"
if grep -q '^hash path' "$NIX_TEST_CALLS"; then
  echo 'FAIL: unchanged input did not take the no-op path' >&2
  exit 1
fi
echo 'ok: unchanged input succeeds despite JSON key reordering'

for change in \
  '.nodes.pandaos.locked.narHash = "sha256-tampered"' \
  '.nodes.pandaos.locked.url = "file:///untrusted"' \
  '.nodes.pandaos.locked.type = "tarball"' \
  '.nodes.pandaos.locked.dir = "other-tree"' \
  '.nodes.pandaos.original.ref = "other-release"' \
  'del(.nodes.pandaos.locked.narHash)'; do
  prepare
  jq "$change" "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
  refused "same-revision change: $change" 'without a new release commit' || failures=$((failures + 1))
done

prepare
cp "$tmp/base.lock" "$NIX_TEST_NEXT_LOCK"
mv "$tmp/signers" "$tmp/signers.saved"
: >"$tmp/signers"
refused 'unchanged input with no allowed signers' 'allowedSigners is empty'
[ ! -e "$NIX_TEST_CALLS" ]
mv "$tmp/signers.saved" "$tmp/signers"

prepare
jq --arg rev "$new" '.nodes.pandaos.locked.rev = $rev' "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
verify
grep -q '^verified ' "$tmp/state"
grep -q 'is signed by release@pandaos.test' "$tmp/log"
grep -q '^hash path' "$NIX_TEST_CALLS"
cmp -s "$NIX_TEST_NEXT_LOCK" "$tmp/flake/flake.lock"
echo 'ok: changed revision verifies the real SSH signature and checks the tree'

prepare
jq --arg rev "$new" '.nodes.pandaos.locked.rev = $rev | .nodes.pandaos.locked.narHash = "sha256-tampered"' \
  "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
refused 'changed revision with a tampered hash' "is not the signed commit's tree"

prepare
jq --arg rev "$unsigned" '.nodes.pandaos.locked.rev = $rev' "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
refused 'unsigned changed revision' 'has no SSH signature'

prepare
jq --arg rev "$untrusted" '.nodes.pandaos.locked.rev = $rev' "$tmp/base.lock" >"$NIX_TEST_NEXT_LOCK"
refused 'changed revision signed by an untrusted key' 'not from a key in pandaos.updates.allowedSigners'

prepare
jq --arg rev "$new" '.nodes.pandaos.locked.rev = $rev' "$tmp/base.lock" >"$tmp/flake/flake.lock"
cp "$tmp/flake/flake.lock" "$tmp/original.lock"
cp "$tmp/base.lock" "$NIX_TEST_NEXT_LOCK"
refused 'rollback to an older signed revision' 'does not descend from the running release'

[ "$failures" -eq 0 ] || { echo "FAIL: $failures same-revision changes accepted" >&2; exit 1; }
echo 'test-update-verify: ok'
