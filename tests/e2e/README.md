# Custom Kubernetes E2E

This client-go suite tests the deployed iSCSI CSI driver.

```bash
KUBECONFIG=/path/to/kubeconfig \
  bash tests/run-all-tests.sh e2e --allow-live
```

The suite covers dynamic provisioning and mounted filesystem expansion. The
resize test mounts a 1 GiB PVC, writes a marker, requests 2 GiB, then checks PVC
capacity, actual filesystem growth and the preserved marker. A block driver's
filesystem resize needs a pod; waiting on an unmounted PVC can leave it in
`FileSystemResizePending` after controller expansion already succeeded.

API requests have a 30-second timeout and polling uses its own context. The
suite context is 30 minutes; the public runner gives Go 45 minutes so a test
failure can run cleanup before the outer process timeout. Cleanup requests
remain usable after the suite context is canceled. Logs include PVC resize
state changes and measured filesystem sizes.

For direct execution from this directory, set `E2E_RUN=1`, `KUBECONFIG` and
`TMPDIR=$HOME/.cache/ngxstorage`, create that scratch directory, and run
`go test -timeout=45m -count=1 -v ./...`. Without `E2E_RUN=1`, live cases skip.
