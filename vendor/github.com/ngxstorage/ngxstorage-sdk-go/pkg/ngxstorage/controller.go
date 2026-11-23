package ngxstorage

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// RefreshController resolves the NGX work mode and selects the controller that
// may serve the configured pool. It is idempotent and safe for concurrent use.
//
// Modes:
//   - single-master: one controller, connected and Master.
//   - master-ready: two controllers, one Master and one Ready; Master serves.
//   - cluster: two controllers, both Cluster; the controller whose pool list
//     contains the configured pool is selected.
//
// The selection is published atomically with a refresh timestamp. A failed
// refresh preserves the previous selection. When PoolName is empty, only the
// work-mode is validated and the first connected controller is selected.
func (c *Client) RefreshController(ctx context.Context) error {
	// Serialize refresh campaigns without making a context-cancelled caller
	// wait for another campaign to finish.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.refreshGate:
	}
	defer func() { c.refreshGate <- struct{}{} }()

	type observation struct {
		index     int
		ip        string
		connected bool
		status    string
	}
	observations := make([]observation, len(c.controllers))
	for i, ip := range c.controllers {
		observations[i] = observation{index: i, ip: ip}
		link := endpointURL("GetClusterStatus", ip, nil)
		body, _, err := c.requestController(ctx, http.MethodGet, link, nil)
		if err != nil {
			c.logger.Warnf("ngxstorage: cluster lookup failed for %s: %v", ip, err)
			continue
		}
		var cluster ClusterStatus
		if err := json.Unmarshal(body, &cluster); err != nil {
			c.logger.Warnf("ngxstorage: cluster decode failed for %s: %v", ip, err)
			continue
		}
		observations[i].status = cluster.Status
		if cluster.Connected != 1 {
			continue
		}
		observations[i].connected = true
	}

	// Resolve a work mode.
	mode := ""
	var candidates []int
	switch {
	case len(observations) == 1 && observations[0].connected && observations[0].status == "Master":
		mode = "single-master"
		candidates = []int{observations[0].index}
	case len(observations) == 2 &&
		observations[0].connected && observations[1].connected &&
		observations[0].status == "Master" && observations[1].status == "Ready":
		mode = "master-ready"
		candidates = []int{observations[0].index}
	case len(observations) == 2 &&
		observations[0].connected && observations[1].connected &&
		observations[0].status == "Ready" && observations[1].status == "Master":
		mode = "master-ready"
		candidates = []int{observations[1].index}
	case len(observations) == 2 &&
		observations[0].connected && observations[1].connected &&
		observations[0].status == "Cluster" && observations[1].status == "Cluster":
		mode = "cluster"
		candidates = []int{observations[0].index, observations[1].index}
	default:
		c.logger.Errorf("ngxstorage: controllers do not form a serviceable work mode")
		return ErrClusterNotReady
	}

	// With no configured pool, select the first connected candidate.
	if c.poolName == "" {
		for _, idx := range candidates {
			c.publishControllerSelection(idx)
			c.logger.Infof("ngxstorage: controller selected (%s): %s", mode, c.controllers[idx])
			return nil
		}
		return ErrClusterNotReady
	}

	// Find the controller whose pool list contains the configured pool.
	var selected = -1
	for _, idx := range candidates {
		ip := c.controllers[idx]
		link := endpointURL("GetPoolList", ip, nil)
		body, _, err := c.requestController(ctx, http.MethodGet, link, nil)
		if err != nil {
			c.logger.Warnf("ngxstorage: pool lookup failed for %s: %v", ip, err)
			continue
		}
		var pools []Pool
		if err := json.Unmarshal(body, &pools); err != nil {
			c.logger.Warnf("ngxstorage: pool decode failed for %s: %v", ip, err)
			continue
		}
		for _, p := range pools {
			if p.Name == c.poolName || p.ID == c.poolName {
				selected = idx
				break
			}
		}
		if selected != -1 {
			break
		}
	}
	if selected == -1 {
		c.logger.Errorf("ngxstorage: configured pool %q not found", c.poolName)
		return ErrPoolNotFound
	}

	c.publishControllerSelection(selected)
	c.logger.Infof("ngxstorage: controller selected (%s): %s", mode, c.controllers[selected])
	return nil
}

func (c *Client) publishControllerSelection(index int) {
	c.selectionMu.Lock()
	c.controllerIndex = index
	c.lastRefresh = time.Now()
	c.selectionMu.Unlock()
}

// ensureRefresh triggers a periodic refresh if the selection is stale.
func (c *Client) ensureRefresh(ctx context.Context) {
	c.selectionMu.RLock()
	lastRefresh := c.lastRefresh
	c.selectionMu.RUnlock()
	if lastRefresh.IsZero() || time.Since(lastRefresh) > 5*time.Minute {
		if err := c.RefreshController(ctx); err != nil {
			c.logger.Warnf("ngxstorage: periodic refresh failed (retaining selection): %v", err)
		}
	}
}
