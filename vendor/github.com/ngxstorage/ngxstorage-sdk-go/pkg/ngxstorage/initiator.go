package ngxstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// InitiatorService lists FC and iSCSI initiators.
type InitiatorService struct{ c *Client }

// Initiators returns the initiator service.
func (c *Client) Initiators() *InitiatorService { return &InitiatorService{c} }

// List fetches all initiators.
func (s *InitiatorService) List(ctx context.Context) ([]Initiator, error) {
	body, err := s.c.get(ctx, "GetInitiators", nil)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: list initiators: %w", err)
	}
	var inits []Initiator
	if err := json.Unmarshal(body, &inits); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode initiators: %w", err)
	}
	return inits, nil
}

// GetFC fetches FC initiators; pass an empty id for the unfiltered list.
func (s *InitiatorService) GetFC(ctx context.Context, id string) ([]Initiator, error) {
	name, vars := "GetFCInitiators", url.Values{"id": {id}}
	if id == "" {
		name, vars = "GetFCInitiatorsAll", nil
	}
	body, err := s.c.get(ctx, name, vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get FC initiators: %w", err)
	}
	var inits []Initiator
	if err := json.Unmarshal(body, &inits); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode FC initiators: %w", err)
	}
	return inits, nil
}

// GetISCSI fetches iSCSI initiators; pass an empty id for the unfiltered list.
func (s *InitiatorService) GetISCSI(ctx context.Context, id string) ([]Initiator, error) {
	name, vars := "GetISCSIInitiators", url.Values{"id": {id}}
	if id == "" {
		name, vars = "GetISCSIInitiatorsAll", nil
	}
	body, err := s.c.get(ctx, name, vars)
	if err != nil {
		return nil, fmt.Errorf("ngxstorage: get iSCSI initiators: %w", err)
	}
	var inits []Initiator
	if err := json.Unmarshal(body, &inits); err != nil {
		return nil, fmt.Errorf("ngxstorage: decode iSCSI initiators: %w", err)
	}
	return inits, nil
}
