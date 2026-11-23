#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
# Provision a real backend LUN for the static PV; the handle rendered into
# the manifests carries its ID. Owned volumes are deleted by post-test.sh.
mkdir -p "$OUT"
EX_ID=$("$NGX_SDK_CLI" --controllers "$NGX_CONTROLLER" --api-key "$NGX_API_KEY" \
    --pool-name "$NGX_POOL_NAME" create-lun "csi-iscsi-$STAMP" 1 | jq -r '.id // empty')
[ -n "$EX_ID" ] || { echo "[ERROR] profile returned an empty existing-volume ID" >&2; exit 1; }
echo "$EX_ID" > "$OUT/existing-volume.id"
echo "[pre] existing volume id=$EX_ID" >&2
export EXISTING_VOLUME_ID="$EX_ID"
