#!/usr/bin/env bash
# A real agent, through the real AI proxy, doing real work: if CI has an
# ANTHROPIC_API_KEY or OPENAI_API_KEY, store it as a team AI credential, run
# `boat prompt` in a VM with a task that writes a file, and assert the file. Also
# asserts the key never reached the VM and that the proxy metered the tokens.
#
#   BOAT_URL=… BOAT_API_KEY=<admin key> ANTHROPIC_API_KEY=… components/boat/sdk/e2e-agent-from-env.sh
#
# With no key this prints a SKIPPED line and exits 0, so a green job does NOT
# mean an agent ran unless the log shows "e2e-agent: PASS". Needs boat and jq on
# PATH, and a boat-api with -ai-proxy-listen that boatd lets VMs reach.
set -euo pipefail
: "${BOAT_URL:?set BOAT_URL}"
export BOAT_API_KEY=${BOAT_API_KEY:-${BOAT_KEY:-}}
[ -n "$BOAT_API_KEY" ] || { echo "e2e-agent: set BOAT_API_KEY or BOAT_KEY" >&2; exit 2; }

if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  agent=claude provider=anthropic real=$ANTHROPIC_API_KEY envname=ANTHROPIC_API_KEY
elif [ -n "${OPENAI_API_KEY:-}" ]; then
  agent=codex provider=openai real=$OPENAI_API_KEY envname=OPENAI_API_KEY
else
  echo "SKIPPED: no ANTHROPIC_API_KEY or OPENAI_API_KEY in the environment; no real agent ran"
  exit 0
fi
# The key must not sit in the environment of anything this script starts in a VM.
unset ANTHROPIC_API_KEY OPENAI_API_KEY

fail() { echo "e2e-agent: FAIL: $*" >&2; exit 1; }
[ "$(boat ai proxy --json | jq -r .available)" = true ] || fail "this boat-api has no -ai-proxy-listen/-ai-proxy-url"

vm=""
cleanup() {
  set +e
  [ -z "$vm" ] || boat rm "$vm" --json >/dev/null
  boat ai credentials rm "$provider" --json >/dev/null
}
trap cleanup EXIT
printf '%s' "$real" | boat ai credentials set "$provider" - --json >/dev/null

vm=$(boat new --name "agent-e2e-$$" --no-env --no-secrets --ttl 30m --json | jq -r .id)
echo "VM $vm, agent $agent, provider $provider"

token=$(boat exec "$vm" -- bash -lc "printenv $envname")
case $token in boatvm_*) ;; *) fail "VM env has no proxy token" ;; esac
if boat exec "$vm" -- sudo cat /etc/boat/env | grep -qF -- "$real"; then
  fail "the real key is in the VM's /etc/boat/env"
fi
echo "ok - the VM holds a per-VM token, not the key"

proof=/home/dev/agent-proof.txt
want="boat-agent-ok-$$"
args=(prompt "$vm" "Create the file $proof containing exactly this one line: $want. Then stop." --agent "$agent")
[ -z "${BOAT_AGENT_MODEL:-}" ] || args+=(--model "$BOAT_AGENT_MODEL")
boat "${args[@]}" || fail "boat prompt exited $?"

got=$(boat exec "$vm" -- cat "$proof") || fail "the agent did not create $proof"
[ "$(printf '%s' "$got" | tr -d '[:space:]')" = "$want" ] || fail "file contents: $got"
echo "ok - the agent wrote the file"

reqs=$(boat ai usage "$vm" --json | jq -s 'map(.requests) | add // 0')
[ "$reqs" -gt 0 ] || fail "the proxy metered no requests, so the agent did not go through it"
echo "ok - the proxy metered $reqs request(s)"
echo "e2e-agent: PASS"
