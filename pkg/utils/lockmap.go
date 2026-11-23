package utils

import (
	"context"
	"errors"
	"fmt"
	"time"

	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// CoordinationReleaseTimeout bounds lock-release calls so a stuck Lease
// release cannot hang the RPC past its cleanup budget.
const CoordinationReleaseTimeout = 5 * time.Second

// CoordinationError maps coordinator failures to gRPC status: ended contexts
// keep their cancellation/deadline code, everything else is Unavailable.
func CoordinationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return status.Errorf(codes.Unavailable, "%s: %v", operation, err)
}

// ReleaseOperationLock releases one operation lock under a bounded context.
func ReleaseOperationLock(lock coord.Lock) error {
	ctx, cancel := context.WithTimeout(context.Background(), CoordinationReleaseTimeout)
	defer cancel()
	return lock.Release(ctx)
}

// ReleaseOperationLockSet releases a lock set under a bounded context.
func ReleaseOperationLockSet(lock coord.LockSet) error {
	ctx, cancel := context.WithTimeout(context.Background(), CoordinationReleaseTimeout)
	defer cancel()
	return lock.Release(ctx)
}

// WrapOptional annotates an optional-step error without changing its identity.
func WrapOptional(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
