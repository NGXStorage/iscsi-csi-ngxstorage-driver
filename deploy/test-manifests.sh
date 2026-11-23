#!/usr/bin/env bash
set -euo pipefail
driver_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 "$driver_root/deploy/test-manifests-common.py" iSCSI "$@"
