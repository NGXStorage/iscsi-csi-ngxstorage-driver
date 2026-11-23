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
[ -n "$POD_A" ] && [ -n "$POD_B" ] || fail "readonly-multipod pod manifestleri eksik"
# Prefer worker nodes: block driver node plugins run only on workers.
mapfile -t READY_NODES < <(oc get nodes --no-headers | awk '$2=="Ready"{print $1}' | awk '/worker/{print}' | head -2)
[ "${#READY_NODES[@]}" -ge 2 ] || mapfile -t READY_NODES < <(oc get nodes --no-headers | awk '$2=="Ready"{print $1}' | head -2)
[ "${#READY_NODES[@]}" -ge 2 ] || fail "iki Ready node bulunamadı, ROX testine yeterli node yok"
sed -i "s|__NODE_A__|${READY_NODES[0]}|; s|__NODE_B__|${READY_NODES[1]}|" "$TEST_DIR"/*pod-a.yaml "$TEST_DIR"/*pod-b.yaml
note_node "${READY_NODES[0]}"; note_node "${READY_NODES[1]}"
log "ROX node pin: $POD_A -> ${READY_NODES[0]}, $POD_B -> ${READY_NODES[1]}"
oc apply -f "$TEST_DIR"/*-sc.yaml -f "$TEST_DIR"/*-pvc.yaml -n "$NS" >/dev/null
oc apply -f "$TEST_DIR"/*pod-a.yaml -f "$TEST_DIR"/*pod-b.yaml -n "$NS" >/dev/null
wait_pod_ready "$POD_A" 600
wait_pod_ready "$POD_B" 600
for p in "$POD_A" "$POD_B"; do
    # Gate on the volume actually being mounted; CSI mount completion can lag
    # behind pod Ready, and exec racing it fails flakily.
    mout=""
    for attempt in $(seq 1 24); do
        mout=$(toc exec "$p" -n "$NS" -- sh -c 'mountpoint /data' 2>&1 || true)
        printf '%s' "$mout" | grep -q "is a mountpoint" && break
        sleep 5
    done
    printf '%s' "$mout" | grep -q "is a mountpoint" || fail "$p üzerinde /data mount değil: $mout"
    log "$p /data mount doğrulandı"
    wout=""
    for attempt in $(seq 1 12); do
        wout=$(toc exec "$p" -n "$NS" -- sh -c 'echo rox-test > /data/rox-write-test' 2>&1 || true)
        printf '%s' "$wout" | grep -qi "read-only" && break
        # Retry only CRI exec races right after container start; any other
        # non-empty output is unexpected and must fail fast.
        if [ -n "$wout" ] && ! printf '%s' "$wout" | grep -q "failed to execute process"; then
            break
        fi
        sleep 5
    done
    printf '%s' "$wout" | grep -qi "read-only" || fail "$p üzerinde write read-only değildi: $wout"
    log "$p WRITE REDDEDİ (read-only doğrulandı)"
done
log "ROX MULTI-POD DOĞRULANDI: iki node aynı anda read-only mount aldı, yazma reddedildi"
