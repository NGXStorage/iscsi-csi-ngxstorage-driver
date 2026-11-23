#!/usr/bin/env bash
# Generic writable I/O contract: the REAL generic_scenario body must fail
# closed on a failed marker write or readback mismatch; the designed
# read-only denial (rc=1) stays a positive readonly flow; normal write/read
# succeeds. Extracted from this driver's lib.sh — no reimplementation.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/generic-io-contract.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

# Extract the actual generic_scenario function body from lib.sh.
awk '/^generic_scenario\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "$root/lib.sh" > "$scratch/generic.sh"
grep -q "write_marker" "$scratch/generic.sh" || { echo "extraction failed"; exit 1; }

run_case() { # $1=name $2=test_name $3=write_rc $4=read_behavior(ok|mismatch) $5=expected_rc
    local name="$1" tname="$2" wrc="$3" readb="$4" expect="$5"
    cat > "$scratch/harness.sh" <<HARNESS
set -uo pipefail
TEST_NAME="$tname"
TEST_DIR="$scratch/empty"
POD_NAME=pod-x
NS=ns-x
MARKER=marker-x
MOUNT_PATH=/data
mkdir -p "\$TEST_DIR"
log() { :; }
fail() { echo "FAIL: \$*"; exit 1; }
apply_wait_pod() { :; }
snapshot_scenario_flow() { :; }
toc() { :; }
oc() { :; }
write_marker() { return $wrc; }
read_marker() {
    if [ "$readb" = mismatch ]; then fail "veri bütünlüğü BOZUK"; fi
    return 0
}
source "$scratch/generic.sh"
generic_scenario
HARNESS
    local rc=0
    bash "$scratch/harness.sh" > "$scratch/out" 2>&1 || rc=$?
    if [ "$rc" -ne "$expect" ]; then
        echo "CASE $name: rc=$rc expected=$expect" >&2
        cat "$scratch/out" >&2
        exit 1
    fi
    echo "case $name: rc=$rc (expected $expect) OK"
}

run_case writable-write-fail      volume-lifecycle-rwo 2 ok       1
run_case writable-read-mismatch   volume-lifecycle-rwo 0 mismatch 1
run_case writable-write-read-ok   volume-lifecycle-rwo 0 ok       0
run_case readonly-denial-designed volume-access-readonly 1 ok     0
run_case unexpected-denial        volume-lifecycle-rwo 1 ok       1
echo "Generic writable I/O fail-closed contract PASS"
