package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// FCTargetService manages Fibre Channel targets and LUN mappings.
type FCTargetService struct{ c *Client }

// FCTargets returns the FC target service.
func (c *Client) FCTargets() *FCTargetService { return &FCTargetService{c} }

// List fetches the lightweight FC target identity list.
func (s *FCTargetService) List(ctx context.Context) ([]FCTarget, error) {
	return s.ListNames(ctx)
}

// ListNames fetches the lightweight FC target identity list.
func (s *FCTargetService) ListNames(ctx context.Context) ([]FCTarget, error) {
	body, err := s.c.get(ctx, "GetFCTargetList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list FC target names: %w", err)
	}
	var targets []FCTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC target name list: %w", err)
	}
	return targets, nil
}

// ListDetail fetches all FC targets including ports and LUN mappings.
func (s *FCTargetService) ListDetail(ctx context.Context) ([]FCTarget, error) {
	body, err := s.c.get(ctx, "GetFCTargetDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list FC target details: %w", err)
	}
	var targets []FCTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC target detail list: %w", err)
	}
	return targets, nil
}

// Get fetches a single FC target with ports and LUN mappings. Field values
// are returned exactly as the backend produces them (WWPNs retain the "naa."
// prefix; LUN numbers are strings). Interpretation is the driver's job.
func (s *FCTargetService) Get(ctx context.Context, id string) (*FCTarget, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetFCTarget", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get FC target: %w", err)
	}
	var target FCTarget
	if err := json.Unmarshal(body, &target); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC target: %w", err)
	}
	return &target, nil
}

// AddLUN maps a LUN to a pre-created FC target. Idempotent.
func (s *FCTargetService) AddLUN(ctx context.Context, targetID, lunID, owner string) error {
	vars := url.Values{"id": {targetID}}
	body := map[string]interface{}{"owner": owner, "lun_ids": []string{lunID}}
	_, err := s.c.mutation(ctx, "AddLunToFCTarget", vars, body)
	if IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: add LUN to FC target: %w", err)
	}
	return nil
}

// RemoveLUN unmaps a LUN from an FC target. Idempotent.
func (s *FCTargetService) RemoveLUN(ctx context.Context, targetID, lunID, owner string) error {
	vars := url.Values{"id": {targetID}}
	body := map[string]interface{}{"owner": owner, "lun_ids": []string{lunID}}
	_, err := s.c.mutation(ctx, "RemoveLunFromFCTarget", vars, body)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: remove LUN from FC target: %w", err)
	}
	return nil
}

// ListInitiatorTags fetches all FC initiator tags (zoning aliases).
func (s *FCTargetService) ListInitiatorTags(ctx context.Context) ([]FCInitiatorTag, error) {
	body, err := s.c.get(ctx, "GetFCInitiatorTagList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list FC initiator tags: %w", err)
	}
	var tags []FCInitiatorTag
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC initiator tag list: %w", err)
	}
	return tags, nil
}

// CreateInitiatorTag provisions an FC initiator tag (zoning alias) for one
// initiator NAA. initiatorNAA is the 16-hex-char initiator identifier (no
// "naa." prefix); tag is the alias name.
func (s *FCTargetService) CreateInitiatorTag(ctx context.Context, initiatorNAA, tag string) (*FCInitiatorTag, error) {
	body := map[string]interface{}{"initiator_naa": initiatorNAA, "tag": tag}
	resp, err := s.c.mutation(ctx, "CreateFCInitiatorTag", nil, body)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: create FC initiator tag: %w", err)
	}
	var created FCInitiatorTag
	if err := json.Unmarshal(resp, &created); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode create FC initiator tag: %w", err)
	}
	return &created, nil
}

// GetInitiatorTag fetches a single FC initiator tag.
func (s *FCTargetService) GetInitiatorTag(ctx context.Context, id string) (*FCInitiatorTag, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetFCInitiatorTag", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get FC initiator tag: %w", err)
	}
	var tag FCInitiatorTag
	if err := json.Unmarshal(body, &tag); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC initiator tag: %w", err)
	}
	return &tag, nil
}

// DeleteInitiatorTag removes an FC initiator tag. Idempotent.
func (s *FCTargetService) DeleteInitiatorTag(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteFCInitiatorTag", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete FC initiator tag: %w", err)
	}
	return nil
}
