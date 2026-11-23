# NGX Storage iSCSI CSI versioning

The source currently declares driver version `1.0.0`. The refactor target `2.0.0` is not released. Do not infer release status from a local build or image tag.

Use `make docker-driver` for a local build. Use `make docker-push REGISTRY_TARGET=quay` only for an authorized publish from committed source. Verify source commit and registry digest before deployment; never overwrite a release tag.

Record driver version, full source commit, image digest, target platform, and manifest revision together. A driver image release does not change cluster state automatically.
