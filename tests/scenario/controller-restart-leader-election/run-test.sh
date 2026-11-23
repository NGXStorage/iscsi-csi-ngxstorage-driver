#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
cleanup() {
    local rc=$?
    trap - EXIT
    controller_restart_cleanup || true
    run_test_cleanup || true
    exit "$rc"
}
trap cleanup EXIT
controller_restart_leader_election_scenario
