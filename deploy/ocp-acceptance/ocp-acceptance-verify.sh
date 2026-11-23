#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: ocp-acceptance-verify.sh <FC|iSCSI|NFS> [--namespace NAME] [--prefix PREFIX] [--shard-count 32|64|128|256] [--expected-image IMAGE]"
  echo "read-only post-rollout readiness/SCC/rootfs/CSINode verifier"
}

if [[ $# -eq 0 || "$1" == "-h" || "$1" == "--help" ]]; then
  usage
  exit 0
fi
protocol="$1"
shift
namespace=""
prefix=""
shard_count=128
expected_image=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      [[ $# -ge 2 ]] || { echo "--namespace requires a value" >&2; exit 2; }
      namespace="$2"
      shift 2
      ;;
    --prefix)
      [[ $# -ge 2 ]] || { echo "--prefix requires a value" >&2; exit 2; }
      prefix="$2"
      shift 2
      ;;
    --shard-count)
      [[ $# -ge 2 ]] || { echo "--shard-count requires a value" >&2; exit 2; }
      shard_count="$2"
      shift 2
      ;;
    --expected-image)
      [[ $# -ge 2 ]] || { echo "--expected-image requires a value" >&2; exit 2; }
      expected_image="$2"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$protocol" in
  FC) default_namespace=ngxstorage-fc; driver=fc.csi.ngxstorage.com; scc=fc-csi-ngxstorage-node-scc; controller=fc-csi-ngxstorage-controller; node=fc-csi-ngxstorage-node; controller_sa=fc-csi-ngxstorage-controller; node_sa=fc-csi-ngxstorage-node; main_container_name=fc-csi-ngxstorage ;;
  iSCSI) default_namespace=ngxstorage-iscsi; driver=iscsi.csi.ngxstorage.com; scc=iscsi-csi-ngxstorage-node-scc; controller=iscsi-csi-ngxstorage-controller; node=iscsi-csi-ngxstorage-node; controller_sa=iscsi-csi-ngxstorage-controller; node_sa=iscsi-csi-ngxstorage-node; main_container_name=iscsi-csi-ngxstorage ;;
  NFS) default_namespace=ngxstorage-nfs; driver=nfs.csi.ngxstorage.com; scc=nfs-csi-ngxstorage-node-scc; controller=nfs-csi-ngxstorage-controller; node=nfs-csi-ngxstorage-node; controller_sa=nfs-csi-ngxstorage-controller; node_sa=nfs-csi-ngxstorage-node; main_container_name=nfs-csi-ngxstorage ;;
  *) echo "unsupported protocol: $protocol" >&2; exit 2 ;;
esac
case "$shard_count" in 32|64|128|256) ;; *) echo "unsupported shard count: $shard_count" >&2; exit 2 ;; esac
namespace="${namespace:-$default_namespace}"
if [[ -n "$prefix" ]]; then
  controller="${prefix}-${controller}"
  node="${prefix}-${node}"
  controller_sa="${prefix}-${controller_sa}"
  node_sa="${prefix}-${node_sa}"
  scc="${prefix}-${scc}"
  driver="${prefix}.${driver}"
fi
resource_base="${controller%-controller}"
command -v oc >/dev/null || { echo "oc is required" >&2; exit 127; }
oc get namespace "$namespace" >/dev/null || { echo "namespace/$namespace not found" >&2; exit 1; }

failures=()
tmp_root="$HOME/.cache/ngxstorage"
mkdir -p "$tmp_root"
tmp_dir="$(mktemp -d "$tmp_root/ngxstorage-ocp-verify.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT
if ! oc rollout status "deployment/$controller" -n "$namespace" --timeout=10s >/dev/null 2>&1; then
  failures+=("deployment/$controller is not Ready")
fi

read -r desired ready < <(oc get daemonset "$node" -n "$namespace" -o json | python3 -c 'import json,sys; s=json.load(sys.stdin).get("status",{}); print(s.get("desiredNumberScheduled",0),s.get("numberReady",0))')
if [[ "$desired" != "$ready" || "$desired" == "0" ]]; then
  failures+=("daemonset/$node readiness is $ready/$desired")
fi

pod_file="$tmp_dir/pods.json"
oc get pods -n "$namespace" -o json >"$pod_file"
pod_failures="$(python3 - "$controller" "$node" "$scc" "$pod_file" <<'PY'
import json
import sys

controller, node, expected_scc, pod_file = sys.argv[1:]
with open(pod_file, encoding="utf-8") as stream:
    data = json.load(stream)
failures = []
for pod in data.get("items", []):
    name = pod.get("metadata", {}).get("name", "")
    if not (name.startswith(controller + "-") or name.startswith(node + "-")):
        continue
    is_controller = name.startswith(controller + "-")
    actual_scc = pod.get("metadata", {}).get("annotations", {}).get("openshift.io/scc")
    if is_controller:
        if not actual_scc or actual_scc == expected_scc:
            failures.append(f"{name}: controller uses node SCC or has no SCC admission")
    elif actual_scc != expected_scc:
        failures.append(f"{name}: actual SCC is not {expected_scc}")
    for c in pod.get("spec", {}).get("containers", []):
        cname = c.get("name", "")
        security = c.get("securityContext", {})
        if security.get("readOnlyRootFilesystem") is not True:
            failures.append(f"{name}/{cname}: rootfs is writable")
        if is_controller and security.get("privileged"):
            failures.append(f"{name}/{cname}: controller is privileged")
        if not is_controller and cname.endswith("-csi-ngxstorage") and security.get("privileged") is not True:
            failures.append(f"{name}/{cname}: node main is not privileged")
        if not is_controller and not cname.endswith("-csi-ngxstorage") and security.get("privileged"):
            failures.append(f"{name}/{cname}: node sidecar is privileged")
print("\n".join(failures))
PY
)"
if [[ -n "$pod_failures" ]]; then
  while IFS= read -r failure; do failures+=("$failure"); done <<<"$pod_failures"
fi

if [[ -n "$expected_image" ]]; then
  image_failures="$(python3 - "$controller" "$node" "$main_container_name" "$expected_image" "$pod_file" <<'PY'
import json
import sys

controller, node, main_name, expected, pod_file = sys.argv[1:]
with open(pod_file, encoding="utf-8") as stream:
    data = json.load(stream)

failures = []
matched = 0
expected_digest = expected.split("@", 1)[1] if "@sha256:" in expected else ""
for pod in data.get("items", []):
    name = pod.get("metadata", {}).get("name", "")
    if not (name.startswith(controller + "-") or name.startswith(node + "-")):
        continue
    containers = {c.get("name"): c for c in pod.get("spec", {}).get("containers", [])}
    main = containers.get(main_name)
    if main is None:
        failures.append(f"{name}: main driver container {main_name} missing")
        continue
    matched += 1
    if main.get("image") != expected:
        failures.append(f"{name}/{main_name}: image {main.get('image')} != expected {expected}")
    statuses = {c.get("name"): c for c in pod.get("status", {}).get("containerStatuses", [])}
    status = statuses.get(main_name, {})
    if expected_digest:
        image_id = status.get("imageID", "")
        if expected_digest not in image_id:
            failures.append(f"{name}/{main_name}: resolved imageID does not contain expected digest {expected_digest}")

if matched == 0:
    failures.append("no controller/node driver pods matched expected image verification")
print("\n".join(failures))
PY
)"
  if [[ -n "$image_failures" ]]; then
    while IFS= read -r failure; do failures+=("$failure"); done <<<"$image_failures"
  fi
fi

# Plan 11.1 distributed-coordination deployment contract. This verifies live
# resources, not just rendered YAML, so admission/defaulting/drift are covered.
coord_file="$tmp_dir/coordination.json"
if ! oc get lease,role,rolebinding,configmap,deployment,daemonset -n "$namespace" -o json >"$coord_file"; then
  failures+=("cannot read coordination resources in namespace/$namespace")
else
  coordination_failures="$(python3 - "$protocol" "$resource_base" "$main_container_name" "$controller" "$node" "$controller_sa" "$node_sa" "$shard_count" "$coord_file" <<'PY'
import json
import sys

protocol, base, main_name, controller, node, controller_sa, node_sa, shard_count_raw, filename = sys.argv[1:]
count = int(shard_count_raw)
with open(filename, encoding="utf-8") as stream:
    items = json.load(stream).get("items", [])

failures = []

def objects(kind):
    return [item for item in items if item.get("kind") == kind]

def one(kind, name):
    matches = [item for item in objects(kind) if item.get("metadata", {}).get("name") == name]
    if len(matches) != 1:
        failures.append(f"expected one {kind}/{name}, got {len(matches)}")
        return None
    return matches[0]

expected_lease_names = {f"{base}-operation-lock-{index:03d}" for index in range(count)}
actual_lease_names = {
    item.get("metadata", {}).get("name")
    for item in objects("Lease")
    if item.get("metadata", {}).get("name", "").startswith(base + "-operation-lock-")
}
if actual_lease_names != expected_lease_names:
    failures.append(
        f"operation Lease pool mismatch: got={len(actual_lease_names)} expected={count} "
        f"missing={sorted(expected_lease_names-actual_lease_names)[:3]} extra={sorted(actual_lease_names-expected_lease_names)[:3]}"
    )

guard = one("ConfigMap", f"{base}-coordination-config")
if guard is not None:
    if guard.get("immutable") is not True:
        failures.append("coordination ConfigMap is not immutable")
    if guard.get("data") != {"lease-prefix": base, "shard-count": str(count)}:
        failures.append(f"coordination ConfigMap data mismatch: {guard.get('data')}")

def validate_operation_role(name, expected_sa):
    role = one("Role", name)
    binding = one("RoleBinding", name)
    if role is not None:
        rules = role.get("rules", [])
        if len(rules) != 1:
            failures.append(f"{name} must have exactly one rule")
        else:
            rule = rules[0]
            if rule.get("apiGroups") != ["coordination.k8s.io"] or rule.get("resources") != ["leases"]:
                failures.append(f"{name} is not Lease-only")
            if set(rule.get("resourceNames", [])) != expected_lease_names or len(rule.get("resourceNames", [])) != count:
                failures.append(f"{name} resourceNames do not exactly match Lease pool")
            if set(rule.get("verbs", [])) != {"get", "update", "patch"} or len(rule.get("verbs", [])) != 3:
                failures.append(f"{name} has overbroad verbs {rule.get('verbs')}")
    if binding is not None:
        subjects = binding.get("subjects", [])
        if len(subjects) != 1 or subjects[0].get("kind") != "ServiceAccount" or subjects[0].get("name") != expected_sa:
            failures.append(f"{name} must bind only ServiceAccount {expected_sa}")
        ref = binding.get("roleRef", {})
        if ref.get("kind") != "Role" or ref.get("name") != name:
            failures.append(f"{name} RoleBinding roleRef mismatch")

validate_operation_role(f"{base}-controller-operation-lock", controller_sa)

deployment = one("Deployment", controller)
daemonset = one("DaemonSet", node)

def env_map(container):
    return {entry.get("name"): entry for entry in container.get("env", []) if entry.get("name")}

def main_container(workload):
    if workload is None:
        return None
    matches = [c for c in workload["spec"]["template"]["spec"].get("containers", []) if c.get("name") == main_name]
    if len(matches) != 1:
        failures.append(f"cannot identify exactly one main container {main_name}")
        return None
    return matches[0]

controller_main = main_container(deployment)
if controller_main is not None:
    env = env_map(controller_main)
    if env.get("NGX_COORDINATION_LEASE_PREFIX", {}).get("value") != base:
        failures.append("controller coordination prefix env mismatch")
    if env.get("NGX_COORDINATION_SHARD_COUNT", {}).get("value") != str(count):
        failures.append("controller coordination shard-count env mismatch")
    for name, field in (("POD_NAMESPACE", "metadata.namespace"), ("POD_NAME", "metadata.name"), ("POD_UID", "metadata.uid")):
        actual = env.get(name, {}).get("valueFrom", {}).get("fieldRef", {}).get("fieldPath")
        if actual != field:
            failures.append(f"controller {name} must come from {field}")

node_main = main_container(daemonset)
if daemonset is not None and node_main is not None:
    pod = daemonset["spec"]["template"]["spec"]
    env = env_map(node_main)
    token_volume_name = "kube-api-access-ngxstorage"
    token_mounts = [
        (c.get("name"), m)
        for c in pod.get("containers", [])
        for m in c.get("volumeMounts", [])
        if m.get("name") == token_volume_name
    ]
    token_volumes = [v for v in pod.get("volumes", []) if v.get("name") == token_volume_name]

    if protocol == "NFS":
        forbidden = {"NGX_COORDINATION_LEASE_PREFIX", "NGX_COORDINATION_SHARD_COUNT", "POD_NAME", "POD_UID"}
        leaked = forbidden.intersection(env)
        if leaked:
            failures.append(f"NFS node receives coordination env {sorted(leaked)}")
        if token_volumes or token_mounts:
            failures.append("NFS node receives projected coordination credentials")
    else:
        if pod.get("automountServiceAccountToken") is not False:
            failures.append("block node automountServiceAccountToken must be false")
        if env.get("NGX_COORDINATION_LEASE_PREFIX", {}).get("value") != base:
            failures.append("node/controller coordination prefix mismatch")
        if env.get("NGX_COORDINATION_SHARD_COUNT", {}).get("value") != str(count):
            failures.append("node/controller coordination shard-count mismatch")
        validate_operation_role(f"{base}-node-operation-lock", node_sa)
        holder = one("Role", f"{base}-node-holder-pod-read")
        holder_binding = one("RoleBinding", f"{base}-node-holder-pod-read")
        if holder is not None and holder.get("rules") != [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get"]}]:
            failures.append("node holder Pod Role is broader than pods/get")
        if holder_binding is not None:
            subjects = holder_binding.get("subjects", [])
            if len(subjects) != 1 or subjects[0].get("kind") != "ServiceAccount" or subjects[0].get("name") != node_sa:
                failures.append("node holder Pod RoleBinding must bind only node SA")
        if len(token_volumes) != 1 or "projected" not in token_volumes[0]:
            failures.append("projected node API credential volume missing")
        if len(token_mounts) != 1 or token_mounts[0][0] != main_name:
            failures.append("projected API credential must be mounted only by main node container")
        elif token_mounts[0][1].get("mountPath") != "/var/run/secrets/kubernetes.io/serviceaccount" or token_mounts[0][1].get("readOnly") is not True:
            failures.append("projected API credential mount path/readOnly mismatch")

print("\n".join(failures))
PY
)"
  if [[ -n "$coordination_failures" ]]; then
    while IFS= read -r failure; do failures+=("$failure"); done <<<"$coordination_failures"
  fi
fi

if [[ "$(oc auth can-i use "scc/$scc" --as="system:serviceaccount:$namespace:$node_sa" -n "$namespace" 2>/dev/null)" != "yes" ]]; then
  failures+=("node ServiceAccount cannot use $scc")
fi
if [[ "$(oc auth can-i use "scc/$scc" --as="system:serviceaccount:$namespace:$controller_sa" -n "$namespace" 2>/dev/null)" == "yes" ]]; then
  failures+=("controller ServiceAccount can use $scc")
fi

registered=0
for _ in {1..12}; do
  csinode_file="$tmp_dir/csinodes.json"
  oc get csinode -o json >"$csinode_file"
  if python3 - "$driver" "$csinode_file" "$pod_file" "$node" "$desired" <<'PY'
import json
import sys

driver, csinode_file, pod_file, node_prefix, desired = sys.argv[1:]
with open(csinode_file, encoding="utf-8") as stream:
    data = json.load(stream)
with open(pod_file, encoding="utf-8") as stream:
    pods = json.load(stream)
expected_nodes = {
    p.get("spec", {}).get("nodeName") for p in pods.get("items", [])
    if p.get("metadata", {}).get("name", "").startswith(node_prefix + "-")
    and not p.get("metadata", {}).get("deletionTimestamp")
}
registered_nodes = {
    n.get("metadata", {}).get("name") for n in data.get("items", [])
    if driver in {d.get("name") for d in (n.get("spec", {}).get("drivers") or [])}
}
ok = bool(expected_nodes) and None not in expected_nodes and len(expected_nodes) == int(desired) and expected_nodes <= registered_nodes
raise SystemExit(0 if ok else 1)
PY
  then
    registered=1
    break
  fi
  sleep 5
done
if [[ "$registered" != "1" ]]; then
  failures+=("CSINode registration missing for $driver on one or more node-plugin nodes")
fi

if ((${#failures[@]})); then
  echo "[$protocol] post-rollout verification FAILED" >&2
  printf '  %s\n' "${failures[@]}" >&2
  exit 1
fi
if [[ -n "$expected_image" ]]; then
  echo "[$protocol] expected driver image verified: $expected_image"
fi
echo "[$protocol] post-rollout verification PASS; no resources changed"
