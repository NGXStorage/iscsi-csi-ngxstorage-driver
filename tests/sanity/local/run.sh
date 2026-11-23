#!/bin/bash

set -o pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SUDO="${SUDO-sudo --preserve-env=NGX_COORDINATION_LEASE_PREFIX,POD_NAMESPACE,POD_NAME,POD_UID,KUBE_NODE_NAME,KUBECONFIG}"
readonly FOCUS="${SANITY_FOCUS:-${FOCUS-}}"
readonly FOCUS_FILE="${SANITY_FOCUS_FILE:-${FOCUS_FILE-}}"
# Shared csi-sanity binary resolved by the dispatcher (verified prebuilt, or
# built from the pinned fork commit). Never a stale driver-local copy.
readonly CSI_SANITY="${CSI_SANITY_BIN:?shared csi-sanity binary path not resolved}"
# shellcheck source=tests/sanity/cloud/prebuilt.sh
source "$SCRIPT_DIR/../cloud/prebuilt.sh"
readonly CSI_DRIVER="$SCRIPT_DIR/../../../cmd/csi-driver/iscsi-csi-ngxstorage-driver"
readonly SKIP_TESTS="${SANITY_SKIP_TESTS-}"
readonly RAND_NUM=$(((RANDOM % 1000) + 1))
readonly MOUNT_DIR_1="$SCRIPT_DIR/mnt1$RAND_NUM"
readonly STAGING_DIR_1="$SCRIPT_DIR/staging1$RAND_NUM"

# Unix sockets have a short path limit; keep each run isolated in the home cache.
SOCKET_ROOT="$HOME/.cache/ngxstorage"
mkdir -p "$SOCKET_ROOT" || exit 1
SOCKET_DIR="$(mktemp -d "$SOCKET_ROOT/csi-sanity-sock.XXXXXX")" || exit 1
readonly SOCKET_DIR
readonly CONT_ENDPOINT="$SOCKET_DIR/c.sock"
readonly NODE_ENDPOINT_1="$SOCKET_DIR/n1.sock"
readonly NODE_ID_1="${SANITY_NODE_ID_1-01}"
readonly IP="${STORAGE_IPS:?set STORAGE_IPS}"
readonly POOL_NAME="${STORAGE_POOL_NAME:?set STORAGE_POOL_NAME}"
readonly PORTAL_GROUP="${STORAGE_PORTAL_GROUP:?set STORAGE_PORTAL_GROUP}"
readonly DATE=$(date +%d-%m-%Y_%H:%M:%S:%N)
readonly CONT_LOG="$SCRIPT_DIR/logs/cont_$DATE.log"
readonly NODE_LOG_1="$SCRIPT_DIR/logs/node1_$DATE.log"
readonly SNTY_LOG="$SCRIPT_DIR/logs/test-sanity-logs_$DATE.log"
readonly API_KEY="${STORAGE_API_KEY:?STORAGE_API_KEY must be set}"

NODE_1_PID=""
CONT_PID=""

cleanup() {
    if [ -n "$NODE_1_PID" ]; then
        $SUDO kill "$NODE_1_PID" 2>/dev/null || true
        wait "$NODE_1_PID" 2>/dev/null || true
    fi
    if [ -n "$CONT_PID" ]; then
        $SUDO kill "$CONT_PID" 2>/dev/null || true
        wait "$CONT_PID" 2>/dev/null || true
    fi
    $SUDO rm -rf "$SOCKET_DIR" "$MOUNT_DIR_1" "$STAGING_DIR_1"
}
trap cleanup EXIT

wait_for_socket() {
    local pid="$1" socket="$2" role="$3"
    for _ in $(seq 1 50); do
        [ -S "$socket" ] && return 0
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "ERROR: $role driver exited before its CSI socket became ready" >&2
            return 1
        fi
        sleep 0.2
    done
    echo "ERROR: $role CSI socket was not ready within 10 seconds" >&2
    return 1
}

mkdir -p "$SCRIPT_DIR/logs"
$SUDO rm -rf "$MOUNT_DIR_1" "$STAGING_DIR_1"

function log() {
    # $1 : type
    # $2 : message
    # $3 : log file

    echo -e "$(date)\t$1\t$2" 2>&1 | tee -a "$3"
}

log "DEBUG" "Running node 1 module" "$NODE_LOG_1"
log "DEBUG" "SCRIPT_DIR: $SCRIPT_DIR" "$NODE_LOG_1"
log "DEBUG" "CSI_SANITY: $CSI_SANITY" "$NODE_LOG_1"
log "DEBUG" "SKIP_TESTS: $SKIP_TESTS" "$NODE_LOG_1"
log "DEBUG" "NODE_ENDPOINT_1: $NODE_ENDPOINT_1" "$NODE_LOG_1"
log "DEBUG" "NODE_ID_1: $NODE_ID_1" "$NODE_LOG_1"
log "DEBUG" "IP: $IP" "$NODE_LOG_1"
log "DEBUG" "POOL_NAME: $POOL_NAME" "$NODE_LOG_1"
log "DEBUG" "PORTAL_GROUP: $PORTAL_GROUP" "$NODE_LOG_1"
log "DEBUG" "API_KEY: configured" "$NODE_LOG_1"

$SUDO "$CSI_DRIVER" --endpoint "unix://$NODE_ENDPOINT_1" --role node --nodeid "$NODE_ID_1" \
    --ip "$IP" --pool-name "$POOL_NAME" --api-key "$API_KEY" --log info \
    --portal-group "$PORTAL_GROUP" >>"$NODE_LOG_1" 2>&1 &
NODE_1_PID=$!
wait_for_socket "$NODE_1_PID" "$NODE_ENDPOINT_1" node || exit 1

log "DEBUG" "Running controller module" "$CONT_LOG"
log "DEBUG" "SCRIPT_DIR: $SCRIPT_DIR" "$CONT_LOG"
log "DEBUG" "CSI_SANITY: $CSI_SANITY" "$CONT_LOG"
log "DEBUG" "SKIP_TESTS: $SKIP_TESTS" "$CONT_LOG"
log "DEBUG" "CONT_ENDPOINT: $CONT_ENDPOINT" "$CONT_LOG"
log "DEBUG" "IP: $IP" "$CONT_LOG"
log "DEBUG" "POOL_NAME: $POOL_NAME" "$CONT_LOG"
log "DEBUG" "PORTAL_GROUP: $PORTAL_GROUP" "$CONT_LOG"
log "DEBUG" "API_KEY: configured" "$CONT_LOG"

$SUDO "$CSI_DRIVER" --endpoint "unix://$CONT_ENDPOINT" --role controller \
    --ip "$IP" --pool-name "$POOL_NAME" --api-key "$API_KEY" --log info \
    --portal-group "$PORTAL_GROUP" >>"$CONT_LOG" 2>&1 &
CONT_PID=$!
wait_for_socket "$CONT_PID" "$CONT_ENDPOINT" controller || exit 1

log "DEBUG" "Running test module" "$SNTY_LOG"
log "DEBUG" "SCRIPT_DIR: $SCRIPT_DIR" "$SNTY_LOG"
log "DEBUG" "CSI_SANITY: $CSI_SANITY" "$SNTY_LOG"
log "DEBUG" "CSI_DRIVER: $CSI_DRIVER" "$SNTY_LOG"
log "DEBUG" "SKIP_TESTS: $SKIP_TESTS" "$SNTY_LOG"
log "DEBUG" "RAND_NUM: $RAND_NUM" "$SNTY_LOG"
log "DEBUG" "MOUNT_DIR_1: $MOUNT_DIR_1" "$SNTY_LOG"
log "DEBUG" "STAGING_DIR_1: $STAGING_DIR_1" "$SNTY_LOG"

sleep 2
GINKGO_ARGS=(--ginkgo.vv --ginkgo.fail-on-empty --ginkgo.json-report="test-sanity-results_$DATE.json" --ginkgo.skip="$SKIP_TESTS" --ginkgo.trace)
if [ -n "$FOCUS" ]; then
    GINKGO_ARGS+=(--ginkgo.focus="$FOCUS")
fi
if [ -n "$FOCUS_FILE" ]; then
    GINKGO_ARGS+=(--ginkgo.focus-file="$FOCUS_FILE")
fi
# Opt-in shared-array inventory checks; omitted (default) => dedicated mode.
_sh="$(csi_sanity_shared_array_args)"
[ -n "$_sh" ] && GINKGO_ARGS+=("$_sh")
$SUDO "$CSI_SANITY" "${GINKGO_ARGS[@]}" \
    --csi.testvolumesize=1073741824 --csi.testvolumeexpandsize=2147483648 \
    --csi.testvolumeaccesstype="block" --csi.mountdir="$MOUNT_DIR_1" \
    --csi.stagingdir="$STAGING_DIR_1" \
    --csi.endpoint="unix://$NODE_ENDPOINT_1" --csi.controllerendpoint="unix://$CONT_ENDPOINT" \
    --csi.secrets="$SCRIPT_DIR/secrets.yaml" --csi.testvolumeparameters="$SCRIPT_DIR/params.yaml" \
    --csi.testvolumemutableparameters="$SCRIPT_DIR/mutableparams.yaml" 2>&1 | tee -a "$SNTY_LOG"
SANITY_STATUS=${PIPESTATUS[0]}

if [ "$SANITY_STATUS" -ne 0 ]; then
    log "ERROR" "Sanity failed: node1=$SANITY_STATUS" "$SNTY_LOG"
    exit 1
fi
exit 0
