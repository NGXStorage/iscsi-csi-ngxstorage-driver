package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// AuthGroupService manages iSCSI auth groups (CHAP credential containers).
type AuthGroupService struct{ c *Client }

// AuthGroups returns the auth group service.
func (c *Client) AuthGroups() *AuthGroupService { return &AuthGroupService{c} }

// Create provisions an auth group and returns its new ID. The live backend
// acks with a quoted sha1 hex string (only the legacy DefaultAuthGroup keeps
// the numeric id 1, which Create never returns); older releases emitted the
// id as a bare JSON number.
func (s *AuthGroupService) Create(ctx context.Context, name, owner string) (string, error) {
	body := map[string]interface{}{"name": name, "owner": owner}
	resp, err := s.c.mutation(ctx, "CreateAuthGroup", nil, body)
	if err != nil {
		return "", fmt.Errorf("ngxstorage: create auth group: %w", err)
	}
	var parsed struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("ngxstorage: decode create auth group: %w", err)
	}
	var idString string
	if err := json.Unmarshal(parsed.ID, &idString); err == nil && idString != "" {
		return idString, nil
	}
	var idNumber json.Number
	if err := json.Unmarshal(parsed.ID, &idNumber); err == nil && idNumber.String() != "" {
		return idNumber.String(), nil
	}
	return "", fmt.Errorf("ngxstorage: create auth group returned no id")
}

// Get fetches an auth group including its CHAP and initiators.
func (s *AuthGroupService) Get(ctx context.Context, id string) (*AuthGroup, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetAuthGroup", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get auth group: %w", err)
	}
	var group AuthGroup
	if err := json.Unmarshal(body, &group); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode auth group: %w", err)
	}
	return &group, nil
}

// List fetches all auth groups.
func (s *AuthGroupService) List(ctx context.Context) ([]AuthGroup, error) {
	body, err := s.c.get(ctx, "GetAuthGroupList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list auth groups: %w", err)
	}
	var groups []AuthGroup
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode auth group list: %w", err)
	}
	return groups, nil
}

// ListDetail fetches all auth groups with full detail from /api/v2/auth_group/list.
func (s *AuthGroupService) ListDetail(ctx context.Context) ([]AuthGroup, error) {
	body, err := s.c.get(ctx, "GetAuthGroupDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list auth group details: %w", err)
	}
	var groups []AuthGroup
	if err := json.Unmarshal(body, &groups); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode auth group detail list: %w", err)
	}
	return groups, nil
}

// Delete removes an auth group. Idempotent.
func (s *AuthGroupService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteAuthGroup", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete auth group: %w", err)
	}
	return nil
}

// AddCHAP attaches CHAP credentials to an auth group. The password is never
// logged by the SDK.
func (s *AuthGroupService) AddCHAP(ctx context.Context, id, username, password string) error {
	vars := url.Values{"id": {id}}
	body := map[string]interface{}{"username": username, "password": password}
	_, err := s.c.mutation(ctx, "AddCHAP", vars, body)
	if err != nil {
		return fmt.Errorf("ngxstorage: add CHAP: %w", err)
	}
	return nil
}

// DeleteCHAP removes CHAP credentials from an auth group.
func (s *AuthGroupService) DeleteCHAP(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteCHAP", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete CHAP: %w", err)
	}
	return nil
}

// AddIQN registers an initiator IQN on an auth group. The backend authorizes
// iSCSI logins only for registered IQNs; without this, login fails with error
// 24 (authorization failure). The alias is required by the backend. Returns
// the created IQN record so callers can track the IQN ID for later removal.
func (s *AuthGroupService) AddIQN(ctx context.Context, id, iqn, alias, owner string) (map[string]interface{}, error) {
	vars := url.Values{"id": {id}}
	body := map[string]interface{}{
		"owner": owner,
		"iqn":   iqn,
		"alias": alias,
	}
	resp, err := s.c.mutation(ctx, "AddIQN", vars, body)
	if IsAlreadyExists(err) {
		return map[string]interface{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: add IQN to auth group: %w", err)
	}
	var record map[string]interface{}
	if err := json.Unmarshal(resp, &record); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode add IQN response: %w", err)
	}
	return record, nil
}

// DeleteIQN removes an initiator IQN from an auth group. Idempotent.
func (s *AuthGroupService) DeleteIQN(ctx context.Context, iqnID string) error {
	vars := url.Values{"id": {iqnID}}
	_, err := s.c.mutation(ctx, "DeleteIQN", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete IQN from auth group: %w", err)
	}
	return nil
}
