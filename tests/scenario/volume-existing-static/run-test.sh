#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
# pre-test.sh runs in a separate process, so read the LUN ID it provisioned.
EXISTING_VOLUME_ID=$(cat "$OUT/existing-volume.id" 2>/dev/null)
[ -n "$EXISTING_VOLUME_ID" ] || fail "pre-test.sh existing-volume.id üretmedi"
# Render the backend handle into a temp copy: the source fixtures stay
# driver-neutral.
SCRATCH_ROOT="${TMPDIR:-$HOME/.cache/ngxstorage}"
case "$SCRATCH_ROOT" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) fail "refusing scratch under /tmp or /var/tmp" ;; esac
mkdir -p "$SCRATCH_ROOT"
WORK=$(mktemp -d "$SCRATCH_ROOT/csi-existing.XXXXXX")
cp -a "$TEST_DIR"/. "$WORK/"
sed -i \
    -e "s|iscsi.csi.ngxstorage.com\.<LUN-ID>|iscsi.csi.ngxstorage.com.$EXISTING_VOLUME_ID|g" \
    -e "s|<LUN-ID>|$EXISTING_VOLUME_ID|g" \
    -e "s|volume-existing-static|existing-iscsi-$STAMP|g" "$WORK"/*.yaml
TEST_DIR="$WORK"
generic_scenario
