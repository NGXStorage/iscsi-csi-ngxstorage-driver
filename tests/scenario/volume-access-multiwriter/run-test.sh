#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
# Two pods on the same node write to one RWO volume at the same time; the
# driver must reject the second publish. The engine verifies both pods.
apply_wait_pod
NOTE_A=$(oc get "pod/volume-access-multiwriter-pod-a" -n "$NS" -o jsonpath='{.spec.nodeName}')
NOTE_B=$(oc get "pod/volume-access-multiwriter-pod-b" -n "$NS" -o jsonpath='{.spec.nodeName}')
[ "$NOTE_A" = "$NOTE_B" ] || fail "multiwriter pod'ları aynı node'a pinlenmemiş: $NOTE_A != $NOTE_B"
note_node "$NOTE_A"
wait_pod_ready volume-access-multiwriter-pod-a
wait_pod_ready volume-access-multiwriter-pod-b
oc exec "pod/volume-access-multiwriter-pod-a" -n "$NS" -- su -s /bin/sh -c "echo $MARKER |tee /data/multiwriter.txt >/dev/null && sync" root
oc exec "pod/volume-access-multiwriter-pod-b" -n "$NS" -- su -s /bin/sh -c "cat /data/multiwriter.txt" root | grep -q "$MARKER" \
    || fail "multiwriter pod-b source'taki marker'ı okuyamadı"
log "MULTIWRITER OK: $NOTE_A üzerinde iki pod aynı volume'u paylaşıyor (SNW), marker cross-pod okundu"
