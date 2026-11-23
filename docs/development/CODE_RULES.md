# NGX Storage iSCSI CSI code rules

Self-contained rules for changing this driver's code. They mirror the shared
CSI workspace contract; this repository alone is sufficient to apply them.

## Package boundaries

- `pkg/driver` keeps the four entry files: `driver.go` (setup and lifecycle),
  `controller.go`, `node.go`, `identity.go` (the CSI RPC entry points), plus
  their tests. The RPC entry surface stays readable in these files; do not
  pile helper logic into them.
- Protocol-specific implementation lives in its owning package: `pkg/iscsi`
  owns portal, session, initiator and device/multipath operations;
  `pkg/helper` owns protocol-specific persistent state and LUN parameter
  handling (attachment, filesystem, initiator, lunparams, reconcile, session
  state); `pkg/utils` holds protocol-neutral utilities (backoff, capability,
  errors, lockmap, validation).
- Do not create `helpers.go`/`utils.go`/`misc.go` dump files, one file per
  function, or single-line wrapper chains. When the same behavior is used at
  more than two call sites, factor it into a named function with one clear
  responsibility.
- Behavior shared by FC, iSCSI, and NFS belongs in `csi-ngxstorage-utils`
  (a pinned public module, vendored here under `vendor/`); check its consumers
  and public API
  together with any change. Do not share behavior merely because code looks
  similar when protocol semantics differ. iSCSI session semantics stay in
  this driver.

## Comments and logs

- Keep comments short and function-oriented. Explain the function's
  contract, reason, units, ownership, or side effect. Do not put task, plan,
  chat, agent, or implementation-history narration in comments.
- This driver's per-operation logging structure is the reference for the
  other drivers: record useful safe request identity, validation and branch
  decisions, protocol actions, retry/deadline and result. Redact API keys,
  tokens, credentials, secret-bearing volume context, and raw API bodies.
- Verify device/session/mount state before returning idempotent success.
  Serialize mutation by resource identity, preserve cancellation, and let
  unrelated volumes progress independently.
- Advertise only capabilities implemented by the request path and deployment.

## Build and test inputs

- The SDK, logging and CSI utilities use pinned Go modules. Use the committed vendor tree for standalone builds.
- Use the driver-local `tests/` entry point and keep unit, live-backend,
  cluster, image, and publish evidence separate.
