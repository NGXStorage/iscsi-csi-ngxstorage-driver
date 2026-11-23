#!/usr/bin/env python3
import pathlib
import shutil
import subprocess
import sys

try:
    import yaml
except ModuleNotFoundError:
    yaml = None

protocol = sys.argv[1]
platform = "all"
if len(sys.argv) > 2:
    if sys.argv[2] != "--platform" or len(sys.argv) < 4 or sys.argv[3] not in {"all", "kubernetes", "openshift"}:
        raise SystemExit("usage: test-manifests.sh [--platform kubernetes|openshift|all]")
    platform = sys.argv[3]

driver_root = pathlib.Path(__file__).resolve().parents[1]
root = driver_root / "deploy" / "manifest"
expected_ns = {"FC": "ngxstorage-fc", "iSCSI": "ngxstorage-iscsi", "NFS": "ngxstorage-nfs"}[protocol]
platforms = ("kubernetes", "openshift") if platform == "all" else (platform,)


def parse(text, label):
    if yaml is None:
        raise RuntimeError("PyYAML is unavailable")
    try:
        return [d for d in yaml.safe_load_all(text) if isinstance(d, dict)]
    except yaml.YAMLError as exc:
        raise SystemExit(f"{protocol}: invalid YAML in {label}: {exc}")


def env_by_name(container):
    return {item.get("name"): item for item in container.get("env", []) if item.get("name")}


def require_downward_env(env, name, field_path, selected):
    actual = env.get(name, {}).get("valueFrom", {}).get("fieldRef", {}).get("fieldPath")
    if actual != field_path:
        raise SystemExit(f"{protocol}/{selected}: {name} must come from {field_path}, got {actual!r}")


def one_named(docs, kind, name, selected):
    matches = [
        doc for doc in docs
        if doc.get("kind") == kind and doc.get("metadata", {}).get("name") == name
    ]
    if len(matches) != 1:
        raise SystemExit(f"{protocol}/{selected}: expected one {kind} {name}, got {len(matches)}")
    return matches[0]


def validate_binding(docs, name, service_account, selected):
    binding = one_named(docs, "RoleBinding", name, selected)
    if binding.get("roleRef", {}).get("kind") != "Role" or binding.get("roleRef", {}).get("name") != name:
        raise SystemExit(f"{protocol}/{selected}: {name} RoleBinding roleRef mismatch")
    subjects = binding.get("subjects", [])
    if len(subjects) != 1 or subjects[0].get("kind") != "ServiceAccount" or subjects[0].get("name") != service_account:
        raise SystemExit(f"{protocol}/{selected}: {name} must bind only ServiceAccount {service_account}")


def validate_operation_role(docs, name, expected_names, selected):
    role = one_named(docs, "Role", name, selected)
    rules = role.get("rules", [])
    if len(rules) != 1:
        raise SystemExit(f"{protocol}/{selected}: {name} must contain exactly one rule")
    rule = rules[0]
    if rule.get("apiGroups") != ["coordination.k8s.io"] or rule.get("resources") != ["leases"]:
        raise SystemExit(f"{protocol}/{selected}: {name} is not Lease-only")
    names = rule.get("resourceNames", [])
    if len(names) != len(expected_names) or set(names) != expected_names:
        raise SystemExit(f"{protocol}/{selected}: {name} resourceNames do not exactly match operation Lease pool")
    if len(rule.get("verbs", [])) != 3 or set(rule.get("verbs", [])) != {"get", "update", "patch"}:
        raise SystemExit(f"{protocol}/{selected}: {name} has overbroad Lease verbs {rule.get('verbs', [])}")


def validate_coordination(docs, selected):
    prefix = f"{protocol.lower()}-csi-ngxstorage"
    # Source validation also parses the controller security strategic-merge
    # patch, which is itself a partial Deployment document. Select the actual
    # workload by the required selector; rendered output contains only this one.
    deployment = next(
        d for d in docs
        if d.get("kind") == "Deployment" and d.get("spec", {}).get("selector")
    )
    daemonset = next(d for d in docs if d.get("kind") == "DaemonSet")
    controller_spec = deployment["spec"]["template"]["spec"]
    node_spec = daemonset["spec"]["template"]["spec"]
    controller_main = [c for c in controller_spec["containers"] if c["name"] == prefix]
    if len(controller_main) != 1:
        raise SystemExit(f"{protocol}/{selected}: main controller container identity mismatch")
    controller_env = env_by_name(controller_main[0])
    if controller_env.get("NGX_COORDINATION_LEASE_PREFIX", {}).get("value") != prefix:
        raise SystemExit(f"{protocol}/{selected}: controller Lease prefix mismatch")
    if controller_env.get("NGX_COORDINATION_SHARD_COUNT", {}).get("value") != "128":
        raise SystemExit(f"{protocol}/{selected}: manual default shard count is not 128")
    for name, field_path in (("POD_NAMESPACE", "metadata.namespace"), ("POD_NAME", "metadata.name"), ("POD_UID", "metadata.uid")):
        require_downward_env(controller_env, name, field_path, selected)

    guard_name = f"{prefix}-coordination-config"
    guard = one_named(docs, "ConfigMap", guard_name, selected)
    if guard.get("immutable") is not True:
        raise SystemExit(f"{protocol}/{selected}: coordination ConfigMap must be immutable")
    if guard.get("data") != {"lease-prefix": prefix, "shard-count": "128"}:
        raise SystemExit(f"{protocol}/{selected}: coordination ConfigMap data mismatch")

    expected_names = {f"{prefix}-operation-lock-{index:03d}" for index in range(128)}
    leases = [doc for doc in docs if doc.get("kind") == "Lease"]
    lease_names = [doc.get("metadata", {}).get("name") for doc in leases]
    if len(leases) != 128 or set(lease_names) != expected_names:
        raise SystemExit(f"{protocol}/{selected}: manual operation Lease pool mismatch: count={len(leases)}")

    controller_role = f"{prefix}-controller-operation-lock"
    validate_operation_role(docs, controller_role, expected_names, selected)
    validate_binding(docs, controller_role, f"{prefix}-controller", selected)

    node_main = [c for c in node_spec["containers"] if c["name"] == prefix]
    if len(node_main) != 1:
        raise SystemExit(f"{protocol}/{selected}: main node container identity mismatch")
    node_main = node_main[0]
    node_env = env_by_name(node_main)
    token_volume_name = "kube-api-access-ngxstorage"

    if protocol == "NFS":
        if any(name in node_env for name in ("NGX_COORDINATION_LEASE_PREFIX", "NGX_COORDINATION_SHARD_COUNT", "POD_NAME", "POD_UID")):
            raise SystemExit(f"{protocol}/{selected}: NFS node unexpectedly receives coordination environment")
        if any(volume.get("name") == token_volume_name for volume in node_spec.get("volumes", [])):
            raise SystemExit(f"{protocol}/{selected}: NFS node unexpectedly receives projected Kubernetes credentials")
        return

    if node_spec.get("automountServiceAccountToken") is not False:
        raise SystemExit(f"{protocol}/{selected}: block node must keep automountServiceAccountToken=false")
    if node_env.get("NGX_COORDINATION_LEASE_PREFIX", {}).get("value") != prefix:
        raise SystemExit(f"{protocol}/{selected}: node/controller Lease prefix mismatch")
    if node_env.get("NGX_COORDINATION_SHARD_COUNT", {}).get("value") != "128":
        raise SystemExit(f"{protocol}/{selected}: node/controller shard count mismatch")
    for name, field_path in (("POD_NAMESPACE", "metadata.namespace"), ("POD_NAME", "metadata.name"), ("POD_UID", "metadata.uid")):
        require_downward_env(node_env, name, field_path, selected)
    require_downward_env(node_env, "KUBE_NODE_NAME", "spec.nodeName", selected)

    token_volumes = [volume for volume in node_spec.get("volumes", []) if volume.get("name") == token_volume_name]
    if len(token_volumes) != 1 or "projected" not in token_volumes[0]:
        raise SystemExit(f"{protocol}/{selected}: projected node API credential volume missing")
    sources = token_volumes[0]["projected"].get("sources", [])
    token_sources = [source.get("serviceAccountToken") for source in sources if source.get("serviceAccountToken")]
    ca_sources = [source.get("configMap") for source in sources if source.get("configMap")]
    ns_sources = [source.get("downwardAPI") for source in sources if source.get("downwardAPI")]
    if len(token_sources) != 1 or token_sources[0].get("path") != "token" or token_sources[0].get("expirationSeconds") != 3600:
        raise SystemExit(f"{protocol}/{selected}: projected serviceAccountToken contract failed")
    if len(ca_sources) != 1 or ca_sources[0].get("name") != "kube-root-ca.crt":
        raise SystemExit(f"{protocol}/{selected}: projected kube-root-ca source missing")
    if len(ns_sources) != 1 or not any(
        item.get("path") == "namespace" and item.get("fieldRef", {}).get("fieldPath") == "metadata.namespace"
        for item in ns_sources[0].get("items", [])
    ):
        raise SystemExit(f"{protocol}/{selected}: projected namespace source missing")

    token_mounts = [
        (container["name"], mount)
        for container in node_spec["containers"]
        for mount in container.get("volumeMounts", [])
        if mount.get("name") == token_volume_name
    ]
    if len(token_mounts) != 1 or token_mounts[0][0] != prefix:
        raise SystemExit(f"{protocol}/{selected}: projected API credential is not main-container-only")
    if token_mounts[0][1].get("mountPath") != "/var/run/secrets/kubernetes.io/serviceaccount" or token_mounts[0][1].get("readOnly") is not True:
        raise SystemExit(f"{protocol}/{selected}: projected API credential mount path/readOnly mismatch")

    node_role = f"{prefix}-node-operation-lock"
    validate_operation_role(docs, node_role, expected_names, selected)
    validate_binding(docs, node_role, f"{prefix}-node", selected)
    pod_role = f"{prefix}-node-holder-pod-read"
    if one_named(docs, "Role", pod_role, selected).get("rules") != [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get"]}]:
        raise SystemExit(f"{protocol}/{selected}: holder Pod liveness Role is broader than pods/get")
    validate_binding(docs, pod_role, f"{prefix}-node", selected)


def validate(docs, selected, rendered=False):
    workloads = [d for d in docs if d.get("kind") in {"Deployment", "DaemonSet"}]
    if not any(d.get("kind") == "Deployment" for d in workloads) or not any(d.get("kind") == "DaemonSet" for d in workloads):
        raise SystemExit(f"{protocol}/{selected}: controller/node workload missing")

    for d in docs:
        meta = d.get("metadata", {})
        if d.get("kind") == "Namespace" and meta.get("name") != expected_ns:
            raise SystemExit(f"{protocol}/{selected}: unexpected Namespace {meta.get('name')}")
        if rendered and meta.get("namespace") and meta["namespace"] != expected_ns:
            raise SystemExit(f"{protocol}/{selected}: rendered namespace drift: {meta['namespace']}")
        if d.get("kind") == "SecurityContextConstraints":
            if selected != "openshift":
                raise SystemExit(f"{protocol}/{selected}: Kubernetes output contains SCC")
            if d.get("users") or d.get("groups"):
                raise SystemExit(f"{protocol}/{selected}: SCC directly grants users/groups")
        if d.get("kind") == "Role":
            for rule in d.get("rules", []):
                if "securitycontextconstraints" in rule.get("resources", []) and "use" in rule.get("verbs", []):
                    if not rule.get("resourceNames"):
                        raise SystemExit(f"{protocol}/{selected}: SCC Role is not resource-scoped")

    validate_coordination(docs, selected)

    if not rendered:
        return

    deployment = next(d for d in docs if d.get("kind") == "Deployment")
    daemonset = next(d for d in docs if d.get("kind") == "DaemonSet")
    controller_pod = deployment["spec"]["template"]["spec"]
    node_pod_template = daemonset["spec"]["template"]
    node_pod = node_pod_template["spec"]
    for container in controller_pod["containers"] + node_pod["containers"]:
        for mount in container.get("volumeMounts", []):
            mount_path = mount.get("mountPath", "")
            if mount_path == "/tmp" or mount_path.startswith("/tmp/"):
                raise SystemExit(
                    f"{protocol}/{selected}: container {container['name']} "
                    f"mounts forbidden runtime path {mount_path}"
                )
    if any(controller_pod.get(k) for k in ("hostNetwork", "hostPID", "hostIPC")):
        raise SystemExit(f"{protocol}/{selected}: controller uses host namespace")
    if any(v.get("hostPath") for v in controller_pod.get("volumes", [])):
        raise SystemExit(f"{protocol}/{selected}: controller uses hostPath")
    for container in controller_pod["containers"]:
        security = container.get("securityContext", {})
        if security.get("privileged") or security.get("allowPrivilegeEscalation") is not False:
            raise SystemExit(f"{protocol}/{selected}: controller {container['name']} escalates")
        if security.get("readOnlyRootFilesystem") is not True or "ALL" not in security.get("capabilities", {}).get("drop", []):
            raise SystemExit(f"{protocol}/{selected}: controller {container['name']} hardening incomplete")
    main = [c for c in node_pod["containers"] if c["name"].endswith("-csi-ngxstorage")]
    if len(main) != 1 or main[0].get("securityContext", {}).get("privileged") is not True or main[0].get("securityContext", {}).get("readOnlyRootFilesystem") is not True:
        raise SystemExit(f"{protocol}/{selected}: node main container privilege contract failed")
    required_mounts = {"/csi", "/var/lib/kubelet", "/host", "/dev"}
    mount_paths = {m.get("mountPath") for m in main[0].get("volumeMounts", [])}
    if not required_mounts.issubset(mount_paths):
        raise SystemExit(f"{protocol}/{selected}: node main mounts are incomplete")
    for mount in main[0].get("volumeMounts", []):
        if mount.get("mountPath") in {"/var/lib/kubelet", "/host"} and mount.get("mountPropagation") != "Bidirectional":
            raise SystemExit(f"{protocol}/{selected}: {mount['mountPath']} is not Bidirectional")
    expected_log = f"/host/var/log/ngxstorage/{protocol.lower()}"
    if expected_log not in {e.get("value") for e in main[0].get("env", []) if e.get("name") == "NGXSTORAGE_CSI_LOG_DIR"}:
        raise SystemExit(f"{protocol}/{selected}: node log path contract failed")
    expected_port = {"FC": 9811, "iSCSI": 9812, "NFS": 9809}[protocol]
    if main[0].get("livenessProbe", {}).get("httpGet", {}).get("port") != expected_port:
        raise SystemExit(f"{protocol}/{selected}: node liveness port contract failed")
    liveness_ports = [
        c.get("livenessProbe", {}).get("httpGet", {}).get("port")
        for c in node_pod["containers"]
    ]
    liveness_ports = [port for port in liveness_ports if port is not None]
    if len(liveness_ports) != len(set(liveness_ports)):
        raise SystemExit(f"{protocol}/{selected}: duplicate node liveness port")
    if controller_pod.get("metadata", {}).get("annotations", {}).get("openshift.io/required-scc"):
        raise SystemExit(f"{protocol}/{selected}: controller requests node SCC")
    for container in node_pod["containers"]:
        if container is main[0]:
            continue
        security = container.get("securityContext", {})
        if security.get("privileged") or security.get("allowPrivilegeEscalation") is not False:
            raise SystemExit(f"{protocol}/{selected}: node sidecar {container['name']} escalates")
        if security.get("readOnlyRootFilesystem") is not True or "ALL" not in security.get("capabilities", {}).get("drop", []):
            raise SystemExit(f"{protocol}/{selected}: node sidecar {container['name']} hardening incomplete")

    sccs = [d for d in docs if d.get("kind") == "SecurityContextConstraints"]
    roles = [d for d in docs if d.get("kind") == "Role"]
    bindings = [d for d in docs if d.get("kind") == "RoleBinding"]
    if selected == "kubernetes":
        if sccs:
            raise SystemExit(f"{protocol}/{selected}: Kubernetes output contains SCC")
    else:
        if len(sccs) != 1:
            raise SystemExit(f"{protocol}/{selected}: expected exactly one SCC")
        scc_name = sccs[0]["metadata"]["name"]
        annotations = node_pod_template.get("metadata", {}).get("annotations", {})
        if annotations.get("openshift.io/required-scc") != scc_name:
            raise SystemExit(f"{protocol}/{selected}: required-scc mismatch")
        use_roles = [r for r in roles if any(
            rule.get("resources") == ["securitycontextconstraints"]
            and "use" in rule.get("verbs", [])
            and scc_name in rule.get("resourceNames", [])
            for rule in r.get("rules", [])
        )]
        if len(use_roles) != 1:
            raise SystemExit(f"{protocol}/{selected}: exact SCC use Role missing or duplicated")
        role_name = use_roles[0]["metadata"]["name"]
        matching = [b for b in bindings if b.get("roleRef", {}).get("name") == role_name]
        expected_sa = f"{protocol.lower()}-csi-ngxstorage-node"
        if len(matching) != 1 or [s.get("name") for s in matching[0].get("subjects", [])] != [expected_sa]:
            raise SystemExit(f"{protocol}/{selected}: SCC RoleBinding subject is not only node SA")


for selected in platforms:
    renderer = shutil.which("kustomize") or shutil.which("oc")
    source_paths = [root / "base"] + [root / selected]
    docs = []
    if yaml is not None:
        for source_root in source_paths:
            for path in sorted(source_root.glob("*.yaml")):
                docs.extend(parse(path.read_text(), str(path)))
        validate(docs, selected)
    elif renderer is None:
        raise SystemExit(f"{protocol}/{selected}: PyYAML or a Kustomize renderer is required")

    if renderer:
        command = [renderer, "kustomize", str(root / selected)] if pathlib.Path(renderer).name == "oc" else [renderer, "build", str(root / selected)]
        rendered = subprocess.check_output(command, text=True, stderr=subprocess.STDOUT)
        if yaml is None:
            print(f"{protocol}/{selected}: Kustomize render passed (PyYAML unavailable; semantic parse skipped)")
        else:
            validate(parse(rendered, f"{protocol}/{selected} rendered output"), selected, rendered=True)
            print(f"{protocol}/{selected}: Kustomize render and security contract passed")
    else:
        print(f"{protocol}/{selected}: source YAML/SCC contract passed (Kustomize renderer unavailable)")
