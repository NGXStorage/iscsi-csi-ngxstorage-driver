package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// ISCSITargetService manages iSCSI targets and LUN mappings.
type ISCSITargetService struct{ c *Client }

// ISCSITargets returns the iSCSI target service.
func (c *Client) ISCSITargets() *ISCSITargetService { return &ISCSITargetService{c} }

// List fetches the lightweight iSCSI target identity list.
func (s *ISCSITargetService) List(ctx context.Context) ([]ISCSITarget, error) {
	return s.ListNames(ctx)
}

// ListNames fetches the lightweight iSCSI target identity list.
func (s *ISCSITargetService) ListNames(ctx context.Context) ([]ISCSITarget, error) {
	body, err := s.c.get(ctx, "GetISCSITargetList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list iSCSI target names: %w", err)
	}
	var targets []ISCSITarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode iSCSI target name list: %w", err)
	}
	return targets, nil
}

// ListDetail fetches all iSCSI targets with their full configuration.
func (s *ISCSITargetService) ListDetail(ctx context.Context) ([]ISCSITarget, error) {
	body, err := s.c.get(ctx, "GetISCSITargetDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list iSCSI target details: %w", err)
	}
	var targets []ISCSITarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode iSCSI target detail list: %w", err)
	}
	return targets, nil
}

// Get fetches a single iSCSI target.
func (s *ISCSITargetService) Get(ctx context.Context, id string) (*ISCSITarget, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetISCSITarget", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get iSCSI target: %w", err)
	}
	var target ISCSITarget
	if err := json.Unmarshal(body, &target); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode iSCSI target: %w", err)
	}
	return &target, nil
}

// Create provisions a new iSCSI target. The backend requires name,
// auth_group_id, portal_group_id, and owner and acknowledges with only
// {"success": true}; resolve the created target's id from ListNames by name.
func (s *ISCSITargetService) Create(ctx context.Context, name, authGroupID, portalGroupID, owner string) error {
	body := map[string]interface{}{
		"name":            name,
		"auth_group_id":   authGroupID,
		"portal_group_id": portalGroupID,
		"owner":           owner,
	}
	if _, err := s.c.mutation(ctx, "CreateISCSITarget", nil, body); err != nil {
		return fmt.Errorf("ngxstorage: create iSCSI target: %w", err)
	}
	return nil
}

// Delete removes an iSCSI target. Idempotent.
func (s *ISCSITargetService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteISCSITarget", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete iSCSI target: %w", err)
	}
	return nil
}

// AddLUN maps a LUN to an iSCSI target. Idempotent.
func (s *ISCSITargetService) AddLUN(ctx context.Context, targetID, lunID, owner string) error {
	vars := url.Values{"id": {targetID}}
	body := map[string]interface{}{"owner": owner, "lun_ids": []string{lunID}}
	_, err := s.c.mutation(ctx, "AddLunToISCSITarget", vars, body)
	if IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: add LUN to iSCSI target: %w", err)
	}
	return nil
}

// RemoveLUN unmaps a LUN from an iSCSI target. Idempotent.
func (s *ISCSITargetService) RemoveLUN(ctx context.Context, targetID, lunID, owner string) error {
	vars := url.Values{"id": {targetID}}
	body := map[string]interface{}{"owner": owner, "lun_ids": []string{lunID}}
	_, err := s.c.mutation(ctx, "RemoveLunFromISCSITarget", vars, body)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: remove LUN from iSCSI target: %w", err)
	}
	return nil
}

// ChangeName renames an iSCSI target.
func (s *ISCSITargetService) ChangeName(ctx context.Context, id, name, owner string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "ChangeISCSITargetName", vars, map[string]interface{}{"name": name, "owner": owner})
	if err != nil {
		return fmt.Errorf("ngxstorage: change iSCSI target name: %w", err)
	}
	return nil
}

// ChangeAuthGroup reassigns the auth group of an iSCSI target.
func (s *ISCSITargetService) ChangeAuthGroup(ctx context.Context, id, authGroupID, owner string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "ChangeISCSITargetAuthGroup", vars, map[string]interface{}{"auth_group_id": authGroupID, "owner": owner})
	if err != nil {
		return fmt.Errorf("ngxstorage: change iSCSI target auth group: %w", err)
	}
	return nil
}

// ChangePortalGroup reassigns the portal group of an iSCSI target.
func (s *ISCSITargetService) ChangePortalGroup(ctx context.Context, id, portalGroupID, owner string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "ChangeISCSITargetPortalGroup", vars, map[string]interface{}{"portal_group_id": portalGroupID, "owner": owner})
	if err != nil {
		return fmt.Errorf("ngxstorage: change iSCSI target portal group: %w", err)
	}
	return nil
}
