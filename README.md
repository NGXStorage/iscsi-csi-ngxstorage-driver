# NGX Storage iSCSI CSI Driver

The NGX Storage iSCSI CSI driver provisions LUNs from the array and connects Linux workers to them over iSCSI. Workloads can consume each LUN as a filesystem or as a raw block device.

## Features

- Dynamic provisioning of LUNs from a selected pool and portal group.
- Filesystem and raw block volumes.
- Snapshots and volume expansion through the CSI API.
- Multipath and initiator IQN handling on the workers.
- Kubernetes and OpenShift deployments with manifests or Helm charts.
- Standalone build from the committed Go dependencies.

## Prerequisites

Configure an initiator IQN, `iscsiadm` and multipath on each storage worker. Select the array pool and portal group. Workers need a route to the group's data portals, and both the controller and node plugins need access to the array management API.

| Platform | Requirements |
| --- | --- |
| Kubernetes | [Worker and storage requirements](docs/deployment/kubernetes/REQUIREMENTS.md) |
| OpenShift | [Worker, storage and SCC requirements](docs/deployment/openshift/REQUIREMENTS.md) |

## Install

| Platform | Installation |
| --- | --- |
| Kubernetes | [Manifests](docs/deployment/kubernetes/manifest/DEPLOYMENT.md) · [Helm](docs/deployment/kubernetes/helm/DEPLOYMENT.md) |
| OpenShift | [Manifests](docs/deployment/openshift/manifest/DEPLOYMENT.md) · [Helm](docs/deployment/openshift/helm/DEPLOYMENT.md) |

The manifest bundle registers `iscsi.csi.ngxstorage.com` and creates the StorageClass `iscsi-csi-ngxstorage`.

## First volume

Once the controller and node pods are ready, [provision a volume and verify that a pod can write to and read from it](docs/first-volume.md).

## Build and test

Run `make build` to compile the driver from the committed Go dependencies. For container images, test commands and configuration, see the [build guide](build/README.md), the [test guide](tests/README.md) and the [documentation index](docs/README.md).

## License

Licensed under [Apache 2.0](LICENSE).
