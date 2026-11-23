package ngxstorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// SnapshotService manages point-in-time copies.
type SnapshotService struct{ c *Client }

// Snapshots returns the snapshot service.
func (c *Client) Snapshots() *SnapshotService { return &SnapshotService{c} }

// Create provisions a snapshot of a volume.
func (s *SnapshotService) Create(ctx context.Context, volumeID, name string) (*Snapshot, error) {
	body := map[string]interface{}{"name": name, "volume_id": volumeID}
	resp, err := s.c.mutation(ctx, "CreateSnapshot", nil, body)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: create snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(resp, &snap); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode create snapshot: %w", err)
	}
	return &snap, nil
}

// Get fetches a single snapshot.
func (s *SnapshotService) Get(ctx context.Context, id string) (*Snapshot, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetSnapshot", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode snapshot: %w", err)
	}
	return &snap, nil
}

// List fetches all snapshots.
func (s *SnapshotService) List(ctx context.Context) ([]Snapshot, error) {
	body, err := s.c.get(ctx, "GetSnapshotList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list snapshots: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(body, &snaps); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode snapshot list: %w", err)
	}
	return snaps, nil
}

// ListDetail fetches detailed snapshots from /api/v2/snapshot/list, optionally
// filtered by one source volume ID.
func (s *SnapshotService) ListDetail(ctx context.Context, volumeID string) ([]Snapshot, error) {
	vars := url.Values{}
	if volumeID != "" {
		// The backend filters snapshot/list by the canonical volume_id query
		// parameter.
		vars.Set("volume_id", volumeID)
	}
	body, err := s.c.get(ctx, "GetSnapshotDetailList", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list snapshot details: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(body, &snaps); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode snapshot detail list: %w", err)
	}
	return snaps, nil
}

// Delete removes a snapshot. Idempotent.
func (s *SnapshotService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteSnapshot", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 400 && apiErr.Code == 999 &&
			strings.TrimSpace(apiErr.Message) == "Snapshot could not be deleted. Because there are clone volumes created from this snapshot." {
			return fmt.Errorf("%w: %w", ErrSnapshotInUse, err)
		}
		return fmt.Errorf("ngxstorage: delete snapshot: %w", err)
	}
	return nil
}

// Clone creates a new volume from a snapshot, returning the new volume ID.
func (s *SnapshotService) Clone(ctx context.Context, snapID, newName string) (string, error) {
	vars := url.Values{"id": {snapID}}
	body := map[string]interface{}{"name": newName}
	resp, err := s.c.mutation(ctx, "CloneSnapshot", vars, body)
	if err != nil {
		return "", fmt.Errorf("ngxstorage: clone snapshot: %w", err)
	}
	var parsed struct {
		VolumeID string `json:"volume_id"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("ngxstorage: decode clone snapshot: %w", err)
	}
	if parsed.VolumeID == "" {
		return "", fmt.Errorf("ngxstorage: clone snapshot returned no volume ID")
	}
	return parsed.VolumeID, nil
}

// Restore reverts a volume to a snapshot.
func (s *SnapshotService) Restore(ctx context.Context, snapID string) error {
	vars := url.Values{"id": {snapID}}
	// The backend requires an empty body but a Content-Length header; an empty
	// JSON object satisfies both.
	body := map[string]interface{}{"confirm": "true"}
	_, err := s.c.mutation(ctx, "RestoreSnapshot", vars, body)
	if err != nil {
		return fmt.Errorf("ngxstorage: restore snapshot: %w", err)
	}
	return nil
}

// DeleteAll removes every snapshot of one source volume. Idempotent.
func (s *SnapshotService) DeleteAll(ctx context.Context, volumeID string) error {
	vars := url.Values{"id": {volumeID}}
	_, err := s.c.mutation(ctx, "DeleteAllSnapshots", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete all snapshots: %w", err)
	}
	return nil
}

// GetSchedule fetches the snapshot schedule of one source volume.
func (s *SnapshotService) GetSchedule(ctx context.Context, volumeID string) (*SnapshotSchedule, error) {
	vars := url.Values{"id": {volumeID}}
	body, err := s.c.get(ctx, "GetSnapshotSchedule", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get snapshot schedule: %w", err)
	}
	var schedule SnapshotSchedule
	if err := json.Unmarshal(body, &schedule); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode snapshot schedule: %w", err)
	}
	return &schedule, nil
}

// CreateSchedule installs a cron snapshot schedule on one source volume.
// cronTime is the backend cron expression (digits, *, -, , only) and keepMax
// bounds retained snapshots at 1..512. The backend acknowledges with the
// human-readable cron text (h_read).
func (s *SnapshotService) CreateSchedule(ctx context.Context, volumeID, cronTime string, keepMax int) (string, error) {
	if keepMax < 1 || keepMax > 512 {
		return "", fmt.Errorf("ngxstorage: snapshot schedule keep_max must be between 1 and 512")
	}
	vars := url.Values{"id": {volumeID}}
	body := map[string]interface{}{"cron_time": cronTime, "keep_max": keepMax}
	resp, err := s.c.mutation(ctx, "CreateSnapshotSchedule", vars, body)
	if err != nil {
		return "", fmt.Errorf("ngxstorage: create snapshot schedule: %w", err)
	}
	var parsed struct {
		HumanRead string `json:"h_read"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("ngxstorage: decode create snapshot schedule: %w", err)
	}
	return parsed.HumanRead, nil
}

// DeleteSchedule removes the snapshot schedule of one source volume. Idempotent.
func (s *SnapshotService) DeleteSchedule(ctx context.Context, volumeID string) error {
	vars := url.Values{"id": {volumeID}}
	_, err := s.c.mutation(ctx, "DeleteSnapshotSchedule", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete snapshot schedule: %w", err)
	}
	return nil
}
