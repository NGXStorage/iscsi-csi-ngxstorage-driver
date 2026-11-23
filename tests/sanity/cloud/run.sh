#!/usr/bin/env bash
# Tests deployed node/controller sockets using temporary client and proxy binaries.
set -uo pipefail
source "$CSI_DRIVER_TESTS/sanity/cloud/helpers.sh"
set -euo pipefail

CONTROLLER_POD=""
NODE_POD=""

cleanup() {
    local rc=$?
    trap - EXIT
    if ! sanity_cleanup_workdir "${NODE_POD:-}"; then
        touch "$OUT/.cleanup-failed"
        rc=1
    fi
    sanity_withdraw "${NODE_POD:-}" /csi/csi-sanity
    sanity_bridge_stop || true
    exit "$rc"
}
trap cleanup EXIT

NODE_POD="$(sanity_pick_pod node || true)"
CONTROLLER_POD="$(sanity_pick_pod controller || true)"
[ -n "$NODE_POD" ] || fail "running ${PROTOCOL} node pod bulunamadı (oc auth? ns=$(sanity_ns))"
[ -n "$CONTROLLER_POD" ] || fail "running ${PROTOCOL} controller pod bulunamadı (oc auth? ns=$(sanity_ns))"

SANITY_BRIDGE_POD="$CONTROLLER_POD"
bridge="$(sanity_bridge_start "$CONTROLLER_POD")"
sanity_run "$NODE_POD" "$bridge"
