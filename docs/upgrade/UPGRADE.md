# Immutable image deployment and rollback

Deployment installs the driver from scratch with the image digest pinned in
the Quartet CR. Before changing the deployed digest, record the current digest,
source revision, Secret references, manifest revision, and active PVC workloads.
Review coordination/state compatibility in the canonical [manual deployment
guide](../../deploy/README.md); do not change coordination shard configuration
during an ordinary rolling update.

Build or publish only from intended committed source, referenced by immutable
digest. After deployment, verify controller/node readiness, CSI registration,
protocol data read/write, normal detach/unmount, and cleanup. Rollback re-applies
the recorded immutable image and matching configuration. Backend customer data
is never deleted as part of an image change, uninstall, or rollback.

No compatibility or certification claim is made here; current-source test and
workload evidence must be attached to each release before publication.