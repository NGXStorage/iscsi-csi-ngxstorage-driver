#!/usr/bin/env bash
set -euo pipefail
source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache_root="$HOME/.cache/ngxstorage"
mkdir -p "$cache_root"
scratch="$(mktemp -d "$cache_root/csi-runner-cleanup.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
fixture="$scratch/NFS/tests/scenario"
mkdir -p "$fixture/case"
cp "$source_root/run.sh" "$fixture/run.sh"
cat > "$fixture/lib.sh" <<'EOF'
backend_snapshots() { :; }
collect_logs() { :; }
EOF
export BENCH=ocp2 KUBE_BIN=bash ALLOW_LIVE=1 NAMESPACE=cleanup-test-ns
export NGX_CONTROLLER=fixture STORAGE_API_KEY=fixture NGX_POOL_NAME=fixture
export NGX_SDK_CLI="$(type -P true)" EVENTS="$scratch/events"
cat > "$fixture/case/pre-test.sh" <<'EOF'
echo pre >> "$EVENTS"
exit "${PRE_RC:-0}"
EOF
cat > "$fixture/case/run-test.sh" <<'EOF'
echo run >> "$EVENTS"
[ "${EMIT_SKIP:-0}" != 1 ] || echo 'SKIP: fixture'
# Simulate the EXIT-trap render/snapshot collection removing the run copy.
# mode=render: NFS (RENDER_DIR is the collected copy, TEST_DIR is source).
# mode=snapshot: FC/iSCSI (TEST_DIR==RENDER_DIR is the collected snapshot).
if [ "${DELETE_RUN_COPY:-0}" = 1 ]; then
    if [ "${DELETE_RUN_COPY_MODE:-render}" = snapshot ]; then
        rm -rf "$TEST_DIR"
    else
        rm -rf "$RENDER_DIR"
    fi
fi
exit "${RUN_RC:-0}"
EOF
cat > "$fixture/case/post-test.sh" <<'EOF'
echo post >> "$EVENTS"
[ "${CLEANUP_MARKER:-0}" != 1 ] || touch "$OUT/.cleanup-failed"
exit 0
EOF
run_case() {
    local name="$1" expected_rc="$2" expected_status="$3" expected_events="$4" rc=0
    rm -rf "$fixture/case/logs" "$fixture/case/result.md"
    : > "$EVENTS"
    bash "$fixture/run.sh" case > "$scratch/output" 2>&1 || rc=$?
    [ "$rc" -eq "$expected_rc" ] || { cat "$scratch/output"; echo "$name: rc=$rc" >&2; exit 1; }
    grep -Fq "Result: **$expected_status** (rc=$expected_rc)" "$fixture/case/result.md"
    [ "$(paste -sd , "$EVENTS")" = "$expected_events" ] || { echo "$name: wrong lifecycle" >&2; exit 1; }
}
export PRE_RC=7 RUN_RC=0 EMIT_SKIP=0 CLEANUP_MARKER=0
run_case pre-failure 7 FAIL pre,post
export PRE_RC=0 RUN_RC=3 EMIT_SKIP=1
run_case failure-with-skip 3 FAIL pre,run,post
export RUN_RC=0
run_case explicit-skip 0 SKIPPED pre,run,post
export CLEANUP_MARKER=1
run_case cleanup-failure-with-skip 1 FAIL pre,run,post
export CLEANUP_MARKER=0 EMIT_SKIP=0
run_case success 0 PASS pre,run,post
export DELETE_RUN_COPY=1 DELETE_RUN_COPY_MODE=snapshot
run_case run-copy-collected-post-survives 0 PASS pre,run,post
unset DELETE_RUN_COPY DELETE_RUN_COPY_MODE
printf '%s\n' 'Scenario preflight/cleanup and failure classification PASS'
