# Kubernetes external storage E2E

Run this suite only against an already deployed driver and a Kubernetes-version
matched `e2e.test` binary:

```bash
KUBECONFIG=/path/to/kubeconfig \
E2E_TEST_BINARY=/path/to/e2e.test \
bash tests/external-e2e/run.sh
```

The DriverDefinition advertises only capabilities that NGX Storage iSCSI CSI supports.
It intentionally excludes raw block, inline ephemeral volumes, direct volume
cloning, fsGroup ownership changes, node expansion, and GroupController.

If the target cluster already has the cluster-scoped
`volumepopulators.populator.storage.k8s.io` CRD, `run.sh` preserves it and
automatically skips only the upstream any-volume-data-source case that tries
to create the same CRD. This is an environmental skip; the driver is not
modified and the CRD is never deleted.

Control-plane and master role taints do not block suite startup. Worker readiness
remains required (`E2E_ALLOWED_NOT_READY_NODES=0`). Override the comma-delimited
`E2E_NON_BLOCKING_TAINTS` only for the target cluster's scheduling contract;
this setting does not remove node taints or schedule workloads onto masters.

When including disruptive kubelet/SSH cases, set KUBE_SSH_USER and
KUBE_SSH_KEY_PATH for the worker nodes and verify passwordless sudo beforehand.
Keep the private key outside the repository and remove temporary credentials
after the campaign. Excluding stress does not exclude disruptive cases.

JUnit reports are saved under tests/external-e2e/logs by default. Set
E2E_REPORT_DIR to a separate evidence directory for each campaign attempt.

The suite timeout defaults to four hours. Set `E2E_TIMEOUT` for a different
budget; individual operation timeouts remain unchanged.

Kubernetes v1.33.13's restore-to-larger test deletes its source snapshot before
the restored consumer. For backends that retain a snapshot dependency, apply
`kubernetes-1.33-snapshot-cleanup.patch` to that exact Kubernetes source tag and
build `e2e.test` with `go test -c ./test/e2e`. The patch deletes the restored pod
and PVC and waits for its PV deletion before snapshot cleanup. Capacity and data
assertions are unchanged. Select the rebuilt binary through `E2E_TEST_BINARY`
and record its source tag, patch hash, and binary hash with the results; report
this as a patched harness run separately from stock upstream results.
