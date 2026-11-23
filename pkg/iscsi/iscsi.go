package iscsi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var sessionCleanupMu sync.Mutex

// Connector discovers and manages iSCSI block devices on the host. All
// sysfs reads/writes use os.ReadFile/os.WriteFile (never shell redirection),
// and all polling is bounded by a context deadline.
type Connector struct {
	// IQNPath is the host initiatorname.iscsi path (default /host/etc/iscsi).
	IQNPath string
	// SYSFSPath is the host sysfs mount point (default /host/sys). It is used to
	// verify that every discovered iSCSI SCSI path reports the backend WWID before
	// a caller may format or mount the device.
	SYSFSPath string
	// DEVRoot is the device directory (default /dev). Tests override it.
	DEVRoot string
	// WaitInterval is the polling interval between device-discovery checks.
	WaitInterval time.Duration
	// WaitTimeout is the maximum total time to wait for a device to appear.
	WaitTimeout time.Duration
	// Exec runs iscsiadm/blockdev; tests override it.
	Exec                 func(name string, args ...string) ([]byte, error)
	ExecContext          func(ctx context.Context, name string, args ...string) ([]byte, error)
	cleanupRetryInterval time.Duration
}

// NewConnector builds a Connector with production defaults.
func NewConnector() *Connector {
	return &Connector{
		IQNPath:      "/host/etc/iscsi/initiatorname.iscsi",
		SYSFSPath:    "/host/sys",
		DEVRoot:      "/dev",
		WaitInterval: 1 * time.Second,
		WaitTimeout:  60 * time.Second,
		Exec: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
		ExecContext: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

func normalizeWWID(value string) string {
	clean := strings.ToLower(strings.TrimSpace(value))
	clean = strings.TrimPrefix(clean, "0x")
	clean = strings.TrimPrefix(clean, "naa.")
	clean = strings.TrimPrefix(clean, "eui.")
	// Device-mapper multipath commonly prefixes the 16-byte NAA identifier with
	// the designator nibble "3". Backend scsi_id is the 32-hex-digit identifier.
	if len(clean) == 33 && clean[0] == '3' {
		clean = clean[1:]
	}
	return clean
}

func (c *Connector) blockWWID(devicePath string) (string, error) {
	base := filepath.Base(devicePath)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("invalid block device path %q", devicePath)
	}
	raw, err := os.ReadFile(filepath.Join(c.SYSFSPath, "class", "block", base, "device", "wwid"))
	if err != nil {
		return "", fmt.Errorf("read WWID for %s: %w", devicePath, err)
	}
	wwid := strings.TrimSpace(string(raw))
	if wwid == "" {
		return "", fmt.Errorf("empty WWID for %s", devicePath)
	}
	return wwid, nil
}

func (c *Connector) rescanBlockDevice(devicePath string) error {
	base := filepath.Base(devicePath)
	path := filepath.Join(c.SYSFSPath, "class", "block", base, "device", "rescan")
	if err := os.WriteFile(path, []byte("1\n"), 0o200); err != nil {
		return fmt.Errorf("rescan block device %s: %w", devicePath, err)
	}
	return nil
}

func containsAll(have []string, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, value := range have {
		set[value] = struct{}{}
	}
	for _, value := range want {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

// waitForExpectedPath waits until one portal/IQN/LUN path exists and reports
// exactly the backend scsi_id through sysfs. A stale H:C:T:L/by-path device is
// rescanned, but never accepted or formatted while its identity differs.
func (c *Connector) waitForExpectedPath(ctx context.Context, portal, iqn string, lun int, scsiID string) (string, error) {
	interval := c.WaitInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := c.WaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		resolved, err := c.ResolveDevicePath(portal, iqn, lun)
		if err == nil {
			actual, readErr := c.blockWWID(resolved)
			if readErr == nil && normalizeWWID(actual) == normalizeWWID(scsiID) {
				return resolved, nil
			}
			if readErr != nil {
				lastErr = readErr
			} else {
				lastErr = fmt.Errorf("stale iSCSI path %s reports WWID %s, want %s", resolved, actual, scsiID)
				_ = c.rescanBlockDevice(resolved)
				if refreshed, refreshErr := c.blockWWID(resolved); refreshErr == nil && normalizeWWID(refreshed) == normalizeWWID(scsiID) {
					return resolved, nil
				}
			}
		} else {
			lastErr = err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: timed out waiting for iSCSI path %s/%s lun %d with WWID %s: %w", ErrDeviceWaitTimeout, portal, iqn, lun, scsiID, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// WaitForMappedDevice verifies every advertised portal path belongs to the
// expected backend LUN and, for multipath volumes, waits until the dm map owns
// all verified SCSI paths. Callers must use this gate before filesystem probing.
func (c *Connector) WaitForMappedDevice(ctx context.Context, portals []string, iqn string, lun int, scsiID string) (string, error) {
	if len(portals) == 0 {
		return "", fmt.Errorf("%w: at least one iSCSI portal is required", ErrInvalidDeviceRequest)
	}
	if strings.TrimSpace(iqn) == "" {
		return "", fmt.Errorf("%w: target IQN is required", ErrInvalidDeviceRequest)
	}
	if lun < 0 {
		return "", fmt.Errorf("%w: invalid iSCSI LUN number %d", ErrInvalidDeviceRequest, lun)
	}
	if normalizeWWID(scsiID) == "" {
		return "", fmt.Errorf("%w: backend SCSI ID is required", ErrInvalidDeviceRequest)
	}

	paths := make([]string, 0, len(portals))
	for _, portal := range portals {
		portal = strings.TrimSpace(portal)
		if portal == "" {
			return "", fmt.Errorf("%w: empty iSCSI portal", ErrInvalidDeviceRequest)
		}
		devicePath, err := c.waitForExpectedPath(ctx, portal, iqn, lun, scsiID)
		if err != nil {
			return "", err
		}
		paths = append(paths, filepath.Base(devicePath))
	}
	if len(paths) == 1 {
		return filepath.Join(c.DEVRoot, paths[0]), nil
	}

	dmPath, err := c.WaitForMultipathDevice(ctx, scsiID)
	if err != nil {
		return "", err
	}
	dmBase := filepath.Base(dmPath)
	if !strings.HasPrefix(dmBase, "dm-") {
		return "", fmt.Errorf("expected dm-multipath device for %s, got %s", scsiID, dmPath)
	}
	interval := c.WaitInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := c.WaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		entries, readErr := os.ReadDir(filepath.Join(c.SYSFSPath, "class", "block", dmBase, "slaves"))
		if readErr == nil {
			slaves := make([]string, 0, len(entries))
			for _, entry := range entries {
				slaves = append(slaves, entry.Name())
			}
			if containsAll(slaves, paths) {
				return dmPath, nil
			}
			lastErr = fmt.Errorf("multipath device %s missing expected iSCSI paths: have=%v want=%v", dmPath, slaves, paths)
		} else {
			lastErr = readErr
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: timed out waiting for iSCSI multipath convergence: %w", ErrDeviceWaitTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// DiscoverIQN reads the host initiator IQN from initiatorname.iscsi.
func (c *Connector) DiscoverIQN() (string, error) {
	path := c.IQNPath
	if path == "" {
		return "", fmt.Errorf("host initiator path is required (expected /host/etc/iscsi/initiatorname.iscsi)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read initiatorname: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "InitiatorName=") {
			iqn := strings.TrimPrefix(line, "InitiatorName=")
			if iqn != "" {
				return iqn, nil
			}
		}
	}
	return "", fmt.Errorf("no InitiatorName found in %s", path)
}

// Login discovers and logs into the target portal for an IQN.
// Login logs in to one portal. The bool result reports whether THIS attempt
// newly established the session: iscsiadm exit 0 means this call performed
// the login (owner); exit 15 ("already logged in") means the session
// pre-existed and is NOT owned by this attempt. Ownership is decided by
// iscsiadm's own serialized login result, not by a racy pre-check GET, so
// concurrent acquisition of the same session yields exactly one owner.
// Callers must only roll back sessions they own (created == true).
func (c *Connector) Login(portal, iqn string) (bool, error) {
	// Discovery: create the sendtargets discovery record.
	if _, err := c.Exec("iscsiadm", "-m", "discoverydb", "-t", "sendtargets", "-p", portal, "-I", "default", "-o", "new"); err != nil {
		return false, fmt.Errorf("iscsiadm discoverydb new: %w", err)
	}
	if _, err := c.Exec("iscsiadm", "-m", "discoverydb", "-t", "sendtargets", "-p", portal, "-I", "default", "--discover"); err != nil {
		return false, fmt.Errorf("iscsiadm discover: %w", err)
	}
	// Login: exit 15 means a session already existed — not owned by us.
	if _, err := c.Exec("iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-l"); err != nil {
		if isAlreadyLoggedIn(err) {
			return false, nil
		}
		if isLoginRejected(err) {
			code, _ := loginExitCode(err)
			return false, &LoginRejectedError{Portal: portal, IQN: iqn, ExitCode: code, Err: err}
		}
		return false, fmt.Errorf("iscsiadm login: %w", err)
	}
	return true, nil
}

// Logout removes one IQN/portal session and its node record.
func (c *Connector) Logout(portal, iqn string) error {
	sessionCleanupMu.Lock()
	defer sessionCleanupMu.Unlock()
	if out, err := c.Exec("iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-u"); err != nil {
		if !noObjects(err) {
			return fmt.Errorf("iscsiadm logout: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := c.Exec("iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-o", "delete"); err != nil {
		if !noObjects(err) {
			return fmt.Errorf("iscsiadm delete db entry: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if _, checkErr := c.Exec("iscsiadm", "-m", "node", "-T", iqn, "-p", portal); !noObjects(checkErr) {
			return fmt.Errorf("verify absent node record for %s@%s: %v", iqn, portal, checkErr)
		}
	}
	out, err := c.Exec("iscsiadm", "-m", "session")
	if err != nil && !noObjects(err) {
		return fmt.Errorf("inspect iSCSI sessions: %w", err)
	}
	want, err := normalizePortal(portal)
	if err != nil {
		return fmt.Errorf("invalid iSCSI portal %q: %w", portal, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != iqn {
			continue
		}
		address, parseErr := normalizePortal(strings.SplitN(fields[2], ",", 2)[0])
		if parseErr != nil {
			return fmt.Errorf("invalid session portal %q: %w", fields[2], parseErr)
		}
		if address == want {
			return fmt.Errorf("iSCSI session remains for %s@%s", iqn, portal)
		}
	}
	return nil
}

func normalizePortal(portal string) (string, error) {
	host, port, err := net.SplitHostPort(portal)
	if err != nil {
		if ip := net.ParseIP(portal); ip != nil {
			host, port = ip.String(), "3260"
		} else if portal != "" && !strings.ContainsAny(portal, ":[] \t\n") {
			host, port = portal, "3260"
		} else {
			return "", err
		}
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("missing host or port")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, port), nil
}

func noObjects(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 21
}

// Disconnect flushes a multipath map, when present, then logs out and removes
// the exact node record for every portal used by one staged volume.
func (c *Connector) Disconnect(ctx context.Context, portals []string, iqn, scsiID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(portals) > 1 {
		wwid, err := MultipathWWID(scsiID)
		if err != nil {
			return fmt.Errorf("resolve multipath identity: %w", err)
		} else if out, err := c.execContext(ctx, "multipath", "-f", wwid); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			name, lookupErr := c.multipathName(wwid)
			if lookupErr != nil {
				return fmt.Errorf("inspect multipath %s: %w", wwid, lookupErr)
			}
			if name != "" {
				attempts := 10
				if name == wwid {
					if !strings.Contains(strings.ToLower(string(out)), "map in use") {
						return fmt.Errorf("flush multipath %s: %w: %s", wwid, err, strings.TrimSpace(string(out)))
					}
					attempts = 9
				}
				if err := c.flushBusyMap(ctx, wwid, name, attempts); err != nil {
					return err
				}
			}
			gone, checkErr := c.multipathGone(wwid)
			if checkErr != nil {
				return fmt.Errorf("flush multipath %s: %w: %s; map inspection: %v", wwid, err, strings.TrimSpace(string(out)), checkErr)
			}
			if !gone {
				return fmt.Errorf("flush multipath %s: %w: %s; map still present", wwid, err, strings.TrimSpace(string(out)))
			}
		}
	}
	for _, portal := range portals {
		if err := ctx.Err(); err != nil {
			return err
		}
		portal = strings.TrimSpace(portal)
		if portal == "" {
			return fmt.Errorf("empty iSCSI portal")
		}
		if err := c.Logout(portal, iqn); err != nil {
			return fmt.Errorf("logout portal %s: %w", portal, err)
		}
	}
	return nil
}

func (c *Connector) execContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c.ExecContext != nil {
		return c.ExecContext(ctx, name, args...)
	}
	return c.Exec(name, args...)
}

func (c *Connector) flushBusyMap(ctx context.Context, wwid, name string, attempts int) error {
	interval := c.cleanupRetryInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := c.execContext(ctx, "multipath", "-f", name)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(string(out)), "map in use") || attempt == attempts {
			return fmt.Errorf("flush multipath %s (%s), attempt %d: %w: %s", wwid, name, attempt, err, strings.TrimSpace(string(out)))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		current, lookupErr := c.multipathName(wwid)
		if lookupErr != nil {
			return fmt.Errorf("inspect multipath %s: %w", wwid, lookupErr)
		}
		if current == "" {
			return nil
		}
		if current != name {
			return fmt.Errorf("multipath %s changed name from %s to %s", wwid, name, current)
		}
	}
	return fmt.Errorf("no multipath flush attempts configured")
}

func (c *Connector) multipathName(wwid string) (string, error) {
	blocks, err := os.ReadDir(filepath.Join(c.SYSFSPath, "class", "block"))
	if err != nil {
		return "", err
	}
	name := ""
	for _, block := range blocks {
		if !strings.HasPrefix(block.Name(), "dm-") {
			continue
		}
		dir := filepath.Join(c.SYSFSPath, "class", "block", block.Name(), "dm")
		uuid, err := os.ReadFile(filepath.Join(dir, "uuid"))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(uuid)) != "mpath-"+wwid {
			continue
		}
		if name != "" {
			return "", fmt.Errorf("multiple maps for WWID %s", wwid)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(string(raw))
		if name == "" || strings.ContainsAny(name, "/ \t\n") {
			return "", fmt.Errorf("invalid multipath map name")
		}
	}
	return name, nil
}

func (c *Connector) multipathGone(wwid string) (bool, error) {
	blocks, err := os.ReadDir(filepath.Join(c.SYSFSPath, "class", "block"))
	if err != nil {
		return false, err
	}
	for _, block := range blocks {
		if !strings.HasPrefix(block.Name(), "dm-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(c.SYSFSPath, "class", "block", block.Name(), "dm", "uuid"))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(raw)) == "mpath-"+wwid {
			return false, nil
		}
	}
	return true, nil
}

// DevicePathForLUN returns the /dev/disk/by-path symlink for a portal/IQN/LUN.
func (c *Connector) DevicePathForLUN(portal, iqn string, lun int) string {
	// e.g. /dev/disk/by-path/ip-192.168.1.201:3260-iscsi-iqn-lun-6
	// The by-path symlink always carries the iSCSI port, even when the driver
	// stores the bare portal IP; append the default port when absent.
	if !strings.Contains(portal, ":") {
		portal += ":3260"
	}
	return filepath.Join(c.DEVRoot, "disk", "by-path", fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", portal, iqn, lun))
}

// ResolveDevicePath resolves the by-path symlink to a real /dev/sdX node.
func (c *Connector) ResolveDevicePath(portal, iqn string, lun int) (string, error) {
	byPath := c.DevicePathForLUN(portal, iqn, lun)
	resolved, err := filepath.EvalSymlinks(byPath)
	if err != nil {
		return "", fmt.Errorf("resolve device for %s: %w", byPath, err)
	}
	return resolved, nil
}

// WaitForDevice polls until the device for portal/IQN/LUN appears, or ctx ends.
func (c *Connector) WaitForDevice(ctx context.Context, portal, iqn string, lun int) (string, error) {
	interval := c.WaitInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(c.WaitTimeout)
	for {
		if path, err := c.ResolveDevicePath(portal, iqn, lun); err == nil {
			return path, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: timed out waiting for device %s lun %d", ErrDeviceWaitTimeout, iqn, lun)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Connect logs into every advertised portal and waits for all resulting SCSI
// paths. A single path is returned directly. Multiple paths are assembled by
// device-mapper multipath and resolved through the stable WWID symlink before
// the caller formats or mounts the device.
func (c *Connector) Connect(ctx context.Context, portals []string, iqn string, lun int, scsiID string) (string, error) {
	if len(portals) == 0 {
		return "", fmt.Errorf("%w: at least one iSCSI portal is required", ErrInvalidDeviceRequest)
	}
	var firstDevice string
	var loggedIn []string
	// A failed Connect must not leave partial portal sessions behind.
	// NodeStage persists the session state BEFORE Connect, so NodeUnstage
	// can always recover the volume's portals; this rollback is the first
	// line of defense and removes exactly the sessions THIS attempt created.
	// Roll back owned portals best-effort; rollback errors are joined with
	// the original error, never swallowed.
	rollback := func() error {
		var rerr error
		for i := len(loggedIn) - 1; i >= 0; i-- {
			if err := c.Logout(loggedIn[i], iqn); err != nil {
				rerr = errors.Join(rerr, fmt.Errorf("rollback logout portal %s: %w", loggedIn[i], err))
			}
		}
		return rerr
	}
	for _, portal := range portals {
		portal = strings.TrimSpace(portal)
		if portal == "" {
			return "", errors.Join(fmt.Errorf("%w: empty iSCSI portal", ErrInvalidDeviceRequest), rollback())
		}
		created, err := c.Login(portal, iqn)
		if err != nil {
			return "", errors.Join(fmt.Errorf("login portal %s: %w", portal, err), rollback())
		}
		// Only sessions THIS attempt created may be rolled back. A portal
		// whose session pre-existed (Login exit 15) belongs to another
		// owner; logging it out here would disconnect a live volume.
		if created {
			loggedIn = append(loggedIn, portal)
		}
		devicePath, err := c.WaitForDevice(ctx, portal, iqn, lun)
		if err != nil {
			return "", errors.Join(fmt.Errorf("wait for portal %s: %w", portal, err), rollback())
		}
		if firstDevice == "" {
			firstDevice = devicePath
		}
	}
	if len(portals) == 1 {
		return firstDevice, nil
	}
	device, err := c.WaitForMultipathDevice(ctx, scsiID)
	if err != nil {
		return "", errors.Join(err, rollback())
	}
	return device, nil
}

// MultipathWWID converts the NGX LUN SCSI ID into the VPD page 83 WWID used
// by multipath. NGX returns the 16-byte identifier without the leading NAA
// designator byte, while Linux scsi_id prefixes it with "3".
func MultipathWWID(scsiID string) (string, error) {
	clean := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(scsiID), "0x"))
	if clean == "" {
		return "", fmt.Errorf("empty SCSI ID")
	}
	if len(clean) == 32 {
		clean = "3" + clean
	}
	return clean, nil
}

// WaitForMultipathDevice registers the WWID and waits for multipathd and udev
// to provide the stable dm-uuid symlink. The wait is bounded by the
// connector timeout and request context.
func (c *Connector) WaitForMultipathDevice(ctx context.Context, scsiID string) (string, error) {
	wwid, err := MultipathWWID(scsiID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDeviceRequest, err)
	}
	if out, err := c.Exec("multipath", "-a", wwid); err != nil {
		return "", fmt.Errorf("register multipath WWID %s: %w: %s", wwid, err, strings.TrimSpace(string(out)))
	}
	interval := c.WaitInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := c.WaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	byID := filepath.Join(c.DEVRoot, "disk", "by-id", "dm-uuid-mpath-"+wwid)
	for {
		if path, err := filepath.EvalSymlinks(byID); err == nil {
			return path, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: timed out waiting for multipath device with WWID %s", ErrDeviceWaitTimeout, wwid)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

// RescanSessions asks the host iSCSI initiator to refresh the capacity of all
// devices in its active sessions after a controller-side LUN expansion.
func (c *Connector) RescanSessions() error {
	out, err := c.Exec("iscsiadm", "-m", "session", "--rescan")
	if err != nil {
		return fmt.Errorf("iscsiadm session rescan: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WaitForDeviceSize waits until the host block layer reports at least the
// requested capacity. The wait is bounded by both ctx and WaitTimeout.
func (c *Connector) WaitForDeviceSize(ctx context.Context, devicePath string, requiredBytes int64) error {
	if requiredBytes <= 0 {
		return fmt.Errorf("required device size must be positive")
	}
	interval := c.WaitInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := c.WaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.Exec("blockdev", "--getsize64", devicePath)
		if err == nil {
			size, parseErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if parseErr == nil && size >= requiredBytes {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to reach %d bytes", devicePath, requiredBytes)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// isAlreadyLoggedIn reports whether the iscsiadm error means the session was
// already established (exit status 15).
func isAlreadyLoggedIn(err error) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode() == 15
	}
	return strings.Contains(err.Error(), "exit status 15")
}
