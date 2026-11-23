#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache_root="$HOME/.cache/ngxstorage"
mkdir -p "$cache_root"
export GOCACHE="${GOCACHE:-$cache_root/ngxstorage-csi-iscsi-go-cache}"
export TMPDIR="$cache_root"
export CSI_TEST_CACHE="$cache_root/ngxstorage-csi-iscsi"
mkdir -p "$CSI_TEST_CACHE"
run_race="${RUN_RACE:-0}"
run_vet="${RUN_VET:-0}"
run_e2e_compile="${RUN_E2E_COMPILE:-1}"

cd "$repo_root"
printf '[entrypoint] iSCSI immutable-rootfs contract\n'
./tests/test-entrypoint-contract.sh

printf '[layout] iSCSI pkg/driver responsibility contract\n'
# Main entry files stay the only production files in pkg/driver; extracted
# implementation lives in pkg/helper, pkg/iscsi, pkg/utils and csi-ngxstorage-utils.
allowed_driver_files="driver.go identity.go controller.go node.go driver_test.go identity_test.go controller_test.go node_test.go"
for file in "$repo_root"/pkg/driver/*.go; do
  name="$(basename "$file")"
  case " $allowed_driver_files " in
    *" $name "*) ;;
    *) printf 'pkg/driver contains disallowed file: %s\n' "$name" >&2; exit 1 ;;
  esac
done
for required in driver.go identity.go controller.go node.go; do
  if [[ ! -f "$repo_root/pkg/driver/$required" ]]; then
    printf 'pkg/driver is missing required file: %s\n' "$required" >&2
    exit 1
  fi
done
# RPC placement: each CSI RPC entry lives in its responsible main file.
for rpc in NodeStageVolume NodeUnstageVolume NodePublishVolume NodeUnpublishVolume NodeGetVolumeStats NodeExpandVolume NodeGetCapabilities NodeGetInfo; do
  grep -q "func (d \*Driver) $rpc(" "$repo_root/pkg/driver/node.go" || { printf 'node RPC %s missing from node.go\n' "$rpc" >&2; exit 1; }
done
for rpc in GetPluginInfo GetPluginCapabilities Probe; do
  grep -q "func (d \*Driver) $rpc(" "$repo_root/pkg/driver/identity.go" || { printf 'identity RPC %s missing from identity.go\n' "$rpc" >&2; exit 1; }
done
for rpc in CreateVolume DeleteVolume ControllerPublishVolume ControllerUnpublishVolume GetSnapshot CreateSnapshot DeleteSnapshot ListSnapshots ControllerGetVolume ControllerExpandVolume ListVolumes GetCapacity ValidateVolumeCapabilities ControllerModifyVolume ControllerGetCapabilities; do
  grep -q "func (d \*Driver) $rpc(" "$repo_root/pkg/driver/controller.go" || { printf 'controller RPC %s missing from controller.go\n' "$rpc" >&2; exit 1; }
done

printf '[scenarios] iSCSI matrix, shell contract and detach helper\n'
python3 tests/scenario/check-matrix-contract.py
bash tests/scenario/tests/lib-detach.sh
bash tests/scenario/tests/runner-cleanup.sh
bash tests/scenario/tests/generic-io-contract.sh
bash tests/scenario/tests/reboot-contract.sh
bash tests/scenario/tests/namespace-isolation.sh
bash tests/scenario/tests/render-run-identity.sh
bash tests/scenario/tests/disruptive-gate.sh
bash tests/sanity/tests/cloud-fixtures.sh
bash tests/sanity/tests/runtime-profile.sh
bash tests/sanity/tests/runtime-profile-negative.sh
bash tests/sanity/tests/prebuilt-contract.sh
bash tests/scenario/tests/lib-race-cleanup.sh
while IFS= read -r -d '' script; do bash -n "$script"; done < <(find tests/scenario tests/sanity deploy/ocp-acceptance -type f -name '*.sh' -print0)
bash deploy/ocp-acceptance/test-ocp-acceptance-verify.sh

printf '[unit] iSCSI root packages\n'
go test -count=1 ./... "$@"

if [[ "$run_e2e_compile" == "1" ]]; then
  printf '[e2e-compile] iSCSI tests/e2e (E2E_RUN forced to 0; live tests skip)\n'
  (cd tests/e2e && E2E_RUN=0 go test -count=1 ./...)
fi

if [[ "$run_race" == "1" ]]; then
  printf '[race] iSCSI pkg/...\n'
  go test -race -count=1 ./pkg/...
fi

if [[ "$run_vet" == "1" ]]; then
  printf '[vet] iSCSI root packages\n'
  go vet ./...
fi

printf '[test-tools] iSCSI shared csi-sanity + local race/proxy helpers\n'
# shellcheck source=tests/sanity/cloud/prebuilt.sh
source "$repo_root/tests/sanity/cloud/prebuilt.sh"
# Resolve through the shared channel: a verified prebuilt when
# CSI_PREBUILT_DIR is set, else a build from the pinned fork commit.
csi_sanity_resolve "$CSI_TEST_CACHE/csi-sanity" \
  || { echo "shared csi-sanity resolve failed (fork @${CSI_TEST_FORK_SHA})" >&2; exit 1; }
(
  cd tests/tools/csi-race
  go test -count=1 ./...
  if [[ "$run_race" == "1" ]]; then go test -race -count=1 ./...; fi
  if [[ "$run_vet" == "1" ]]; then go vet ./...; fi
  go build -o "$CSI_TEST_CACHE/csi-race" .
)
(cd tests/tools/sanity-proxy && go build -o "$CSI_TEST_CACHE/sanity-proxy" .)
