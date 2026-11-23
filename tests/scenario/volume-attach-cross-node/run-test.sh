#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
# Ensure the authorized namespace exists before the first namespaced apply
# (custom flow: this scenario applies manifests directly, not via apply_wait_pod).
render_test_dir
ensure_namespace "" || fail "namespace hazırlanamadı (fail-closed): $NS"
POD_A=$(pod_from_manifest "$(ls "$TEST_DIR"/*pod-a.yaml | head -1)")
POD_B=$(pod_from_manifest "$(ls "$TEST_DIR"/*pod-b.yaml | head -1)")
[ -n "$POD_A" ] && [ -n "$POD_B" ] || fail "cross-node pod manifestleri eksik"
# Pin pod-a and pod-b to two distinct Ready workers.
mapfile -t READY_NODES < <(oc get nodes --no-headers | awk '$2=="Ready"{print $1}' | awk '/worker/{print}' | head -2)
[ "${#READY_NODES[@]}" -ge 2 ] || mapfile -t READY_NODES < <(oc get nodes --no-headers | awk '$2=="Ready"{print $1}' | head -2)
[ "${#READY_NODES[@]}" -ge 2 ] || fail "iki Ready node bulunamadı"
sed -i "s|__NODE_A__|${READY_NODES[0]}|; s|__NODE_B__|${READY_NODES[1]}|" "$TEST_DIR"/*pod-a.yaml "$TEST_DIR"/*pod-b.yaml
note_node "${READY_NODES[0]}"; note_node "${READY_NODES[1]}"
oc apply -f "$TEST_DIR/volume-attach-cross-node-sc.yaml" -f "$TEST_DIR/volume-attach-cross-node-pvc.yaml" -n "$NS" >/dev/null
wait_test_pvcs_bound
oc apply -f "$TEST_DIR/volume-attach-cross-node-pod-a.yaml" -n "$NS" >/dev/null
wait_pod_ready "$POD_A" 600
note_node "$(oc get "pod/$POD_A" -n "$NS" -o jsonpath='{.spec.nodeName}')"
oc exec "$POD_A" -n "$NS" -- sh -c 'sha256sum -c /data/marker.sha256'
CROSS_PVC=$(oc get "pod/$POD_A" -n "$NS" -o jsonpath='{.spec.volumes[?(@.persistentVolumeClaim)].persistentVolumeClaim.claimName}')
CROSS_PV=$(pv_for_pvc "$CROSS_PVC")
[ -n "$CROSS_PV" ] || fail "cross-node PV çözülemedi"
oc delete pod "$POD_A" -n "$NS" --wait=true >/dev/null
wait_volume_detached "$CROSS_PV" "$DETACH_TIMEOUT" || fail "cross-node detach timeout: pv=$CROSS_PV"
oc apply -f "$TEST_DIR/volume-attach-cross-node-pod-b.yaml" -n "$NS" >/dev/null
wait_pod_ready "$POD_B" 600 90
note_node "$(oc get "pod/$POD_B" -n "$NS" -o jsonpath='{.spec.nodeName}')"
oc logs "$POD_B" -n "$NS" | grep -q 'passed' || oc exec "$POD_B" -n "$NS" -- sh -c 'sha256sum -c /data/marker.sha256'
log "CROSS-NODE ATTACH VERİ BÜTÜNLÜĞÜ OK: $POD_A -> $POD_B"
