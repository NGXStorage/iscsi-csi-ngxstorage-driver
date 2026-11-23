#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
cleanup() { local rc=$?; trap - EXIT; direct_controller_race_cleanup || true; exit "$rc"; }
trap cleanup EXIT
race_idempotent_create_scenario
