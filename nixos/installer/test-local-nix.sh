#!/usr/bin/env bash
# shellcheck disable=SC2034,SC2016 # globals read by the sourced installer; literal hash
# Unit test of panda-install's flag handling and local.nix output, without a
# disk or root: sources the installer (minus its final `main` call) and runs
# parse_args + write_local_nix into a temp dir. Run: bash test-local-nix.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# shellcheck disable=SC1090
source <(sed '$d' "$here/panda-install.sh")

fail() { echo "FAIL: $*" >&2; exit 1; }
k1="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl one@test"
k2="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEFkAfL5eeZvyBnKgqh9v1Dm8fJmlwFQ53z5mGXM0nF3 two@test"

reset() {
  disk="" profile=server hostname=h user=panda password_hash="" timezone=UTC relay=""
  domain=boat.example.test acme_email="" ipv6_prefix="" data_disk="" extra_config=""
  ssh_keys=() yes=1 build=0 sudo_nopasswd=0 ssh_key_set=0 allow_insecure_relay=0
}

# Two keys, no ACME e-mail, --sudo-nopasswd.
reset
parse_args --ssh-key "$k1" --ssh-key "$k2" --sudo-nopasswd --build
((${#ssh_keys[@]} == 2 && sudo_nopasswd && build)) || fail "flags not parsed"
password_hash='!'
write_local_nix "$tmp"
grep -qF "\"$k1\" \"$k2\"" "$tmp/local.nix" || fail "both keys not in local.nix"
grep -q 'wheelNeedsPassword = lib.mkForce false' "$tmp/local.nix" || fail "no nopasswd sudo"
grep -q acmeEmail "$tmp/local.nix" && fail "acmeEmail written without --acme-email"
grep -q 'extra.nix' "$tmp/local.nix" || fail "extra.nix not imported"

# Defaults: no nopasswd sudo; an e-mail given is written.
reset
parse_args --acme-email ops@example.test
password_hash='$y$x'
write_local_nix "$tmp"
grep -q wheelNeedsPassword "$tmp/local.nix" && fail "nopasswd sudo without the flag"
grep -q 'acmeEmail = "ops@example.test"' "$tmp/local.nix" || fail "acmeEmail missing"

# Validation.
check_email "" || fail "empty e-mail must be accepted"
check_email "bad" 2>/dev/null && fail "bad e-mail accepted"
check_extra_config /nonexistent 2>/dev/null && fail "missing --extra-config accepted"
echo "test-local-nix: ok"
