# Kubernetes E2E guide

The driver has a custom client-go suite under `tests/e2e` and external Kubernetes storage tests under `tests/external-e2e`. Both target a deployed driver and may create cluster and backend resources. Select and inspect the target cluster and image first.

Run the custom suite from `tests/e2e` with `E2E_RUN=1`, `KUBECONFIG`, and task-scoped `GOCACHE`/`GOMODCACHE` under `$HOME/.cache/ngxstorage`. External tests need `KUBECONFIG` and a Kubernetes-version-matched `E2E_TEST_BINARY`; start with `bash tests/run-all-tests.sh external-e2e --allow-live` from the driver root.

Follow the test READMEs for prerequisites and capability exclusions. Save each run's source/image identity, result and cleanup evidence outside the repository.
