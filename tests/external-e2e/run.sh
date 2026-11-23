#!/usr/bin/env bash

# Run Kubernetes External.Storage E2E tests for the already deployed NGX iSCSI
# CSI driver. The caller supplies a Kubernetes-version-matched e2e.test binary
# and kubeconfig; this script never deploys or modifies the driver.
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
readonly DRIVER_NAME="iscsi.csi.ngxstorage.com"
readonly SNAPSHOT_CLASS_NAME="iscsi-csi-ngxstorage"
readonly E2E_TEST_BINARY="${E2E_TEST_BINARY:?E2E_TEST_BINARY must point to a Kubernetes e2e.test binary}"
readonly KUBECONFIG_PATH="${KUBECONFIG:?KUBECONFIG must point to the target cluster kubeconfig}"
readonly E2E_FOCUS="${E2E_FOCUS:-External.Storage.*${DRIVER_NAME}}"
readonly E2E_SKIP="${E2E_SKIP:-\\[Disruptive\\]}"
readonly E2E_TIMEOUT="${E2E_TIMEOUT:-4h}"
readonly E2E_ALLOWED_NOT_READY_NODES="${E2E_ALLOWED_NOT_READY_NODES:-0}"
readonly E2E_NON_BLOCKING_TAINTS="${E2E_NON_BLOCKING_TAINTS:-node-role.kubernetes.io/control-plane,node-role.kubernetes.io/master}"

if [[ ! -x "$E2E_TEST_BINARY" ]]; then
    echo "E2E_TEST_BINARY is not executable: $E2E_TEST_BINARY" >&2
    exit 2
fi
if [[ ! -f "$KUBECONFIG_PATH" ]]; then
    echo "KUBECONFIG does not exist: $KUBECONFIG_PATH" >&2
    exit 2
fi
command -v kubectl >/dev/null

kubectl --kubeconfig "$KUBECONFIG_PATH" get csidriver "$DRIVER_NAME" >/dev/null
kubectl --kubeconfig "$KUBECONFIG_PATH" get volumesnapshotclass "$SNAPSHOT_CLASS_NAME" >/dev/null

# Kubernetes external-storage E2E v1.33.x unconditionally creates the
# cluster-scoped VolumePopulator CRD. If the cluster already owns it, that
# upstream case fails with AlreadyExists before testing the driver. Preserve
# the shared CRD and skip only that case; all other applicable E2E cases run.
readonly VOLUME_POPULATOR_CRD="volumepopulators.populator.storage.k8s.io"
effective_skip="$E2E_SKIP"
if kubectl --kubeconfig "$KUBECONFIG_PATH" get crd "$VOLUME_POPULATOR_CRD" >/dev/null 2>&1; then
    effective_skip="${effective_skip}|should provision storage with any volume data source"
    echo "Existing $VOLUME_POPULATOR_CRD detected; skipping only the conflicting any-volume-data-source case."
fi

readonly SCRATCH_ROOT="$HOME/.cache/ngxstorage"
case "$SCRATCH_ROOT" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "refusing temporary cache under /tmp or /var/tmp" >&2; exit 2 ;; esac
mkdir -p "$SCRATCH_ROOT"
readonly E2E_WORK_DIR="$(mktemp -d "$SCRATCH_ROOT/ngxstorage-csi-e2e.XXXXXX")"
readonly E2E_REPORT_DIR="${E2E_REPORT_DIR:-$PROJECT_ROOT/tests/external-e2e/logs/$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$E2E_REPORT_DIR"
cleanup() {
    rm -rf "$E2E_WORK_DIR"
}
trap cleanup EXIT

readonly STORAGE_CLASS_FILE="$E2E_WORK_DIR/storageclass.yaml"
readonly DRIVER_DEFINITION_FILE="$E2E_WORK_DIR/testdriver.yaml"
cp "$SCRIPT_DIR/storageclass.yaml" "$STORAGE_CLASS_FILE"
sed "s|__STORAGE_CLASS_FILE__|$STORAGE_CLASS_FILE|" \
    "$SCRIPT_DIR/testdriver.yaml" >"$DRIVER_DEFINITION_FILE"

"$E2E_TEST_BINARY" \
    --ginkgo.v \
    --ginkgo.progress \
    --ginkgo.timeout="$E2E_TIMEOUT" \
    --ginkgo.focus="$E2E_FOCUS" \
    --ginkgo.skip="$effective_skip" \
    --allowed-not-ready-nodes="$E2E_ALLOWED_NOT_READY_NODES" \
    --non-blocking-taints="$E2E_NON_BLOCKING_TAINTS" \
    --report-dir="$E2E_REPORT_DIR" \
    --kubeconfig="$KUBECONFIG_PATH" \
    --storage.testdriver="$DRIVER_DEFINITION_FILE"
