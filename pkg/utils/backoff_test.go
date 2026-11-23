package utils

import (
	"context"
	"testing"
)

func TestReconcileBackoffSchedule(t *testing.T) {
	original := ReconcileSleeper
	t.Cleanup(func() { ReconcileSleeper = original })
	var attempts []int
	ReconcileSleeper = func(ctx context.Context, attempt int) error {
		attempts = append(attempts, attempt)
		return nil
	}
	for attempt := 1; attempt < ReconcileAttempts; attempt++ {
		if err := ReconcileBackoff(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}
	// The final attempt must NOT sleep: its error returns immediately.
	if err := ReconcileBackoff(context.Background(), ReconcileAttempts); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != ReconcileAttempts-1 {
		t.Fatalf("slept %v, want attempts 1..%d only", attempts, ReconcileAttempts-1)
	}
}

func TestReconcileBackoffContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ReconcileSleep(ctx, 1); err == nil {
		t.Fatal("canceled context must win over the sleep")
	}
}
