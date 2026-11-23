package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// HostService lists hosts.
//
// Deprecated: the deployed API does not support the host endpoints (they
// return 5012 on the deployed 2.4.5 software). It is retained unchanged for
// released v1 compile compatibility only and is not supported production
// surface.
type HostService struct{ c *Client }

// Hosts returns the host service.
//
// Deprecated: the deployed API does not support the host endpoints (they
// return 5012 on the deployed 2.4.5 software). It is retained unchanged for
// released v1 compile compatibility only and is not supported production
// surface.
func (c *Client) Hosts() *HostService { return &HostService{c} }

// List fetches all hosts.
func (s *HostService) List(ctx context.Context) ([]map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetHostList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list hosts: %w", err)
	}
	var m []map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode host list: %w", err)
	}
	return m, nil
}

// Get fetches a single host.
func (s *HostService) Get(ctx context.Context, id string) (map[string]interface{}, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetHost", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get host: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode host: %w", err)
	}
	return m, nil
}
