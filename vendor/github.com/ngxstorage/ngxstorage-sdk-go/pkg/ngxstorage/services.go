package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
)

// ServiceService controls the NFS service lifecycle.
type ServiceService struct{ c *Client }

// Services returns the service service.
func (c *Client) Services() *ServiceService { return &ServiceService{c} }

// NFSStatus returns the NFS service status document.
func (s *ServiceService) NFSStatus(ctx context.Context) (map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetNFSService", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get NFS status: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode NFS status: %w", err)
	}
	return m, nil
}

// NFSStart starts the NFS service.
func (s *ServiceService) NFSStart(ctx context.Context) error {
	_, err := s.c.mutation(ctx, "StartNFSService", nil, nil)
	if err != nil {
		return fmt.Errorf("ngxstorage: start NFS: %w", err)
	}
	return nil
}

// NFSStop stops the NFS service.
func (s *ServiceService) NFSStop(ctx context.Context) error {
	_, err := s.c.mutation(ctx, "StopNFSService", nil, nil)
	if err != nil {
		return fmt.Errorf("ngxstorage: stop NFS: %w", err)
	}
	return nil
}

// NFSRestart restarts the NFS service.
func (s *ServiceService) NFSRestart(ctx context.Context) error {
	_, err := s.c.mutation(ctx, "RestartNFSService", nil, nil)
	if err != nil {
		return fmt.Errorf("ngxstorage: restart NFS: %w", err)
	}
	return nil
}

// NFSSettings returns the NFS settings document.
func (s *ServiceService) NFSSettings(ctx context.Context) (map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetNFSSettings", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get NFS settings: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode NFS settings: %w", err)
	}
	return m, nil
}

// SetNFSSettings replaces the NFS settings document (PUT). Partial updates
// require the caller to pass the full document as returned by NFSSettings.
func (s *ServiceService) SetNFSSettings(ctx context.Context, settings map[string]interface{}) (map[string]interface{}, error) {
	resp, err := s.c.mutation(ctx, "SetNFSSettings", nil, settings)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: set NFS settings: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(resp, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode set NFS settings: %w", err)
	}
	return m, nil
}
