#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -uo pipefail
# Backend teardown: wait for the normal detach, delete the LUN the test
# created; recovery cleanup never masks a driver cleanup failure.
[ ! -f "$OUT/.cleanup-failed" ] || { echo "[post] preserving backend volume: Kubernetes cleanup failed" >&2; exit 1; }
ID_FILE="$OUT/existing-volume.id"
[ -f "$ID_FILE" ] || exit 0
ID=$(cat "$ID_FILE")
[ "$KEEP" = "1" ] && { echo "[post] backend cleanup skipped (--keep) id=$ID" >&2; exit 0; }
PV_NAME="existing-iscsi-$STAMP-pv"
oc get volumeattachments -o custom-columns='PV:.spec.source.persistentVolumeName' --no-headers 2>/dev/null | grep -Fxq "$PV_NAME" && {
    echo "[post] waiting up to ${DETACH_TIMEOUT:-300}s for VolumeAttachment detach" >&2
    deadline=$((SECONDS + ${DETACH_TIMEOUT:-300}))
    while [ "$SECONDS" -lt "$deadline" ]; do
        oc get volumeattachments -o custom-columns='PV:.spec.source.persistentVolumeName' --no-headers 2>/dev/null | grep -Fxq "$PV_NAME" || break
        sleep 2
    done
}
if backend_delete_lun "$ID"; then
    echo "[post] existing volume deleted id=$ID" >&2
else
    echo "[post] DRIVER CLEANUP FAILURE: LUN delete failed id=$ID even with target unmapping recovery" >&2
    exit 1
fi
