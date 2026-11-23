package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
)

// NetworkService queries network information.
type NetworkService struct{ c *Client }

// Network returns the network service.
func (c *Client) Network() *NetworkService { return &NetworkService{c} }

// Info returns the network info document.
func (s *NetworkService) Info(ctx context.Context) (map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetNetworkInfo", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get network info: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode network info: %w", err)
	}
	return m, nil
}
