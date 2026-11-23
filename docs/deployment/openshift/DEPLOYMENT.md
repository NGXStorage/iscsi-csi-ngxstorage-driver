# Deployment guide

Read the platform [requirements](REQUIREMENTS.md) and [compatibility](COMPATIBILITY.md), then use the canonical [manual deployment guide](../../../deploy/README.md) for commands. It covers image selection, Secret references, manifest rendering, apply, verification, rollback, and uninstall.

The platform overlay selects the namespace and platform-specific resources. Review the rendered bundle before applying it. A rollout does not prove backend provisioning, protocol data I/O, or cleanup; use the applicable live test guide for those checks.
