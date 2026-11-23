# iSCSI scenario guide

Scenario cases and fixtures are owned by `tests/scenario/`. Inspect available cases with `bash tests/run-all-tests.sh list`, then review the selected runner and target profile before starting one scenario:

```bash
BENCH=ocp2 bash tests/run-all-tests.sh scenario <scenario-name> --allow-live
```

Cases can create backend and cluster resources. Verify protocol data I/O, normal detach/unmount, and cleanup, including residual device/session/mount state. Review case-specific disruption or stress flags before use. Passing unit tests, manifest rendering, or pod rollout is not scenario acceptance.

Save run results outside the repository with the source/image identity, command, outcome and cleanup evidence.
