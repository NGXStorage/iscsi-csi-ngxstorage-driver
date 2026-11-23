package ngxstorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// ShareCreateRequest carries only fields supported by share creation. The
// backend expects soft_quota as a flat byte count (never nested) and boolean
// storage attributes. Permissions are changed separately with Modify after
// creation; the SDK does not orchestrate that workflow. Nil cache pointers
// leave those modify-only attributes unchanged; non-nil pointers explicitly
// set them on or off.
//
// The create route declares only blocksize, deduplication, compression, the
// export flags, and the quota fields. An empty BlockSize is omitted so the
// backend default (128k) applies, and FlashCache/DramCache/FlashTierLimit are
// applied with a follow-up modify using the canonical modify key names.
type ShareCreateRequest struct {
	Name           string
	SoftQuotaBytes int64
	BlockSize      string
	FlashCache     *bool
	DramCache      *bool
	Dedup          bool
	Compress       bool
	FlashTierLimit string
}

func flexBoolValue(value *bool) FlexBool {
	return FlexBool(value != nil && *value)
}

// ShareService manages file (NFS) volumes.
type ShareService struct{ c *Client }

// Shares returns the share service.
func (c *Client) Shares() *ShareService { return &ShareService{c} }

// Create provisions an NFS share on the configured pool with its export
// disabled. The backend acks with the canonical volume_id, but the ID is still
// resolved from the lightweight list so a pre-existing same-name share (which
// the create call returns unchanged) resolves to its real identity.
func (s *ShareService) Create(ctx context.Context, req ShareCreateRequest) (*Share, error) {
	if req.Name == "" {
		return nil, errors.New("ngxstorage: share name is required")
	}
	if req.SoftQuotaBytes <= 0 {
		return nil, errors.New("ngxstorage: share soft quota must be a positive byte value")
	}
	// The create route declares only blocksize, deduplication, compression,
	// the export flags, and the quota fields. An empty blocksize is omitted
	// so the backend default (128k) applies instead of failing validation.
	body := map[string]interface{}{
		"pool_name":     s.c.poolName,
		"name":          req.Name,
		"soft_quota":    req.SoftQuotaBytes,
		"deduplication": req.Dedup,
		"compression":   req.Compress,
		"nfs_export":    false,
	}
	if req.BlockSize != "" {
		body["blocksize"] = req.BlockSize
	}
	if _, err := s.c.mutation(ctx, "CreateShare", nil, body); err != nil {
		return nil, fmt.Errorf("ngxstorage: create share: %w", err)
	}

	// SHARE_CREATE returns only success; resolve the canonical identity from
	// the released lightweight list.
	refs, err := s.ListNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: resolve created share id: %w", err)
	}
	var shareID string
	for _, ref := range refs {
		if ref.Name == req.Name {
			shareID = ref.ID
			break
		}
	}
	if shareID == "" {
		return nil, fmt.Errorf("ngxstorage: created share %q is missing from the canonical list", req.Name)
	}

	// Cache and flashtier attributes are modify-only on the backend; apply
	// them with the canonical modify key names after the share exists.
	modifyFields := map[string]interface{}{}
	if req.FlashCache != nil {
		modifyFields["flash_cache"] = *req.FlashCache
	}
	if req.DramCache != nil {
		modifyFields["dram_cache"] = *req.DramCache
	}
	if req.FlashTierLimit != "" {
		modifyFields["flashtier_blocksize_limit"] = req.FlashTierLimit
	}
	if len(modifyFields) > 0 {
		if err := s.Modify(ctx, shareID, modifyFields); err != nil {
			return nil, fmt.Errorf("ngxstorage: apply create attributes: %w", err)
		}
	}

	return &Share{
		ID:       shareID,
		Name:     req.Name,
		PoolName: s.c.poolName,
		Storage: ShareStorage{
			BlockSize:      req.BlockSize,
			Deduplication:  FlexBool(req.Dedup),
			Compression:    FlexBool(req.Compress),
			DramCache:      flexBoolValue(req.DramCache),
			FlashCache:     flexBoolValue(req.FlashCache),
			FlashTierLimit: req.FlashTierLimit,
		},
	}, nil
}

// Get fetches a single share.
func (s *ShareService) Get(ctx context.Context, id string) (*Share, error) {
	vars := url.Values{"id": {id}}
	body, err := s.c.get(ctx, "GetShare", vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get share: %w", err)
	}
	var share Share
	if err := json.Unmarshal(body, &share); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode share: %w", err)
	}
	return &share, nil
}

// List fetches all shares with their canonical detail fields.
func (s *ShareService) List(ctx context.Context) ([]Share, error) {
	body, err := s.c.get(ctx, "GetShareDetailList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list shares: %w", err)
	}
	var shares []Share
	if err := json.Unmarshal(body, &shares); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode share list: %w", err)
	}
	return shares, nil
}

// ShareRef is a lightweight share record carrying only the canonical identity
// fields returned by the released share list.
type ShareRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListNames reads the lightweight share list (GET /api/v2/share) used to
// resolve a created share's canonical ID.
func (s *ShareService) ListNames(ctx context.Context) ([]ShareRef, error) {
	body, err := s.c.get(ctx, "GetShareList", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list share names: %w", err)
	}
	var refs []ShareRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode share names: %w", err)
	}
	return refs, nil
}

// Delete removes a share. Idempotent.
func (s *ShareService) Delete(ctx context.Context, id string) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "DeleteShare", vars, nil)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ngxstorage: delete share: %w", err)
	}
	return nil
}

// Modify patches arbitrary supported share fields.
func (s *ShareService) Modify(ctx context.Context, id string, fields map[string]interface{}) error {
	vars := url.Values{"id": {id}}
	_, err := s.c.mutation(ctx, "ModifyShare", vars, fields)
	if err != nil {
		return fmt.Errorf("ngxstorage: modify share: %w", err)
	}
	return nil
}

// Expand grows a share to softQuotaBytes. The backend expects soft_quota as a
// flat byte count.
func (s *ShareService) Expand(ctx context.Context, id string, softQuotaBytes int64) error {
	return s.Modify(ctx, id, map[string]interface{}{"soft_quota": softQuotaBytes})
}

// SetExportEnabled enables or disables the NFS export for a share through the
// canonical nfs_export JSON boolean.
func (s *ShareService) SetExportEnabled(ctx context.Context, id string, enabled bool) error {
	return s.Modify(ctx, id, map[string]interface{}{"nfs_export": enabled})
}

// SetReadOnly sets the read-only state of a share export through the
// canonical nfs_read_only JSON boolean.
func (s *ShareService) SetReadOnly(ctx context.Context, id string, readOnly bool) error {
	return s.Modify(ctx, id, map[string]interface{}{"nfs_read_only": readOnly})
}
