package ngxstorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// LUNService manages block volumes (LUNs).
type LUNService struct{ c *Client }

// LUNs returns the LUN service.
func (c *Client) LUNs() *LUNService { return &LUNService{c} }

// LUNCreateOptions carries the optional LUN create attributes
// (POST /api/v2/lun/). Zero values are omitted from the request so the
// backend applies its defaults.
type LUNCreateOptions struct {
	Blocksize     string // "4k" | "8k" | "16k" | "32k" | "64k" | "128k"; default 32k (pool blocksize when omitted)
	Deduplication string // "on" | "off"
	Compression   string // "on" | "off"
	ThinProvision string // "on" | "off"
	IoType        string // "transactional" | "sequential"
	QosPriority   int    // 4 | 8 | 16 | 32 | 64 | 128
	Quantity      int    // number of LUNs to provision
}

// Create provisions a single LUN on the configured pool with default
// attributes. The LUN create body is pool_name, name, size (plus optional
// sing, blocksize, deduplication, compression, thin_provision, io_type,
// qos_priority, quantity); owner is not a create field.
func (s *LUNService) Create(ctx context.Context, name string, sizeGB int64) (*LUN, error) {
	return s.CreateWithOptions(ctx, name, sizeGB, LUNCreateOptions{})
}

// CreateWithOptions provisions a LUN with explicit create attributes. Zero
// fields in opts are omitted so the backend applies its defaults.
func (s *LUNService) CreateWithOptions(ctx context.Context, name string, sizeGB int64, opts LUNCreateOptions) (*LUN, error) {
	if name == "" {
		return nil, errors.New("ngxstorage: LUN name is required")
	}
	if sizeGB <= 0 {
		return nil, errors.New("ngxstorage: LUN size must be positive")
	}
	body := map[string]interface{}{
		"pool_name": s.c.poolName,
		"name":      name,
		"size":      sizeGB,
		"sing":      "G",
	}
	if opts.Blocksize != "" {
		body["blocksize"] = opts.Blocksize
	}
	if opts.Deduplication != "" {
		body["deduplication"] = opts.Deduplication
	}
	if opts.Compression != "" {
		body["compression"] = opts.Compression
	}
	if opts.ThinProvision != "" {
		body["thin_provision"] = opts.ThinProvision
	}
	if opts.IoType != "" {
		body["io_type"] = opts.IoType
	}
	if opts.QosPriority != 0 {
		body["qos_priority"] = opts.QosPriority
	}
	if opts.Quantity != 0 {
		body["quantity"] = opts.Quantity
	}
	resp, err := s.c.mutation(ctx, "CreateLUN", nil, body)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: create LUN: %w", err)
	}
	var parsed struct {
		Luns []LUN `json:"luns"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode create LUN: %w", err)
	}
	if len(parsed.Luns) == 0 {
		return nil, fmt.Errorf("ngxstorage: create LUN returned no LUN")
	}
	return &parsed.Luns[0], nil
}

// Get fetches a single LUN.
func (s *LUNService) Get(ctx context.Context, id string) (*LUN, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetLUN", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get LUN: %w", err)
	}
	var lun LUN
	if err := json.Unmarshal(body, &lun); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode LUN: %w", err)
	}
	return &lun, nil
}

// List fetches all LUNs.
func (s *LUNService) List(ctx context.Context) ([]LUN, error) {
	body, err := s.c.get(ctx, "GetLUNDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list LUNs: %w", err)
	}
	var luns []LUN
	if err := json.Unmarshal(body, &luns); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode LUN list: %w", err)
	}
	return luns, nil
}

// Delete removes a LUN. Idempotent: a not-found backend code is success.
func (s *LUNService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteLUN", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete LUN: %w", err)
	}
	return nil
}

// Modify patches arbitrary supported LUN fields.
func (s *LUNService) Modify(ctx context.Context, id string, fields map[string]interface{}) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "ModifyLUN", vars, fields)
	if err != nil {
		return fmt.Errorf("ngxstorage: modify LUN: %w", err)
	}
	return nil
}

// Expand grows a LUN by incrementGB gigabytes. The NGX block LUN modify API
// interprets the size field on an expand request as an additive increment, not
// an absolute target capacity. Callers that have an absolute desired size must
// re-read the current LUN size, calculate target-current, and pass only that
// positive delta. Concurrent callers therefore need external serialization
// around the read/calculate/expand sequence.
func (s *LUNService) Expand(ctx context.Context, id string, incrementGB int64) error {
	return s.Modify(ctx, id, map[string]interface{}{"size": incrementGB, "sing": "G"})
}
