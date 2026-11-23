# iSCSI multipath attach and host teardown

Run one scenario with `BENCH=ocp2 bash tests/run-all-tests.sh scenario volume-attach-multipath --allow-live`.
Provide the runtime NGX Storage settings described in `tests/README.md` first.
It provisions a 1 GiB test volume, verifies marker I/O and at least two iSCSI
paths to one WWN, then deletes the test Pod. A PASS also requires the
VolumeAttachment to disappear and the Pod mount plus the exact recorded
`mpath-*` device-mapper UUID and path WWN to be absent on that worker. The pre/post host
identity evidence is saved under the run's `logs/.../host/` directory. The
test never logs out a session directly: other volumes may share it.
This scenario uses a minimal StorageClass so optional LUN parameter validation
does not prevent the host-teardown test from reaching attach. The FC OCP2
array rejected `blocksize="128k"` and `qos_priority=16` separately with code
5008 on 2026-09-29; those API/parameter checks are a separate issue.

`--keep` preserves the Pod, so it intentionally skips teardown validation.
That run is attach-only evidence, not a host-teardown acceptance result.
The running CSI node image must correspond to the source revision being
accepted; an older deployment cannot validate local teardown changes.
