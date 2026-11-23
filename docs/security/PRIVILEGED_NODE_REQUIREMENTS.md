# Privileged node requirements for NGX Storage iSCSI CSI

The node plugin runs privileged because it performs host-level device and mount operations. The DaemonSet accesses host root, kubelet paths, and device resources; the controller does not need those host mounts.

Protocol-specific work includes `iscsiadm`, an initiator IQN, reachable storage portals over TCP/3260, and expected multipath configuration where used. Verify target login and device discovery from the host.

| Component | Host access | Purpose |
| --- | --- | --- |
| Controller | No host mount/device access | CSI control plane and NGX Storage management API operations |
| Node plugin | Privileged host access | Device discovery and volume mount lifecycle |

OpenShift uses `iscsi-csi-ngxstorage-node-scc` through its node ServiceAccount. Verify the effective pod security context after deployment; SCC YAML alone is not certification approval.
