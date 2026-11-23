#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$root/lib.sh"

oc() { return 17; }
if wait_volume_detached pv-test 2; then
    echo "VolumeAttachment API failure was mistaken for detach" >&2
    exit 1
fi

oc() { printf '%s\n' pv-test; }
if wait_volume_detached pv-test 1; then
    echo "Existing VolumeAttachment was mistaken for detach" >&2
    exit 1
fi

oc() { return 0; }
wait_volume_detached pv-test 2

echo "CSI detach lookup failure contract PASS"
