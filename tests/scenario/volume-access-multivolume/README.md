# High-count multivolume teardown

This scenario uses the shared protocol-neutral FC runner with the iSCSI
provisioner selected by PROTOCOL. It creates 16 distinct 1Gi PVCs, mounts
them in one worker-pinned Pod, verifies a unique marker on each mount, and
then requires exact VolumeAttachment, mount, multipath-map, and path-WWN
teardown after deleting the Pod once.

Run: `BENCH=ocp2 bash tests/run-all-tests.sh scenario volume-access-multivolume --allow-live`.
Provide the runtime NGX Storage settings described in `tests/README.md` first.

The acceptance count is 16 (MULTIVOLUME_COUNT supports 2–32 for diagnostics).
The test does not perform host cleanup. Capture deployed image digests with
the evidence, and do not use --keep.
