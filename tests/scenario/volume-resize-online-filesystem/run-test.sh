#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
apply_wait_pod
RESIZE_PVC=$(grep -m1 '  name:' "$(ls "$TEST_DIR"/*-pvc.yaml | head -1)" | awk '{print $2}')
oc patch "pvc/$RESIZE_PVC" -n "$NS" --type merge \
    -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'
oc wait --for=jsonpath='{.status.capacity.storage}'=2Gi "pvc/$RESIZE_PVC" -n "$NS" --timeout=600s \
    || { oc describe "pvc/$RESIZE_PVC" -n "$NS" | tail -15; fail "resize PVC 2Gi capacity göstermedi"; }
fssize=$(toc exec "$POD_NAME" -n "$NS" -- sh -c "df -B1 --output=size $MOUNT_PATH | tail -1" 2>/dev/null || true)
[ "${fssize:-0}" -ge 1820855552 ] || fail "dosya sistemi 2Gi'e büyümedi: ${fssize:-empty}"
log "RESIZE OK: pvc=$RESIZE_PVC fs=${fssize}B (pod çalışırken)"
