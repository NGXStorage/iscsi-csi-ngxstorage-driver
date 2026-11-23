package coord

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Preflight verifies that the complete declarative Lease pool exists and that
// this process can read the exact resources it will later mutate. Node roles
// additionally verify their own Pod name/UID because destructive stale-holder
// takeover depends on exact Pod liveness checks.
func (m *Manager) Preflight(ctx context.Context) error {
	names, err := m.config.LeaseNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := m.client.CoordinationV1().Leases(m.namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("coord: preflight Lease %s: %w", name, err)
		}
	}
	if m.identity.Role != RoleNode {
		return nil
	}
	pod, err := m.client.CoreV1().Pods(m.namespace).Get(ctx, m.identity.PodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("coord: preflight holder pod %s: %w", m.identity.PodName, err)
	}
	if !PodUIDMatch(pod, m.identity.PodUID) {
		return fmt.Errorf("coord: preflight holder pod UID mismatch: pod=%s expected_uid=%s actual_uid=%s",
			m.identity.PodName, m.identity.PodUID, pod.UID)
	}
	return nil
}
