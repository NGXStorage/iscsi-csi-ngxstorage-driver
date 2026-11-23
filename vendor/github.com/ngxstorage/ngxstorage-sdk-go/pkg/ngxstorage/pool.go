package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// PoolService manages storage pools.
type PoolService struct{ c *Client }

// Pools returns the pool service.
func (c *Client) Pools() *PoolService { return &PoolService{c} }

// List fetches all pools.
func (s *PoolService) List(ctx context.Context) ([]Pool, error) {
	body, err := s.c.get(ctx, "GetPoolList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list pools: %w", err)
	}
	var pools []Pool
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode pool list: %w", err)
	}
	return pools, nil
}

// ListDetail fetches all pools with full detail.
func (s *PoolService) ListDetail(ctx context.Context) ([]Pool, error) {
	body, err := s.c.get(ctx, "GetPoolDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list pool details: %w", err)
	}
	var pools []Pool
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode pool detail list: %w", err)
	}
	return pools, nil
}

// Get fetches a single pool.
func (s *PoolService) Get(ctx context.Context, id string) (*Pool, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetPool", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get pool: %w", err)
	}
	var pool Pool
	if err := json.Unmarshal(body, &pool); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode pool: %w", err)
	}
	return &pool, nil
}

// Overview returns the raw pool overview records. The backend replies with a
// bare array whose entries keep only a reduced field whitelist (they carry
// neither the pool id nor its name).
func (s *PoolService) Overview(ctx context.Context) ([]map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetPoolOverview", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get pool overview: %w", err)
	}
	var records []map[string]interface{}
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode pool overview: %w", err)
	}
	return records, nil
}

// GetConfiguredCapacity returns available and reserved bytes for the
// configured pool from the status/capacity endpoint.
func (s *PoolService) GetConfiguredCapacity(ctx context.Context) (avail, reserved int64, err error) {
	body, err := s.c.get(ctx, "GetCapacityStatus", nil)
	if err != nil {
		return 0, 0, fmt.Errorf("ngxstorage: get capacity: %w", err)
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, 0, fmt.Errorf("ngxstorage: decode capacity: %w", err)
	}
	for _, p := range raw {
		name, _ := p["name"].(string)
		if name != s.c.poolName {
			continue
		}
		logical, _ := p["logical"].(map[string]interface{})
		if logical == nil {
			break
		}
		avail = bytesFromValue(logical["available"])
		reserved = bytesFromValue(logical["reserved"])
		return avail, reserved, nil
	}
	return 0, 0, fmt.Errorf("ngxstorage: configured pool %q capacity not found", s.c.poolName)
}

// bytesFromValue converts a flexible JSON numeric/string byte value to int64.
func bytesFromValue(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		var i int64
		fmt.Sscanf(n, "%d", &i)
		return i
	default:
		return 0
	}
}
