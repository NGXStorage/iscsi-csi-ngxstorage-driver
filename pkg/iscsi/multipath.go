package iscsi

import (
	"fmt"
	"os/exec"
	"strings"
)

// Multipath manages device-mapper multipath state via the multipath/multipathd
// binaries. All invocations are bounded; the caller provides exec injection for
// tests through the Exec field.
type Multipath struct {
	// Exec runs a command and returns combined output. Tests override it.
	Exec func(name string, args ...string) ([]byte, error)
}

// NewMultipath builds a Multipath with production exec.
func NewMultipath() *Multipath {
	return &Multipath{Exec: func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}}
}

// DaemonRunning reports whether multipathd is running.
func (m *Multipath) DaemonRunning() bool {
	out, err := m.Exec("pgrep", "multipathd")
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// FlushDevice flushes and removes a multipath map.
func (m *Multipath) FlushDevice(device string) error {
	if _, err := m.Exec("multipath", "-f", device); err != nil {
		return fmt.Errorf("flush multipath device %s: %w", device, err)
	}
	return nil
}

// ReloadDevice reloads a multipath map (used after a device rescan).
func (m *Multipath) ReloadDevice(device string) error {
	if _, err := m.Exec("multipath", "-r", device); err != nil {
		return fmt.Errorf("reload multipath device %s: %w", device, err)
	}
	return nil
}

// ResizeDevice resizes a multipath map after the underlying LUN grew.
func (m *Multipath) ResizeDevice(name string) error {
	if _, err := m.Exec("multipathd", "resize", "map", name); err != nil {
		return fmt.Errorf("resize multipath device %s: %w", name, err)
	}
	return nil
}
