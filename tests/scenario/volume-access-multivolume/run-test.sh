#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
HOST_TEARDOWN_TIMEOUT=300
COUNT=${MULTIVOLUME_COUNT:-16}
[[ "$COUNT" =~ ^[0-9]+$ ]] && [ "$COUNT" -ge 2 ] && [ "$COUNT" -le 32 ] || fail "MULTIVOLUME_COUNT must be 2..32"
[ "$KEEP" = 0 ] || fail "high-count teardown test cannot run with --keep"

suffix="${PROTOCOL}-${STAMP//-/}"
SC_NAME="mv-sc-${suffix}"
PVC_PREFIX="mv-${suffix}"
POD_NAME="mv-pod-${suffix}"
SC_FILE="$OUT/storageclass.yaml"
PVC_FILE="$OUT/pvcs.yaml"
POD_FILE="$OUT/pod.yaml"
IDENTITY_FILE="$OUT/host/teardown-identities.tsv"
mkdir -p "$OUT/host"
POD_APPLY_ATTEMPTED=0
HOST_TEARDOWN_PROVEN=0

cleanup() {
    local rc=$? failed=0
    trap - EXIT
    oc delete "pod/$POD_NAME" -n "$NS" --ignore-not-found=true --wait=true --timeout=120s >/dev/null 2>&1 || failed=1
    if [ "$HOST_TEARDOWN_PROVEN" = 1 ] || [ "$POD_APPLY_ATTEMPTED" = 0 ]; then
        [ ! -f "$PVC_FILE" ] || oc delete -f "$PVC_FILE" -n "$NS" --ignore-not-found=true --wait=true --timeout=900s >/dev/null 2>&1 || failed=1
        oc delete "storageclass/$SC_NAME" --ignore-not-found=true --wait=true --timeout=120s >/dev/null 2>&1 || failed=1
    else
        echo "[ERROR] host teardown not proven; preserving PVCs and StorageClass for investigation: namespace=$NS" >&2
        failed=1
    fi
    cleanup_owned_namespace || failed=1
    if [ "$failed" -ne 0 ]; then touch "$OUT/.cleanup-failed"; [ "$rc" -ne 0 ] || rc=1; fi
    exit "$rc"
}
trap cleanup EXIT
# Ensure the authorized namespace exists before the first namespaced apply
# (custom flow: this scenario applies manifests directly, not via apply_wait_pod).
render_test_dir
ensure_namespace "" || fail "namespace hazırlanamadı (fail-closed): $NS"

cat > "$SC_FILE" <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SC_NAME
provisioner: $PROTOCOL.csi.ngxstorage.com
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
: > "$PVC_FILE"
for index in $(seq -w 0 $((COUNT - 1))); do
    [ ! -s "$PVC_FILE" ] || printf '%s\n' '---' >> "$PVC_FILE"
    cat >> "$PVC_FILE" <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_PREFIX}-${index}
  namespace: $NS
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources:
    requests:
      storage: 1Gi
  storageClassName: $SC_NAME
EOF
done
{
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $POD_NAME
  namespace: $NS
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ""
  containers:
  - name: multi-volume
    image: nginx
    command: ["/bin/sh", "-c", "sleep 3600"]
    volumeMounts:
EOF
    for index in $(seq -w 0 $((COUNT - 1))); do
        printf '    - name: volume-%s\n      mountPath: /data/volume-%s\n' "$index" "$index"
    done
    printf '  volumes:\n'
    for index in $(seq -w 0 $((COUNT - 1))); do
        cat <<EOF
  - name: volume-$index
    persistentVolumeClaim:
      claimName: ${PVC_PREFIX}-${index}
EOF
    done
} > "$POD_FILE"

log "creating $COUNT independent $PROTOCOL PVCs (1Gi each), one Pod"
oc apply -f "$SC_FILE" >/dev/null
oc apply -f "$PVC_FILE" -n "$NS" >/dev/null
deadline=$((SECONDS + 1800))
for index in $(seq -w 0 $((COUNT - 1))); do
    claim="${PVC_PREFIX}-${index}"
    while [ "$SECONDS" -lt "$deadline" ]; do
        phase=$(oc get "pvc/$claim" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        [ "$phase" = Bound ] && break
        sleep 3
    done
    [ "$phase" = Bound ] || fail "PVC did not bind: $claim phase=$phase"
    log "PVC bound $((10#$index + 1))/$COUNT: $claim"
done
POD_APPLY_ATTEMPTED=1
oc apply -f "$POD_FILE" -n "$NS" >/dev/null
# Sixteen independent ControllerPublishVolume calls can outlive the attach
# controller's per-attempt wait and emit a transient FailedAttachVolume event
# before its retry succeeds. Ignore only that event class for this workload's
# bounded readiness window; all other fatal events remain fail-fast, and the
# pod still must become Ready within 1200 seconds.
wait_pod_ready "$POD_NAME" 1200 1200
for index in $(seq -w 0 $((COUNT - 1))); do
    mount_path="/data/volume-${index}"
    marker="${MARKER}-${index}"
    toc exec "$POD_NAME" -n "$NS" -- sh -c "printf '%s\\n' '$marker' > '$mount_path/verify.txt' && test \"\$(cat '$mount_path/verify.txt')\" = '$marker'" \
        || fail "volume write/read marker failed: mount=$mount_path"
    log "DATA INTEGRITY OK: $mount_path"
done
capture_host_teardown_many "$POD_NAME" "$IDENTITY_FILE"
verify_host_teardown_many "$POD_NAME" "$IDENTITY_FILE"
HOST_TEARDOWN_PROVEN=1
log "HIGH-COUNT MULTIVOLUME TEARDOWN PASS: protocol=$PROTOCOL pvc_count=$COUNT"
