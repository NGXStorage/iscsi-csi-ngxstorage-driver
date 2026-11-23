package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
)

// HardwareService queries physical hardware health.
type HardwareService struct{ c *Client }

// Hardware returns the hardware service.
func (c *Client) Hardware() *HardwareService { return &HardwareService{c} }

// Disks returns the disk list document.
func (s *HardwareService) Disks(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwareDisk")
}

// Memory returns the memory list document.
func (s *HardwareService) Memory(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwareMemory")
}

// MemoryTotal returns the memory total document (aggregate memory usage).
func (s *HardwareService) MemoryTotal(ctx context.Context) (map[string]interface{}, error) {
	body, err := s.c.get(ctx, "GetHardwareMemoryTotal", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get hardware memory total: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode hardware memory total: %w", err)
	}
	return m, nil
}

// CPU returns the CPU list document.
func (s *HardwareService) CPU(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwareCPU")
}

// Fans returns the fan list document.
func (s *HardwareService) Fans(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwareFan")
}

// PowerSupplies returns the power supply list document.
func (s *HardwareService) PowerSupplies(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwarePowerSupply")
}

// Enclosures returns the enclosure list document.
func (s *HardwareService) Enclosures(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetHardwareEnclosure")
}

func (s *HardwareService) list(ctx context.Context, name string) ([]map[string]interface{}, error) {
	body, err := s.c.get(ctx, name, nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: %s: %w", name, err)
	}
	var m []map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode %s: %w", name, err)
	}
	return m, nil
}
