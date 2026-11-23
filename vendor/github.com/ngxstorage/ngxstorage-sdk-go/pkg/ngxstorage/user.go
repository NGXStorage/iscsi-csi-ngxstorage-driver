package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// UserService lists users. User credentials are never present in list
// responses and are never logged.
type UserService struct{ c *Client }

// Users returns the user service.
func (c *Client) Users() *UserService { return &UserService{c} }

// List fetches all users (usernames/roles only, no passwords).
func (s *UserService) List(ctx context.Context) ([]map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetUserList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list users: %w", err)
	}
	var m []map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode user list: %w", err)
	}
	return m, nil
}

// Get fetches a single user (username/roles only, no password).
func (s *UserService) Get(ctx context.Context, id string) (map[string]interface{}, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetUser", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get user: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode user: %w", err)
	}
	return m, nil
}

// Create provisions a user. The password is never logged by the SDK. The
// role value must be the string "admin" or "monitor". The backend acks with
// {"id": <numeric epoch id>}, returned as its decimal string.
func (s *UserService) Create(ctx context.Context, user map[string]interface{}) (string, error) {
	resp, err := s.c.mutation(ctx, "CreateUser", nil, user)
	if err != nil {
		return "", fmt.Errorf("ngxstorage: create user: %w", err)
	}
	var parsed struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return "", fmt.Errorf("ngxstorage: decode create user: %w", err)
	}
	return parsed.ID.String(), nil
}

// Delete removes a user. Idempotent.
func (s *UserService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteUser", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete user: %w", err)
	}
	return nil
}
