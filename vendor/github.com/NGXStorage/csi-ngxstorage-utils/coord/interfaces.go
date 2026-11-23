package coord

import "context"

// Lock is the narrow operation-lock surface consumed by CSI driver code.
type Lock interface {
	AssertOwned(context.Context) error
	OwnershipLost() <-chan struct{}
	Release(context.Context) error
}

// LockSet is the narrow multi-lock surface consumed by CSI driver code.
type LockSet interface {
	AssertOwned(context.Context) error
	Release(context.Context) error
}

// Coordinator hides the concrete Kubernetes Lease manager behind a small
// testable interface. Production construction is fail-closed through in-cluster
// config and preflight; tests can inject a deterministic fake without
// Kubernetes credentials.
type Coordinator interface {
	Acquire(context.Context, string, AcquireOptions) (Lock, error)
	AcquireMany(context.Context, []string, AcquireOptions) (LockSet, error)
	Preflight(context.Context) error
}

var (
	_ Coordinator = (*Manager)(nil)
	_ Lock        = (*leaseLock)(nil)
	_ LockSet     = (*multiLock)(nil)
)
