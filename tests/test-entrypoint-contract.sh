#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
entrypoint="$repo_root/cmd/entrypoint.sh"
dockerfile="$repo_root/Dockerfile"
protocol="$(basename "$repo_root" | sed "s/-consumer$//")"

case "$protocol" in
  fc)
    binary="/usr/local/bin/fc-csi-ngxstorage-driver"
    image_title="NGX Storage FC CSI Driver"
    image_source="https://github.com/NGXStorage/fc-csi-ngxstorage-driver"
    ;;
  iscsi)
    binary="/usr/local/bin/iscsi-csi-ngxstorage-driver"
    image_title="NGX Storage iSCSI CSI Driver"
    image_source="https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver"
    ;;
  nfs)
    binary="/usr/local/bin/nfs-csi-ngxstorage-driver"
    image_title="NGX Storage NFS CSI Driver"
    image_source="https://github.com/NGXStorage/nfs-csi-ngxstorage-driver"
    ;;
  *)
    printf 'unsupported driver repository: %s\n' "$protocol" >&2
    exit 1
    ;;
esac

test -x "$entrypoint"
sh -n "$entrypoint"
test -f "$dockerfile"

if rg -n '^ARG UBI_IMAGE=.*:latest([[:space:]]|$)' "$dockerfile"; then
  printf '%s Dockerfile uses mutable :latest base image\n' "$protocol" >&2
  exit 1
fi
if ! rg -q '^ARG VCS_REF=unknown$' "$dockerfile" \
  || ! rg -q 'org\.opencontainers\.image\.revision="\$\{VCS_REF\}"' "$dockerfile" \
  || ! rg -q 'vcs-ref="\$\{VCS_REF\}"' "$dockerfile"; then
  printf '%s Dockerfile is missing VCS provenance labels\n' "$protocol" >&2
  exit 1
fi
if ! rg -Fq "org.opencontainers.image.title=\"$image_title\"" "$dockerfile" \
  || ! rg -Fq "org.opencontainers.image.source=\"$image_source\"" "$dockerfile"; then
  printf '%s Dockerfile image identity/provenance metadata is wrong\n' "$protocol" >&2
  exit 1
fi

if rg -n '/tmp(/|[[:space:]]|$)' "$entrypoint"; then
  printf '%s entrypoint references forbidden runtime path /tmp\n' "$protocol" >&2
  exit 1
fi
if rg -n '(^|[[:space:]])(ln|rm|mv|cp|install|mkdir|touch|truncate|chmod|chown)[[:space:]].*[[:space:]]/etc(/|[[:space:]]|$)' "$entrypoint"; then
  printf '%s entrypoint mutates immutable /etc\n' "$protocol" >&2
  exit 1
fi
if ! rg -q "^exec ${binary//\//\\/} \"\\\$@\"$" "$entrypoint"; then
  printf '%s entrypoint does not exec the expected driver binary\n' "$protocol" >&2
  exit 1
fi

set +e
controller_output="$("$entrypoint" --role=controller 2>&1)"
controller_status=$?
missing_output="$("$entrypoint" --role 2>&1)"
missing_status=$?
set -e

if [[ "$controller_status" -ne 127 ]] || [[ "$controller_output" != *"$binary"* ]]; then
  printf '%s controller role did not reach the immutable driver exec path (status=%s): %s\n' \
    "$protocol" "$controller_status" "$controller_output" >&2
  exit 1
fi
if [[ "$controller_output" == *"requires the host root"* ]]; then
  printf '%s controller role incorrectly requires /host\n' "$protocol" >&2
  exit 1
fi
if [[ "$missing_status" -ne 2 ]] || [[ "$missing_output" != *"missing value for --role"* ]]; then
  printf '%s missing --role value was not rejected deterministically\n' "$protocol" >&2
  exit 1
fi

printf '%s entrypoint/image immutable-rootfs + provenance contract passed\n' "$protocol"
