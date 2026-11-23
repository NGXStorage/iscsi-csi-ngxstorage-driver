# Kubernetes requirements

- Linux workers with protocol configuration: `iscsiadm`, an initiator IQN, reachable storage portals over TCP/3260, and expected multipath configuration where used. Verify target login and device discovery from the host.
- Permit the privileged CSI node plugin, host root/device/sysfs access, and kubelet mount propagation used by the DaemonSet.
- Controller pods need NGX Storage management HTTPS connectivity. Workers need management access and protocol data-path connectivity.
- A backend pool enabled for iSCSI and a valid runtime API key.
- Snapshot workflows need `snapshot.storage.k8s.io/v1` CRDs and a snapshot controller.
- Create Secret `iscsi-csi-ngxstorage-secret` with `storage-ips`, `pool-name`, `portal-group`, and `api-key`. Never commit credentials or print them in logs.
- The Kubernetes overlay has no SCC objects. Cluster policy must permit the node host access described in the security note.

The checked-in namespace is `ngxstorage-iscsi`. If changing it, inspect rendered namespaced and cluster-scoped references. See [manual deployment](../../../deploy/README.md).
