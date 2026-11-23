#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
remaining=$(oc get pvc -n "$NS" -l app=workload-statefulset -o name)
if [ -n "$remaining" ]; then
    readarray -t claims <<< "$remaining"
    if ! oc wait --for=delete -n "$NS" "${claims[@]}" --timeout="${CLEANUP_TIMEOUT}s"; then
        touch "$OUT/.cleanup-failed"
        fail "StatefulSet test PVC cleanup did not complete"
    fi
fi
