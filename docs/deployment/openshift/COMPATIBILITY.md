# OpenShift compatibility

## Current review baseline

| Property | Baseline | Acceptance |
| --- | --- | --- |
| Platform | OpenShift 4.20.30 | Workload acceptance pending |
| Kubernetes payload | 1.33.13 | Workload acceptance pending |
| Workers | Three | Topology recorded; no full workload acceptance claimed |
| Architecture | Linux amd64 build target | Other architectures are not claimed |
| Driver version | 1.0.0 | See [versioning](../../release/VERSIONING.md) |

This is the current environment baseline, not a compatibility certification. Record cluster version, node OS/architecture, image digest, source revision, manifest revision, and protocol prerequisites for each acceptance run.

The CSI driver name is `iscsi.csi.ngxstorage.com`. The source implements node staging/publishing and controller volume lifecycle; snapshot workflows also require platform snapshot APIs. The OpenShift overlay includes the node SCC and ServiceAccount binding. Manifest presence alone does not prove effective SCC assignment.

Build, render, or rollout success does not prove protocol login, mount, I/O, expansion, or cleanup.
