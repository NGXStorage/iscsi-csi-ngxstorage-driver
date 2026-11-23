#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
cleanup() {
    local rc=$?
    trap - EXIT
    race_publish_unpublish_cleanup || true
    run_test_cleanup || true
    exit "$rc"
}
trap cleanup EXIT
race_publish_unpublish_scenario
