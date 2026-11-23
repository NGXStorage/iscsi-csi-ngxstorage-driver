package utils

import (
	"context"
	"errors"
	"testing"
	"time"

	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

func TestCoordinationErrorMapping(t *testing.T) {
	if CoordinationError("op", nil) != nil {
		t.Fatal("nil error must stay nil")
	}
	if code := status.Code(CoordinationError("op", context.Canceled)); code != codes.Canceled {
		t.Fatalf("canceled -> %v", code)
	}
	if code := status.Code(CoordinationError("op", context.DeadlineExceeded)); code != codes.DeadlineExceeded {
		t.Fatalf("deadline -> %v", code)
	}
	err := CoordinationError("op", errors.New("boom"))
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("generic -> %v", code)
	}
}

func TestWrapOptional(t *testing.T) {
	if WrapOptional("step", nil) != nil {
		t.Fatal("nil must stay nil")
	}
	base := errors.New("base")
	wrapped := WrapOptional("step", base)
	if !errors.Is(wrapped, base) {
		t.Fatal("identity must be preserved")
	}
}

func TestReleaseOperationLockBounded(t *testing.T) {
	released := make(chan struct{})
	lock := &fakeLock{released: released}
	if err := ReleaseOperationLock(lock); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("lock was not released")
	}
}

type fakeLock struct{ released chan struct{} }

func (f *fakeLock) AssertOwned(context.Context) error { return nil }
func (f *fakeLock) OwnershipLost() <-chan struct{}    { return nil }
func (f *fakeLock) Release(context.Context) error     { close(f.released); return nil }
