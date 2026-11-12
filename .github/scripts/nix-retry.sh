#!/usr/bin/env bash
# Run a nix command, retrying (up to 3 attempts, 1 then 2 min apart) only when
# this attempt's output shows a cache/network throttle, e.g. magic-nix-cache's
# GitHub Actions cache answering HTTP 418/429, or when the command printed
# nothing for NIX_RETRY_SILENCE_S (default 1500 s): run 43224143655's panda-cu
# job hung silent for 2 h after the Actions cache went down and magic-nix-cache's
# substituter was disabled mid-copy (a fresh nix gets the substituter back). A real
# build or test failure fails at once. Outputs built by an earlier attempt stay in
# the store, so a retry resumes where it stopped.
#
# usage: nix-retry.sh LOGFILE nix ARGS...   (output is appended to LOGFILE)
set -uo pipefail

log=$1
shift
silence=${NIX_RETRY_SILENCE_S:-1500}
poll=${NIX_RETRY_POLL_S:-10}
transient='(unable to download|error: cannot query|substituter).*(HTTP error (418|429|5[0-9][0-9])|Timeout was reached|Could not resolve host|Couldn.t resolve host|Connection reset|SSL connect error)|GitHub Actions Cache throttled|[Rr]ate.?limit(ed)? exceeded|is required, but there is no substituter that can build it'

touch "$log"
for attempt in 1 2 3; do
  start=$(($(wc -l < "$log") + 1))
  "$@" > >(tee -a "$log") 2>&1 &
  pid=$!
  size=-1 idle=0 stalled=
  while kill -0 "$pid" 2>/dev/null; do
    sleep "$poll"
    now=$(wc -c < "$log")
    if [ "$now" != "$size" ]; then size=$now idle=0; else idle=$((idle + poll)); fi
    if [ "$idle" -ge "$silence" ]; then
      stalled=1
      echo "::warning::nix printed nothing for ${silence} s; killing it (attempt $attempt of 3)" | tee -a "$log"
      kill "$pid" 2>/dev/null
      sleep 5
      kill -9 "$pid" 2>/dev/null
      break
    fi
  done
  wait "$pid"
  rc=$?
  if [ -z "$stalled" ] && [ "$rc" -eq 0 ]; then
    exit 0
  fi
  [ "$rc" -eq 0 ] && rc=1
  if [ "$attempt" -eq 3 ] || { [ -z "$stalled" ] && ! tail -n "+$start" "$log" | grep -Eq "$transient"; }; then
    exit "$rc"
  fi
  echo "::warning::nix exited $rc after a cache/network throttle or a silent hang (attempt $attempt of 3); retrying in $((attempt * 60)) s"
  sleep $((attempt * 60))
done
