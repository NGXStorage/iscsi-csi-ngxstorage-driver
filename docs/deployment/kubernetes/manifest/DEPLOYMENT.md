# Kubernetes raw manifest deployment

Read the platform [requirements](../REQUIREMENTS.md) and [compatibility](../COMPATIBILITY.md), then follow the canonical [manual deployment procedure](../../../../deploy/README.md). It covers namespace and Secret setup, immutable image selection, rendering, server dry-run, apply, verification, rollback, and uninstall.

The current bundle uses the Kubernetes overlay. Inspect the rendered resources before applying them; in particular, confirm namespace, driver and sidecar images, node host access and image references. Image build and source provenance are documented in [build/README.md](../../../../build/README.md).
