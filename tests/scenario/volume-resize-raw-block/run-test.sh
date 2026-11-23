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
# /dev/blockdata is the pod's volumeDevices devicePath; resolve its real
# major:minor via stat because the kernel name varies.
rawsectors=""
for attempt in $(seq 1 20); do
    if [ $((attempt % 4)) -eq 1 ]; then
        raw_node=$(oc get "pod/$POD_NAME" -n "$NS" -o jsonpath='{.spec.nodeName}')
        note_node "$raw_node"
        node_exec "$raw_node" "iscsiadm -m session --rescan; multipath -r" >/dev/null 2>&1 || true
    fi
    sleep 15
    rawsectors=$(toc exec "$POD_NAME" -n "$NS" -- sh -c 'maj=$(printf "%d" 0x$(stat -c %t /dev/blockdata)); min=$(printf "%d" 0x$(stat -c %T /dev/blockdata)); cat /sys/dev/block/$maj:$min/size' 2>/dev/null | tr -d '[:space:]')
    [ "${rawsectors:-0}" -ge 4194304 ] && break
done
[ "${rawsectors:-0}" -ge 4194304 ] || fail "raw block device 2Gi değil: ${rawsectors:-empty} sectors"
log "RAW BLOCK RESIZE OK: $(( rawsectors * 512 ))B"
