# sshd AuthorizedKeysCommand for the `jump` user (runs as `boat`).
#   jump-keys KEYS_FILE [ROUTES_JSON]
# boat-api writes one line per key:
#   restrict,port-forwarding,permitopen="<ip>:22"[,permitopen="<ip>:22"...] <key>
# Only lines of exactly that shape pass, so a bad write can never grant a
# shell or an open forward. Each permitopen must also name a VM that is
# running right now (boatd's routes.json): a grant for a deleted, stopped or
# TTL-expired VM is dropped here even before boat-api prunes it, so an IP
# handed to a new VM is never reachable with an old grant.
f=${1:?keys file}
routes=${2:-/run/boat/routes.json}
[ -r "$f" ] || exit 0
live=$(jq -r '.vms[]?.ip // empty' "$routes" 2>/dev/null | tr '\n' ' ')
grep -E '^restrict,port-forwarding,permitopen="[][0-9a-fA-F.:]+:22"(,permitopen="[][0-9a-fA-F.:]+:22")* (ssh-|ecdsa-|sk-)' "$f" |
  awk -v live=" $live " '{
    n = split($1, o, ",")
    out = "restrict,port-forwarding"
    kept = 0
    for (i = 3; i <= n; i++) {
      ip = o[i]
      sub(/^permitopen="/, "", ip)
      sub(/:22"$/, "", ip)
      gsub(/[][]/, "", ip)
      if (index(live, " " ip " ")) { out = out "," o[i]; kept++ }
    }
    if (kept) print out substr($0, length($1) + 1)
  }'
exit 0
