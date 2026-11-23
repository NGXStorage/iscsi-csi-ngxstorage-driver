package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
)

// LogService queries alert, audit, and system event logs.
type LogService struct{ c *Client }

// Logs returns the log service.
func (c *Client) Logs() *LogService { return &LogService{c} }

// Alerts returns the alert log document.
func (s *LogService) Alerts(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetAlertLog")
}

// Audit returns the audit log document.
func (s *LogService) Audit(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetAuditLog")
}

// SystemEvents returns the system event log document.
func (s *LogService) SystemEvents(ctx context.Context) ([]map[string]interface{}, error) {
	return s.list(ctx, "GetSystemEventLog")
}

func (s *LogService) list(ctx context.Context, name string) ([]map[string]interface{}, error) {
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
