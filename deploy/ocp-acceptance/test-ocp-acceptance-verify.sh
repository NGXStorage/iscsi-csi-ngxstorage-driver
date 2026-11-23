#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp_root="$HOME/.cache/ngxstorage"
mkdir -p "$tmp_root"
tmp="$(mktemp -d "$tmp_root/ngxstorage-verifier-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
expected_image="quay.io/ngxstorage/fc-csi-ngxstorage@$digest"
base="201-fc-csi-ngxstorage"
namespace="fixture-ns"

python3 - "$tmp" "$base" "$namespace" "$expected_image" "$digest" <<'PY'
import json
import pathlib
import sys

out = pathlib.Path(sys.argv[1])
base, namespace, image, digest = sys.argv[2:]
controller = f"{base}-controller"
node = f"{base}-node"
controller_sa = controller
node_sa = node
main = "fc-csi-ngxstorage"

def env():
    return [
        {"name": "NGX_COORDINATION_LEASE_PREFIX", "value": base},
        {"name": "NGX_COORDINATION_SHARD_COUNT", "value": "32"},
        {"name": "POD_NAMESPACE", "valueFrom": {"fieldRef": {"fieldPath": "metadata.namespace"}}},
        {"name": "POD_NAME", "valueFrom": {"fieldRef": {"fieldPath": "metadata.name"}}},
        {"name": "POD_UID", "valueFrom": {"fieldRef": {"fieldPath": "metadata.uid"}}},
    ]

def workload(kind, name, node_workload=False):
    spec = {
        "containers": [
            {
                "name": main,
                "image": image,
                "env": env(),
                "securityContext": {
                    "readOnlyRootFilesystem": True,
                    **({"privileged": True} if node_workload else {}),
                },
            }
        ]
    }
    if node_workload:
        spec["automountServiceAccountToken"] = False
        spec["containers"][0]["volumeMounts"] = [{
            "name": "kube-api-access-ngxstorage",
            "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
            "readOnly": True,
        }]
        spec["volumes"] = [{
            "name": "kube-api-access-ngxstorage",
            "projected": {"sources": [{"serviceAccountToken": {"path": "token"}}]},
        }]
    return {
        "apiVersion": "apps/v1",
        "kind": kind,
        "metadata": {"name": name, "namespace": namespace},
        "spec": {"selector": {"matchLabels": {"app": name}}, "template": {"spec": spec}},
        "status": ({"desiredNumberScheduled": 1, "numberReady": 1} if kind == "DaemonSet" else {}),
    }

leases = [
    {
        "apiVersion": "coordination.k8s.io/v1",
        "kind": "Lease",
        "metadata": {"name": f"{base}-operation-lock-{i:03d}", "namespace": namespace},
        "spec": {},
    }
    for i in range(32)
]
names = [item["metadata"]["name"] for item in leases]

def role(name, rule):
    return {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"name": name}, "rules": [rule]}

def binding(name, sa):
    return {
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "RoleBinding",
        "metadata": {"name": name},
        "roleRef": {"kind": "Role", "name": name},
        "subjects": [{"kind": "ServiceAccount", "name": sa}],
    }

operation_rule = {
    "apiGroups": ["coordination.k8s.io"],
    "resources": ["leases"],
    "resourceNames": names,
    "verbs": ["get", "update", "patch"],
}

coord_items = leases + [
    {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": f"{base}-coordination-config"},
        "immutable": True,
        "data": {"lease-prefix": base, "shard-count": "32"},
    },
    role(f"{base}-controller-operation-lock", operation_rule),
    binding(f"{base}-controller-operation-lock", controller_sa),
    role(f"{base}-node-operation-lock", operation_rule),
    binding(f"{base}-node-operation-lock", node_sa),
    role(f"{base}-node-holder-pod-read", {"apiGroups": [""], "resources": ["pods"], "verbs": ["get"]}),
    binding(f"{base}-node-holder-pod-read", node_sa),
    workload("Deployment", controller),
    workload("DaemonSet", node, True),
]

pods = {
    "items": [
        {
            "metadata": {"name": f"{controller}-abc", "annotations": {"openshift.io/scc": "restricted-v2"}},
            "spec": {"containers": [{"name": main, "image": image, "securityContext": {"readOnlyRootFilesystem": True}}]},
            "status": {"containerStatuses": [{"name": main, "imageID": f"docker-pullable://quay.io/ngxstorage/fc-csi-ngxstorage@{digest}"}]},
        },
        {
            "metadata": {"name": f"{node}-abc", "annotations": {"openshift.io/scc": f"{base}-node-scc"}},
            "spec": {"nodeName": "worker1", "containers": [{"name": main, "image": image, "securityContext": {"readOnlyRootFilesystem": True, "privileged": True}}]},
            "status": {"containerStatuses": [{"name": main, "imageID": f"docker-pullable://quay.io/ngxstorage/fc-csi-ngxstorage@{digest}"}]},
        },
    ]
}

(out / "daemonset.json").write_text(json.dumps({"status": {"desiredNumberScheduled": 1, "numberReady": 1}}))
(out / "pods.json").write_text(json.dumps(pods))
(out / "coord.json").write_text(json.dumps({"items": coord_items}))
(out / "csinode.json").write_text(json.dumps({"items": [{"metadata": {"name": "master0"}, "spec": {"drivers": None}}, {"metadata": {"name": "worker1"}, "spec": {"drivers": [{"name": "201.fc.csi.ngxstorage.com"}]}}]}))
PY

cat > "$tmp/oc" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
args=" $* "
case "$args" in
  *" get namespace fixture-ns "*) exit 0 ;;
  *" rollout status deployment/201-fc-csi-ngxstorage-controller "*) exit 0 ;;
  *" get daemonset 201-fc-csi-ngxstorage-node "*) cat "$FIXTURE_DIR/daemonset.json" ;;
  *" get pods -n fixture-ns -o json "*) cat "$FIXTURE_DIR/pods.json" ;;
  *" get lease,role,rolebinding,configmap,deployment,daemonset -n fixture-ns -o json "*) cat "$FIXTURE_DIR/coord.json" ;;
  *" auth can-i use scc/201-fc-csi-ngxstorage-node-scc "*)
    if [[ "$args" == *" --as=system:serviceaccount:fixture-ns:201-fc-csi-ngxstorage-node "* ]]; then
      echo yes
    else
      echo no
    fi
    ;;
  *" get csinode -o json "*) cat "$FIXTURE_DIR/csinode.json" ;;
  *) echo "unexpected fake oc invocation:$args" >&2; exit 90 ;;
esac
SH
chmod +x "$tmp/oc"

export FIXTURE_DIR="$tmp"
PATH="$tmp:$PATH" "$root/ocp-acceptance-verify.sh" FC \
  --namespace "$namespace" --prefix 201 --shard-count 32 --expected-image "$expected_image" \
  > "$tmp/pass.out"
grep -q 'post-rollout verification PASS' "$tmp/pass.out"
grep -q 'expected driver image verified' "$tmp/pass.out"

set +e
PATH="$tmp:$PATH" "$root/ocp-acceptance-verify.sh" FC \
  --namespace "$namespace" --prefix 201 --shard-count 32 \
  --expected-image "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" \
  > "$tmp/fail.out" 2> "$tmp/fail.err"
rc=$?
set -e
if [[ "$rc" -eq 0 ]]; then
  echo "wrong expected image unexpectedly passed verifier" >&2
  exit 1
fi
grep -q 'image .* != expected' "$tmp/fail.err"

# A second ready node pod without its registration must fail; do not wait in fixtures.
python3 - "$tmp" <<'PYFIXTURE'
import copy,json,pathlib,sys
root=pathlib.Path(sys.argv[1])
pods=json.loads((root/'pods.json').read_text())
other=copy.deepcopy(pods['items'][1])
other['metadata']['name'] += '-second'
other['spec']['nodeName']='worker2'
pods['items'].append(other)
(root/'pods.json').write_text(json.dumps(pods))
(root/'daemonset.json').write_text(json.dumps({'status':{'desiredNumberScheduled':2,'numberReady':2}}))
PYFIXTURE
printf '#!/bin/sh\nexit 0\n' > "$tmp/sleep"
chmod +x "$tmp/sleep"
if PATH="$tmp:$PATH" "$root/ocp-acceptance-verify.sh" FC \
  --namespace "$namespace" --prefix 201 --shard-count 32 --expected-image "$expected_image" \
  > "$tmp/missing-node.out" 2> "$tmp/missing-node.err"; then
  echo "missing worker registration unexpectedly passed verifier" >&2
  exit 1
fi
grep -q 'CSINode registration missing' "$tmp/missing-node.err"
echo "OCP verifier prefix/image/null-master/all-node registration fixtures PASS"
