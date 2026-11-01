# sshd AuthorizedKeysCommand for the `jump` user (runs as `boat`).
# boat-api writes one line per (key, VM) to the keys file. A line passes only
# when its options start with `restrict,port-forwarding,permitopen="<ip>:22"`,
# so a bad write can never grant a shell or an open forward.
f=${1:?keys file}
[ -r "$f" ] || exit 0
grep -E '^restrict,port-forwarding,permitopen="[][0-9a-fA-F.:]+:22"(,permitopen="[][0-9a-fA-F.:]+:22")* (ssh-|ecdsa-|sk-)' "$f" || true
