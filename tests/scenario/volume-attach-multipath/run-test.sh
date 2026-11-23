#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
generic_scenario
if [ "$KEEP" = "1" ]; then
    log "--keep: host teardown doğrulaması çalıştırılmadı"
else
    verify_host_teardown "$POD_NAME"
fi
