#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
NODE0=$(cat "$OUT/reboot-node" 2>/dev/null || true)
[ -n "$NODE0" ] || exit 0
if ! oc wait --for=condition=Ready "node/$NODE0" --timeout=1800s; then
    touch "$OUT/.cleanup-failed"
    fail "reboot node Ready olmadı; cordon korunuyor: $NODE0"
fi
if [ "${KUBE_BIN##*/}" = oc ]; then oc adm uncordon "$NODE0"; else oc uncordon "$NODE0"; fi
log "REBOOT: $NODE0 Ready, test cordon kaldırıldı"
