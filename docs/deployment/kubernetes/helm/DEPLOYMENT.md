# Kubernetes Helm deployment

Use the current `iscsi-csi-ngxstorage-kubernetes` chart for Kubernetes. The chart README documents its resources and values; the Helm repository README gives the current repository, install, co-installation, image digest, and coordination guidance. The charts live in the separate workspace `Helm` repository (`Helm/iscsi/kubernetes/`), which is **not part of this clone**; a standalone checkout can use the raw-manifest path below, while Helm installation requires that repository (chart publication is a separate release step).

Read the platform [requirements](../REQUIREMENTS.md) and [compatibility](../COMPATIBILITY.md) first. Keep the backend Secret externally managed for production, inspect rendered output before applying it, and use only the platform-matching chart. Do not install this chart and the raw bundle over the same CSI identity.

Chart guide: `Helm/iscsi/kubernetes/README.md` in the workspace Helm repository (see above). Canonical manual procedure: [deploy/README.md](../../../../deploy/README.md).
