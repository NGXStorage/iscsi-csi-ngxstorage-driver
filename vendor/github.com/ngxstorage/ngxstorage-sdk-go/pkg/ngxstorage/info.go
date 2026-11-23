package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
)

// InfoService queries system information.
type InfoService struct{ c *Client }

// Info returns the info service.
func (c *Client) Info() *InfoService { return &InfoService{c} }

// System returns the system info document.
func (s *InfoService) System(ctx context.Context) (map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetSystemInfo", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get system info: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode system info: %w", err)
	}
	return m, nil
}
