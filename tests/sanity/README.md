# CSI sanity tests

Use one public entrypoint and select the environment:

```bash
bash tests/sanity/run.sh local --allow-live
bash tests/sanity/run.sh cloud --allow-live
```

Both modes create real backend resources. Check the selected storage and cluster
configuration before running. Run local mode as your normal user; its runner
requests sudo for host operations.

## Local mode

The default is the complete suite against a driver created by the existing Go
test binary, with process-local coordination and node lookup. This tests current
source and real storage/mount operations. It does not validate Kubernetes Lease
coordination or fence another driver process. Use an isolated test pool and run
one suite at a time.

The host needs the protocol's devices, network access and mount tools. The
runner prepares host paths and carries the user toolchain through sudo.
Logs are under `tests/sanity/local/logs/`.

The separate-process method remains available internally in `local/run.sh`.
It requires real coordination identity, service-account access and Lease RBAC;
a kubeconfig alone does not replace those prerequisites. It is not the default
local path.

## Cloud mode

Cloud tests the already deployed node and controller services. The test client
runs inside a node pod and reaches its Unix socket. A temporary TCP-to-Unix
proxy in the controller pod exposes the controller socket to that client.
It uses existing pod permissions and volumes; no driver rollout or new SCC/RBAC
is needed.

The node pod maps the host plugin directory to `/csi`; its absolute host path
differs from `/csi`. Mount and staging paths therefore use the host-visible
`/var/lib/kubelet/plugins/<driver>/sanity-out-<run>/` directory. `/csi` holds
injected binaries; that container path is unsuitable for host mount targets. The node pod's bidirectional kubelet mount preserves the path across
container and host namespaces.

The runner uploads `tests/scenario/sanity/params.yaml` and
`cloud/mutableparams.yaml` to the selected pod. Missing files fail the run;
there is no silent empty-parameter fallback. Mutable parameters exercise actual
attribute updates. CSI secrets are empty because the running driver obtains
its backend credentials from its existing Secret environment.

Suite output, JUnit and backend observations are stored under the scenario's
`logs/<bench>-<stamp>/` directory. Teardown removes injected helpers and the
run's scratch directory. A remaining scratch mount or failed mount inspection
prevents deletion and fails cleanup.

For a diagnostic subset, use `SANITY_FOCUS` or `SANITY_FOCUS_FILE` in cloud
mode. File and line numbers depend on the checked-out test library. A focused
run must match at least one test and is not full-suite acceptance.

## Configuration

```bash
cp tests/sanity/local/config.env.example tests/sanity/local/config.env
cp tests/sanity/cloud/config.env.example tests/sanity/cloud/config.env
```

Real `config.env` files are ignored by Git. Supply `STORAGE_API_KEY` through
that file or the runtime environment. Local configuration includes controller
addresses, pool and the protocol-specific data IP, FC targets or portal group.
Cloud configuration selects `BENCH=k8s|ocp2`, kubeconfig and the installed driver
namespace. `KUBE_CONTEXT` asserts the current context without changing it.
Use explicit settings for the intended backend rather than assuming lab defaults
identify the target.

Build the test tools through `tests/run-all-tests.sh local` before cloud runs.
That command runs source checks and produces the SHARED csi-sanity binary —
built from the pinned csi-test fork commit, never a driver-local copy — along
with its race/proxy helpers into the home cache. It does not run live acceptance.

## Acceptance evidence

Record passed, failed, pending and skipped cases separately. Unsupported
capabilities can skip their own cases; `GroupController` is excluded because
these drivers do not expose that service. Any additional filter needs an
explicit reason. A suite error or failed cleanup cannot be reported as PASS.

Current-source local results and deployed-image cloud results are distinct.
Record the source revision, deployed digest and configuration with each result;
a successful test does not establish push, release or upgrade acceptance.
