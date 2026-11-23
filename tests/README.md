# iSCSI test layout

Test scope is explicit:

- `run-unit-tests.sh`: root unit tests plus `tests/e2e` compile/skip;
- `pkg/*_test.go`: deterministic package tests;
- `tests/e2e`: Kubernetes client-go tests, live only with `E2E_RUN=1`;
- `tests/sanity`: csi-sanity configuration and live runner;
- `tests/external-e2e`: image/cluster acceptance harness.

Run `bash tests/run-all-tests.sh local` for the full local gate. Scenario bodies,
their runner and protocol test data are owned by this driver under
`tests/scenario/`; test support binaries are built from `tests/tools/` into
`$HOME/.cache/ngxstorage`. No scenario source is loaded from the Integrations
`Tests/` tree.

Before a live scenario, export `NGX_CONTROLLER`, `STORAGE_API_KEY`, and
`NGX_POOL_NAME` in the shell from an approved runtime profile/secret store.
The SDK CLI is built from the Go SDK into `tests/tools/bin/ngxstorage-cli`;
set `NGX_SDK_CLI` only to override that local copy. Never put credentials in
command history. Then run `BENCH=ocp2 bash tests/run-all-tests.sh scenario <name>
--allow-live` (or use `BENCH=k8s` where supported). Use
`bash tests/run-all-tests.sh list` to inspect scenario coverage. Live suites
require an explicit `--allow-live`; host reboot and stress scenarios need
additional flags. `all` combines local and ordinary live suites only after
the required environment is supplied.
