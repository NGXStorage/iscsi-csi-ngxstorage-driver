#!/usr/bin/env bash
set -euo pipefail

sanity_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "${1:-}" == -h || "${1:-}" == --help ]]; then
  echo "Usage: bash tests/sanity/run.sh <local|cloud> [--allow-live]"
  exit 0
fi
case "${1:-}" in
  local|cloud) mode="$1"; shift ;;
  *) echo "Usage: bash tests/sanity/run.sh <local|cloud> [--allow-live]" >&2; exit 2 ;;
esac
exec bash "$sanity_dir/../run-all-tests.sh" sanity "$mode" "$@"
