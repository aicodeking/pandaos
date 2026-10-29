#!/usr/bin/env bash
# Run a nix command, retrying (up to 3 attempts, 1 then 2 min apart) only when
# this attempt's output shows a cache/network throttle, e.g. magic-nix-cache's
# GitHub Actions cache answering HTTP 418/429. A real build or test failure
# fails at once. Outputs built by an earlier attempt stay in the store, so a
# retry resumes where it stopped.
#
# usage: nix-retry.sh LOGFILE nix ARGS...   (output is appended to LOGFILE)
set -uo pipefail

log=$1
shift
transient='(unable to download|error: cannot query|substituter).*(HTTP error (418|429|5[0-9][0-9])|Timeout was reached|Could not resolve host|Couldn.t resolve host|Connection reset|SSL connect error)|GitHub Actions Cache throttled|[Rr]ate.?limit(ed)? exceeded'

touch "$log"
for attempt in 1 2 3; do
  start=$(($(wc -l < "$log") + 1))
  "$@" 2>&1 | tee -a "$log"
  rc=${PIPESTATUS[0]}
  if [ "$rc" -eq 0 ]; then
    exit 0
  fi
  if [ "$attempt" -eq 3 ] || ! tail -n "+$start" "$log" | grep -Eq "$transient"; then
    exit "$rc"
  fi
  echo "::warning::nix exited $rc after a cache/network throttle (attempt $attempt of 3); retrying in $((attempt * 60)) s"
  sleep $((attempt * 60))
done
