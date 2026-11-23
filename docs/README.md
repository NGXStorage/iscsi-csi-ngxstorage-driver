# iSCSI driver documentation

## Install and use the driver

| Task | Guide |
| --- | --- |
| Check worker prerequisites and configure the array | [Kubernetes](deployment/kubernetes/REQUIREMENTS.md) · [OpenShift](deployment/openshift/REQUIREMENTS.md) |
| Choose manifests or Helm | [Installation by platform](deployment/README.md) |
| Create a claim, mount it and check I/O | [Your first volume](first-volume.md) |
| Check platform and sidecar versions | [Kubernetes compatibility](deployment/kubernetes/COMPATIBILITY.md) · [OpenShift compatibility](deployment/openshift/COMPATIBILITY.md) |
| Configure management API trust or review worker privileges | [TLS](security/TLS.md) · [Node plugin permissions](security/PRIVILEGED_NODE_REQUIREMENTS.md) |
| Diagnose an OpenShift image pull failure | [Image pull troubleshooting](support/OPENSHIFT_IMAGE_PULL_TROUBLESHOOTING.md) |
| Change or roll back a driver image | [Upgrade guide](upgrade/UPGRADE.md) |

## Build and test

- [Build the driver image](../build/README.md) and [identify its version](release/VERSIONING.md).
- [Run unit tests](testing/unit/GUIDE.md) or [check the CSI interface with csi-sanity](testing/sanity/GUIDE.md).
- [Exercise Kubernetes provisioning](testing/e2e/GUIDE.md) or [run iSCSI storage scenarios](testing/scenario/GUIDE.md).
- [Code conventions](development/CODE_RULES.md) and [test runner options](../tests/README.md).
