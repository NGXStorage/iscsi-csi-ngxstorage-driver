#!/usr/bin/env bash
# Build the SHARED csi-sanity binary from the pinned fork commit into a
# caller-specified destination. One binary for every driver; never a stale
# driver-local cmd/csi-sanity copy. Sources the harness for the pinned SHA and
# csi_sanity_build_shared (SHA-keyed clean clone -> verify HEAD==SHA ->
# go build -mod=vendor, stamped with the commit via -ldflags VERSION).
#
#   CSI_TEST_FORK_URL  git source of the fork (default: the GitHub repo).
#                      Override to a local mirror/checkout when the build
#                      host cannot reach github.com.
#
# Usage: build-shared-sanity.sh <destination-binary>
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dest="${1:?usage: build-shared-sanity.sh <destination-binary>}"

# shellcheck source=cloud/prebuilt.sh
source "$script_dir/cloud/prebuilt.sh"

csi_sanity_build_shared "$dest" || {
  echo "build-shared-sanity: shared csi-sanity build failed (fork @${CSI_TEST_FORK_SHA})" >&2
  exit 1
}
echo "build-shared-sanity: $dest built from ${CSI_TEST_FORK_URL} @${CSI_TEST_FORK_SHA}"
