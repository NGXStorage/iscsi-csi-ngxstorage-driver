#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: ocp-acceptance-preflight.sh <FC|iSCSI|NFS> [--namespace NAME]

The command is intentionally read-only. It renders the OpenShift overlay,
runs an OpenShift server dry-run, checks cluster-scoped ownership collisions,
and verifies the permissions needed for a later controlled rollout.

Set ALLOW_EXISTING_CLUSTER_OBJECTS=1 only during an explicitly approved
migration. This script never applies resources.
EOF
}

if [[ $# -eq 0 || "$1" == "-h" || "$1" == "--help" ]]; then
  usage
  exit 0
fi
protocol="$1"
shift
namespace=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      [[ $# -ge 2 ]] || { echo "--namespace requires a value" >&2; exit 2; }
      namespace="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$protocol" in
  FC)
    default_namespace=ngxstorage-fc
    scc=fc-csi-ngxstorage-node-scc
    csi_driver=fc.csi.ngxstorage.com
    storage_class=fc-csi-ngxstorage
    snapshot_class=fc-csi-ngxstorage
    cluster_roles=(fc-csi-ngxstorage-controller)
    cluster_bindings=(fc-csi-ngxstorage-controller)
    ;;
  iSCSI)
    default_namespace=ngxstorage-iscsi
    scc=iscsi-csi-ngxstorage-node-scc
    csi_driver=iscsi.csi.ngxstorage.com
    storage_class=iscsi-csi-ngxstorage
    snapshot_class=iscsi-csi-ngxstorage
    cluster_roles=(iscsi-csi-ngxstorage-controller)
    cluster_bindings=(iscsi-csi-ngxstorage-controller)
    ;;
  NFS)
    default_namespace=ngxstorage-nfs
    scc=nfs-csi-ngxstorage-node-scc
    csi_driver=nfs.csi.ngxstorage.com
    storage_class=nfs-csi-ngxstorage
    snapshot_class=nfs-csi-ngxstorage
    cluster_roles=(nfs-csi-ngxstorage-controller)
    cluster_bindings=(nfs-csi-ngxstorage-controller)
    ;;
  *)
    echo "unsupported protocol: $protocol" >&2
    usage >&2
    exit 2
    ;;
esac

namespace="${namespace:-$default_namespace}"
command -v oc >/dev/null || { echo "oc is required" >&2; exit 127; }
oc whoami >/dev/null || { echo "oc authentication is required" >&2; exit 1; }

driver_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest="$driver_root/deploy/manifest/openshift"
[[ -d "$manifest" ]] || { echo "manifest not found: $manifest" >&2; exit 1; }

if [[ "$namespace" != "$default_namespace" ]]; then
  tmp_root="$HOME/.cache/ngxstorage"
  mkdir -p "$tmp_root"
  render_tmp="$(mktemp -d "$tmp_root/ngxstorage-ocp-preflight.XXXXXX")"
  trap 'rm -rf "$render_tmp"' EXIT
  cp -R "$driver_root/deploy/manifest" "$render_tmp/manifest"
  sed -i "s/^namespace: .*/namespace: $namespace/" "$render_tmp/manifest/openshift/kustomization.yaml"
  manifest="$render_tmp/manifest/openshift"
  echo "[$protocol] rendering with custom namespace/$namespace"
fi

echo "[$protocol] authenticated as $(oc whoami)"
echo "[$protocol] checking cluster permission to create SCC"
if [[ "$(oc auth can-i create securitycontextconstraints 2>/dev/null)" != "yes" ]]; then
  echo "missing permission: create securitycontextconstraints" >&2
  exit 1
fi

if [[ "${ALLOW_EXISTING_CLUSTER_OBJECTS:-0}" != "1" ]]; then
  collisions=()
  check_existing() {
    local kind="$1" name="$2"
    if oc get "$kind" "$name" >/dev/null 2>&1; then
      collisions+=("$kind/$name")
    fi
  }
  check_existing scc "$scc"
  check_existing csidriver.storage.k8s.io "$csi_driver"
  check_existing storageclass.storage.k8s.io "$storage_class"
  check_existing volumesnapshotclass.snapshot.storage.k8s.io "$snapshot_class"
  for name in "${cluster_roles[@]}"; do check_existing clusterrole.rbac.authorization.k8s.io "$name"; done
  for name in "${cluster_bindings[@]}"; do check_existing clusterrolebinding.rbac.authorization.k8s.io "$name"; done
  if ((${#collisions[@]})); then
    printf '%s\n' "[$protocol] refusing rollout: existing cluster-scoped objects are owned by a legacy release:" >&2
    printf '  %s\n' "${collisions[@]}" >&2
    echo "Use an approved migration window and set ALLOW_EXISTING_CLUSTER_OBJECTS=1 only after ownership/rollback review." >&2
    exit 3
  fi
fi

if ! oc get namespace "$namespace" >/dev/null 2>&1; then
  echo "[$protocol] namespace/$namespace does not exist; create and verify it before server dry-run" >&2
  exit 3
fi

rendered="$(oc kustomize "$manifest")"
printf '%s\n' "$rendered" | oc apply --dry-run=server -f - >/dev/null
echo "[$protocol] server dry-run passed"

echo "[$protocol] no unapproved cluster-scoped ownership collision detected"
echo "[$protocol] preflight passed; no resources were changed"
