#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
if [ "$BENCH" = "ocp2" ]; then
    # OpenShift 4.20 (k8s 1.33) ships the upstream VolumeAttributesClass gate
    # forced off (openshift/kubernetes pkg/features/kube_features.go: beta
    # Default=false; GA only from k8s 1.34 = OCP 4.21+), so the API is never
    # served. Skip deterministically instead of burning a provisioning cycle.
    echo "SKIP: OpenShift 4.20 does not serve VolumeAttributesClass (gate GA only in k8s 1.34 / OCP 4.21+)"
    exit 0
fi
if ! oc api-resources --api-group=storage.k8s.io -o name 2>/dev/null \
    | grep -qx 'volumeattributesclasses.storage.k8s.io'; then
    log "SKIP: VolumeAttributesClass API is not served by this cluster"
    exit 0
fi
apply_wait_pod
MUT_PVC=$(grep -m1 '  name:' "$(ls "$TEST_DIR"/*-pvc.yaml | head -1)" | awk '{print $2}')
MUT_PV=$(pv_for_pvc "$MUT_PVC")
[ -n "$MUT_PV" ] || fail "attributes-mutable PV çözülemedi"
oc patch "pvc/$MUT_PVC" -n "$NS" --type merge -p '{"spec":{"volumeAttributesClassName":"ngx-mutable"}}' >/dev/null
# k8s 1.31+ modifycontroller (csi-resizer) records completion on the PVC:
# status.currentVolumeAttributesClassName is only set once ModifyVolume
# succeeded and status.modifyVolumeStatus was cleared.
oc wait --for=jsonpath='{.status.currentVolumeAttributesClassName}'=ngx-mutable "pvc/$MUT_PVC" -n "$NS" --timeout=600s \
    || { oc describe "pvc/$MUT_PVC" -n "$NS" | tail -15; oc describe "pv/$MUT_PV" | tail -10; fail "VolumeAttributesClass mutation uygulanmadı"; }
log "VAC MUTATION OK: pvc=$MUT_PVC pv=$MUT_PV class=ngx-mutable"
