#!/usr/bin/env bash
# The boatd end-to-end test is the full-stack one: ../e2e.sh GUEST_DIR
# (real Firecracker + jailer on KVM; run by .github/workflows/boat-e2e.yml).
exec "$(cd "$(dirname "$0")" && pwd)/../e2e.sh" "$@"
