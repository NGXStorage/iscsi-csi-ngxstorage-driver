# iSCSI host reboot recovery

Run through the driver test entrypoint:

```bash
BENCH=ocp2 bash tests/run-all-tests.sh scenario volume-recovery-host-reboot --allow-live
```

The test selects a Ready, schedulable worker without any CSI controller,
places the workload there and verifies its checksum. It cordons that worker,
requests a normal reboot, and moves the workload to another worker through
normal Kubernetes rollout and attachment cleanup. Success requires the same
checksum on the replacement, a changed boot ID, and the original worker Ready.
Teardown uncordons only the recorded test worker after it is Ready.

This covers graceful reboot and workload recovery. It does not prove recovery
from a powered-off or fenced node. The automated test never applies the
`node.kubernetes.io/out-of-service` taint or bypasses attachment finalizers.
Manual non-graceful shutdown testing requires independent power-off proof;
a reboot request or NotReady condition alone is insufficient.
See [Kubernetes node shutdowns](https://v1-33.docs.kubernetes.io/docs/concepts/cluster-administration/node-shutdown/).
