# Deployment documentation

Use the requirements and compatibility notes for the target platform, then select one deployment method below. Follow only one method for a given CSI identity; do not install raw manifests and Helm over the same resources.

| Platform | Raw manifests | Helm |
| --- | --- | --- |
| Kubernetes | [Manifest guide](kubernetes/manifest/DEPLOYMENT.md) | [Helm guide](kubernetes/helm/DEPLOYMENT.md) |
| OpenShift | [Manifest guide](openshift/manifest/DEPLOYMENT.md) | [Helm guide](openshift/helm/DEPLOYMENT.md) |

The canonical manual commands are in [deploy/README.md](../../deploy/README.md). Helm chart values and chart documentation are maintained in the separate workspace `Helm` repository, which is not part of this clone; the raw-manifest guides are fully self-contained here, while Helm installation requires that repository (chart publication is a separate release step).
