package utils

import (
	"context"
	"time"
)

// ReconcileAttempts bounds backend reconcile retry loops.
const ReconcileAttempts = 3

// ReconcileSleeper is the between-attempt backoff. Tests replace it to assert
// the sleep schedule without real multi-second waits.
var ReconcileSleeper = ReconcileSleep

// ReconcileSleep waits for the bounded backoff between attempts.
func ReconcileSleep(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReconcileBackoff waits between attempts only: the final attempt's error is
// returned immediately instead of sleeping first.
func ReconcileBackoff(ctx context.Context, attempt int) error {
	if attempt >= ReconcileAttempts {
		return nil
	}
	return ReconcileSleeper(ctx, attempt)
}
