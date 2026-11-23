# CSI sanity guide

The driver-local entry point is `bash tests/run-all-tests.sh sanity local --allow-live` for backend sanity. Cloud mode targets an already deployed driver: `bash tests/run-all-tests.sh sanity cloud --allow-live`. Inspect the cluster context, profile, and target driver before allowing live operations. Run one mutating suite at a time.

Local sanity tests a local process, not Kubernetes Lease coordination or deployed pods. Cloud sanity uses deployed node/controller processes and requires a current cluster profile. Both may create and delete backend volumes. Examples are under `tests/sanity/{local,cloud}/config.env.example`; keep credentials out of reports and command history.

Save run results outside the repository with the source/image identity, command, outcome and cleanup evidence.
