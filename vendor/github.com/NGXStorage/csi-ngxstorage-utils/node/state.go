package node

import (
	"context"
	"sync"
	"time"
)

// VolumeLockRetryDelay is the polling interval used while waiting for a
// per-volume node mutation lock.
const VolumeLockRetryDelay = 10 * time.Millisecond

// State serializes node mount mutations and records published targets.
type State struct {
	accessLock  sync.Mutex
	published   map[string]string
	volumeLocks sync.Map
}

// Lock serializes direct access to published-target state for diagnostics and tests.
func (s *State) Lock() { s.accessLock.Lock() }

// Unlock releases a direct published-target state lock.
func (s *State) Unlock() { s.accessLock.Unlock() }

// LockVolume acquires the per-volume mutation lock, respecting cancellation.
func (s *State) LockVolume(ctx context.Context, volumeID string) (*sync.Mutex, error) {
	lockValue, _ := s.volumeLocks.LoadOrStore(volumeID, &sync.Mutex{})
	volumeLock := lockValue.(*sync.Mutex)
	for {
		if volumeLock.TryLock() {
			return volumeLock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(VolumeLockRetryDelay):
		}
	}
}

// PublishedTarget returns the recorded target path for a volume.
func (s *State) PublishedTarget(volumeID string) (string, bool) {
	s.accessLock.Lock()
	defer s.accessLock.Unlock()
	targetPath, present := s.published[volumeID]
	return targetPath, present
}

// SetPublished records a volume target path.
func (s *State) SetPublished(volumeID, targetPath string) {
	s.accessLock.Lock()
	defer s.accessLock.Unlock()
	if s.published == nil {
		s.published = map[string]string{}
	}
	s.published[volumeID] = targetPath
}

// DeletePublished removes a volume target path.
func (s *State) DeletePublished(volumeID string) {
	s.accessLock.Lock()
	defer s.accessLock.Unlock()
	delete(s.published, volumeID)
}
