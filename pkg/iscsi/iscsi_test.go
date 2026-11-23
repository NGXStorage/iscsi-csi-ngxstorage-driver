package iscsi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoverIQN(t *testing.T) {
	dir := t.TempDir()
	iqnFile := filepath.Join(dir, "initiatorname.iscsi")
	if err := os.WriteFile(iqnFile, []byte("InitiatorName=iqn.2000-01.com.example:node1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Connector{IQNPath: iqnFile}
	iqn, err := c.DiscoverIQN()
	if err != nil {
		t.Fatalf("DiscoverIQN: %v", err)
	}
	if iqn != "iqn.2000-01.com.example:node1" {
		t.Fatalf("iqn = %q, want iqn.2000-01.com.example:node1", iqn)
	}
}

func TestDiscoverIQNMissing(t *testing.T) {
	dir := t.TempDir()
	c := &Connector{IQNPath: filepath.Join(dir, "missing")}
	if _, err := c.DiscoverIQN(); err == nil {
		t.Fatal("expected error for missing initiatorname.iscsi")
	}
}

func TestDiscoverIQNRequiresHostPath(t *testing.T) {
	c := &Connector{}
	if _, err := c.DiscoverIQN(); err == nil {
		t.Fatal("expected empty IQNPath to fail instead of falling back to container /etc")
	}
}

func TestDevicePathForLUN(t *testing.T) {
	c := &Connector{DEVRoot: "/dev"}
	got := c.DevicePathForLUN("192.168.1.201:3260", "iqn.2000-01.example:tgt", 6)
	want := "/dev/disk/by-path/ip-192.168.1.201:3260-iscsi-iqn.2000-01.example:tgt-lun-6"
	if got != want {
		t.Fatalf("DevicePathForLUN = %q, want %q", got, want)
	}
}

func TestWaitForDeviceTimeout(t *testing.T) {
	c := &Connector{DEVRoot: t.TempDir(), WaitInterval: time.Millisecond, WaitTimeout: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.WaitForDevice(ctx, "1.2.3.4:3260", "iqn.tgt", 1); err == nil {
		t.Fatal("expected timeout waiting for nonexistent device")
	}
}

func TestMultipathWWID(t *testing.T) {
	got, err := MultipathWWID("589C005636424757595041E52A73D6D3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "3589c005636424757595041e52a73d6d3" {
		t.Fatalf("MultipathWWID = %q", got)
	}
}

func TestConnectUsesAllPortalsAndReturnsMultipathDevice(t *testing.T) {
	devRoot := t.TempDir()
	byPathDir := filepath.Join(devRoot, "disk", "by-path")
	byIDDir := filepath.Join(devRoot, "disk", "by-id")
	if err := os.MkdirAll(byPathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(byIDDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for index, portal := range []string{"10.0.0.1", "10.0.0.2"} {
		device := filepath.Join(devRoot, fmt.Sprintf("sd%c", 'a'+index))
		if err := os.WriteFile(device, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(byPathDir, fmt.Sprintf("ip-%s:3260-iscsi-iqn.test-lun-4", portal))
		if err := os.Symlink(filepath.Join("..", "..", filepath.Base(device)), link); err != nil {
			t.Fatal(err)
		}
	}
	dmDevice := filepath.Join(devRoot, "dm-0")
	if err := os.WriteFile(dmDevice, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wwid := "3589c005636424757595041e52a73d6d3"
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(dmDevice)), filepath.Join(byIDDir, "dm-uuid-mpath-"+wwid)); err != nil {
		t.Fatal(err)
	}

	var commands []string
	c := &Connector{
		DEVRoot:      devRoot,
		WaitInterval: time.Millisecond,
		WaitTimeout:  50 * time.Millisecond,
		Exec: func(name string, args ...string) ([]byte, error) {
			commands = append(commands, strings.Join(append([]string{name}, args...), " "))
			return nil, nil
		},
	}
	got, err := c.Connect(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got != dmDevice {
		t.Fatalf("Connect device = %q, want %q", got, dmDevice)
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{"-p 10.0.0.1", "-p 10.0.0.2", "multipath -a " + wwid} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "multipath -r") {
		t.Fatalf("unexpected global multipath reload:\n%s", joined)
	}
}

// realExitError builds a genuine *exec.ExitError with the given code (the
// production isAlreadyLoggedIn classifier inspects the concrete type).
func realExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("expected exit %d", code)
	}
	return err
}

// TestConnectPreservesPreexistingSession pins session ownership: a portal whose
// session ALREADY existed (iscsiadm login exit 15) must never be logged out
// by a later portal's failure rollback — the session belongs to another
// owner (a live volume). Ownership is decided by the login result, not a
// racy pre-check.
func TestConnectPreservesPreexistingSession(t *testing.T) {
	t.Run("preexisting A + failing B preserves A", func(t *testing.T) {
		devRoot := t.TempDir()
		connectTestDevice(t, devRoot, "10.0.0.1:3260", "iqn.existing", 4)
		var commands []string
		exit15 := realExitError(t, 15)
		c := &Connector{
			DEVRoot:      devRoot,
			WaitInterval: time.Millisecond,
			WaitTimeout:  50 * time.Millisecond,
			Exec: func(name string, args ...string) ([]byte, error) {
				cmd := strings.Join(append([]string{name}, args...), " ")
				commands = append(commands, cmd)
				if strings.Contains(cmd, " -l") && strings.Contains(cmd, "10.0.0.1") {
					return nil, exit15 // session pre-exists
				}
				if strings.Contains(cmd, "--discover") && strings.Contains(cmd, "10.0.0.2") {
					return nil, fmt.Errorf("exit status 11")
				}
				return nil, nil
			},
		}
		_, err := c.Connect(context.Background(), []string{"10.0.0.1:3260", "10.0.0.2:3260"}, "iqn.existing", 4, "589c005636424757595041e52a73d6d3")
		if err == nil {
			t.Fatal("Connect succeeded despite portal B failure")
		}
		joined := strings.Join(commands, "\n")
		if strings.Contains(joined, "-p 10.0.0.1:3260 -u") || strings.Contains(joined, "-p 10.0.0.1:3260 -o delete") {
			t.Fatalf("preexisting session on portal A was touched by rollback:\n%s", joined)
		}
	})

	t.Run("mixed ownership rolls back only newly created sessions", func(t *testing.T) {
		devRoot := t.TempDir()
		connectTestDevice(t, devRoot, "10.0.0.1:3260", "iqn.mix", 4)
		connectTestDevice2(t, devRoot, "10.0.0.2:3260", "iqn.mix", 4)
		var commands []string
		exit15 := realExitError(t, 15)
		c := &Connector{
			DEVRoot:      devRoot,
			WaitInterval: time.Millisecond,
			WaitTimeout:  50 * time.Millisecond,
			Exec: func(name string, args ...string) ([]byte, error) {
				cmd := strings.Join(append([]string{name}, args...), " ")
				commands = append(commands, cmd)
				// portal A pre-exists; portal B is newly created by us
				if strings.Contains(cmd, " -l") && strings.Contains(cmd, "10.0.0.1") {
					return nil, exit15
				}
				// portal C discovery fails
				if strings.Contains(cmd, "--discover") && strings.Contains(cmd, "10.0.0.3") {
					return nil, fmt.Errorf("exit status 11")
				}
				return nil, nil
			},
		}
		_, err := c.Connect(context.Background(), []string{"10.0.0.1:3260", "10.0.0.2:3260", "10.0.0.3:3260"}, "iqn.mix", 4, "589c005636424757595041e52a73d6d3")
		if err == nil {
			t.Fatal("Connect succeeded despite portal C failure")
		}
		joined := strings.Join(commands, "\n")
		// A (preexisting) untouched
		if strings.Contains(joined, "-p 10.0.0.1:3260 -u") {
			t.Fatalf("preexisting portal A logged out:\n%s", joined)
		}
		// B (newly created) rolled back
		if !strings.Contains(joined, "-p 10.0.0.2:3260 -u") {
			t.Fatalf("newly created portal B NOT rolled back:\n%s", joined)
		}
		// C never logged in
		if strings.Contains(joined, "-p 10.0.0.3:3260 -u") {
			t.Fatalf("never-logged-in portal C logged out:\n%s", joined)
		}
	})
}

// connectTestDevice creates the by-path device symlink WaitForDevice expects.
func connectTestDevice(t *testing.T, devRoot, portal, iqn string, lun int) {
	t.Helper()
	byPathDir := filepath.Join(devRoot, "disk", "by-path")
	if err := os.MkdirAll(byPathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(devRoot, "sda")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(byPathDir, fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", portal, iqn, lun))
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(device)), link); err != nil {
		t.Fatal(err)
	}
}

// connectTestDevice2 adds a second portal device (sdb) to the same devRoot.
func connectTestDevice2(t *testing.T, devRoot, portal, iqn string, lun int) {
	t.Helper()
	byPathDir := filepath.Join(devRoot, "disk", "by-path")
	device := filepath.Join(devRoot, "sdb")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(byPathDir, fmt.Sprintf("ip-%s-iscsi-%s-lun-%d", portal, iqn, lun))
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(device)), link); err != nil {
		t.Fatal(err)
	}
}

// TestConnectRollsBackPartialLoginOnFailure is the regression for the proven
// orphan-session mechanism: portal A login succeeds, portal B fails, and the
// partial session must be logged out by Connect itself (NodeUnstage cannot
// clean it up because session state is persisted only after a successful
// stage). The original error must surface; rollback errors are joined.
func TestConnectRollsBackPartialLoginOnFailure(t *testing.T) {
	t.Run("second portal login failure logs out the first", func(t *testing.T) {
		devRoot := t.TempDir()
		connectTestDevice(t, devRoot, "10.0.0.1:3260", "iqn.test", 4)
		var commands []string
		c := &Connector{
			DEVRoot:      devRoot,
			WaitInterval: time.Millisecond,
			WaitTimeout:  50 * time.Millisecond,
			Exec: func(name string, args ...string) ([]byte, error) {
				cmd := strings.Join(append([]string{name}, args...), " ")
				commands = append(commands, cmd)
				// portal B discovery fails (the observed exit-status trigger)
				if strings.Contains(cmd, "--discover") && strings.Contains(cmd, "10.0.0.2") {
					return nil, fmt.Errorf("exit status 11")
				}
				return nil, nil
			},
		}
		_, err := c.Connect(context.Background(), []string{"10.0.0.1:3260", "10.0.0.2:3260"}, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
		if err == nil {
			t.Fatal("Connect succeeded despite portal B login failure")
		}
		if !strings.Contains(err.Error(), "login portal 10.0.0.2") {
			t.Fatalf("original error lost: %v", err)
		}
		joined := strings.Join(commands, "\n")
		// portal A must be logged out (-u) and its node record deleted
		if !strings.Contains(joined, "-m node -T iqn.test -p 10.0.0.1:3260 -u") {
			t.Fatalf("partial session on portal A not logged out:\n%s", joined)
		}
		// portal B never logged in -> no logout for B
		if strings.Contains(joined, "-p 10.0.0.2:3260 -u") {
			t.Fatalf("unexpected logout of never-logged-in portal B:\n%s", joined)
		}
	})

	t.Run("device wait failure logs out all logged-in portals", func(t *testing.T) {
		var commands []string
		c := &Connector{
			DEVRoot:      t.TempDir(), // no device symlinks -> WaitForDevice fails
			WaitInterval: time.Millisecond,
			WaitTimeout:  10 * time.Millisecond,
			Exec: func(name string, args ...string) ([]byte, error) {
				commands = append(commands, strings.Join(append([]string{name}, args...), " "))
				return nil, nil
			},
		}
		_, err := c.Connect(context.Background(), []string{"10.0.0.1:3260"}, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
		if err == nil {
			t.Fatal("Connect succeeded despite missing device")
		}
		joined := strings.Join(commands, "\n")
		if !strings.Contains(joined, "-m node -T iqn.test -p 10.0.0.1:3260 -u") {
			t.Fatalf("session not logged out after device-wait failure:\n%s", joined)
		}
	})

	t.Run("rollback error is joined not swallowed", func(t *testing.T) {
		devRoot := t.TempDir()
		connectTestDevice(t, devRoot, "10.0.0.1:3260", "iqn.test", 4)
		c := &Connector{
			DEVRoot:      devRoot,
			WaitInterval: time.Millisecond,
			WaitTimeout:  50 * time.Millisecond,
			Exec: func(name string, args ...string) ([]byte, error) {
				cmd := strings.Join(append([]string{name}, args...), " ")
				if strings.Contains(cmd, "--discover") && strings.Contains(cmd, "10.0.0.2") {
					return nil, fmt.Errorf("exit status 11")
				}
				if strings.Contains(cmd, " -u") {
					return nil, fmt.Errorf("logout refused")
				}
				return nil, nil
			},
		}
		_, err := c.Connect(context.Background(), []string{"10.0.0.1:3260", "10.0.0.2:3260"}, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "login portal 10.0.0.2") || !strings.Contains(err.Error(), "rollback logout portal 10.0.0.1") {
			t.Fatalf("original and rollback errors must both surface: %v", err)
		}
	})
}

func TestRescanSessions(t *testing.T) {
	var got []string
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}}
	if err := c.RescanSessions(); err != nil {
		t.Fatalf("RescanSessions: %v", err)
	}
	want := []string{"iscsiadm", "-m", "session", "--rescan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
}

func TestDisconnectUsesExactPortals(t *testing.T) {
	var commands []string
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(append([]string{name}, args...), " "))
		return nil, nil
	}}
	if err := c.Disconnect(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.test", "589c005636424757595041fcbe15d9d9"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"multipath -f 3589c005636424757595041fcbe15d9d9",
		"iscsiadm -m node -T iqn.test -p 10.0.0.1 -u",
		"iscsiadm -m node -T iqn.test -p 10.0.0.1 -o delete",
		"iscsiadm -m node -T iqn.test -p 10.0.0.2 -u",
		"iscsiadm -m node -T iqn.test -p 10.0.0.2 -o delete",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestWaitForDeviceSize(t *testing.T) {
	responses := []int64{1 << 30, 2 << 30}
	calls := 0
	c := &Connector{
		WaitInterval: time.Millisecond,
		WaitTimeout:  50 * time.Millisecond,
		Exec: func(name string, args ...string) ([]byte, error) {
			if name != "blockdev" {
				return nil, fmt.Errorf("unexpected command %s", name)
			}
			idx := calls
			if idx >= len(responses) {
				idx = len(responses) - 1
			}
			calls++
			return []byte(fmt.Sprintf("%d\n", responses[idx])), nil
		},
	}
	if err := c.WaitForDeviceSize(context.Background(), "/dev/sdz", 2<<30); err != nil {
		t.Fatalf("WaitForDeviceSize: %v", err)
	}
	if calls != 2 {
		t.Fatalf("blockdev calls = %d, want 2", calls)
	}
}

func setupMappedDeviceFixture(t *testing.T, scsiID string, includeSecondSlave bool) (*Connector, string) {
	t.Helper()
	root := t.TempDir()
	devRoot := filepath.Join(root, "dev")
	sysRoot := filepath.Join(root, "sys")
	byPathDir := filepath.Join(devRoot, "disk", "by-path")
	byIDDir := filepath.Join(devRoot, "disk", "by-id")
	for _, dir := range []string{byPathDir, byIDDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for index, portal := range []string{"10.0.0.1", "10.0.0.2"} {
		block := fmt.Sprintf("sd%c", 'a'+index)
		device := filepath.Join(devRoot, block)
		if err := os.WriteFile(device, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(byPathDir, fmt.Sprintf("ip-%s:3260-iscsi-iqn.test-lun-4", portal))
		if err := os.Symlink(filepath.Join("..", "..", block), link); err != nil {
			t.Fatal(err)
		}
		deviceDir := filepath.Join(sysRoot, "class", "block", block, "device")
		if err := os.MkdirAll(deviceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		wwid := "naa." + scsiID
		if index == 1 {
			wwid = "0x" + scsiID
		}
		if err := os.WriteFile(filepath.Join(deviceDir, "wwid"), []byte(wwid+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deviceDir, "rescan"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dmDevice := filepath.Join(devRoot, "dm-0")
	if err := os.WriteFile(dmDevice, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wwid, err := MultipathWWID(scsiID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "dm-0"), filepath.Join(byIDDir, "dm-uuid-mpath-"+wwid)); err != nil {
		t.Fatal(err)
	}
	slavesDir := filepath.Join(sysRoot, "class", "block", "dm-0", "slaves")
	if err := os.MkdirAll(filepath.Join(slavesDir, "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	if includeSecondSlave {
		if err := os.MkdirAll(filepath.Join(slavesDir, "sdb"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Connector{
		SYSFSPath:    sysRoot,
		DEVRoot:      devRoot,
		WaitInterval: time.Millisecond,
		WaitTimeout:  20 * time.Millisecond,
		Exec: func(string, ...string) ([]byte, error) {
			return nil, nil
		},
	}
	return c, dmDevice
}

func TestWaitForMappedDeviceVerifiesWWIDsAndMultipathSlaves(t *testing.T) {
	scsiID := "589c005636424757595041e52a73d6d3"
	c, dmDevice := setupMappedDeviceFixture(t, scsiID, true)
	got, err := c.WaitForMappedDevice(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.test", 4, scsiID)
	if err != nil {
		t.Fatalf("WaitForMappedDevice: %v", err)
	}
	if got != dmDevice {
		t.Fatalf("mapped device = %q, want %q", got, dmDevice)
	}
}

func TestWaitForMappedDeviceRejectsStaleWWID(t *testing.T) {
	scsiID := "589c005636424757595041e52a73d6d3"
	c, _ := setupMappedDeviceFixture(t, scsiID, true)
	wrong := "589c005636424757595041ffffffffffff"
	wwidPath := filepath.Join(c.SYSFSPath, "class", "block", "sda", "device", "wwid")
	if err := os.WriteFile(wwidPath, []byte("naa."+wrong+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := c.WaitForMappedDevice(context.Background(), []string{"10.0.0.1"}, "iqn.test", 4, scsiID)
	if err == nil {
		t.Fatal("stale iSCSI path WWID must fail closed")
	}
	if !strings.Contains(err.Error(), "stale iSCSI path") {
		t.Fatalf("unexpected stale WWID error: %v", err)
	}
}

func TestWaitForMappedDeviceRequiresEveryMultipathSlave(t *testing.T) {
	scsiID := "589c005636424757595041e52a73d6d3"
	c, _ := setupMappedDeviceFixture(t, scsiID, false)
	_, err := c.WaitForMappedDevice(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.test", 4, scsiID)
	if err == nil {
		t.Fatal("multipath map missing an expected portal path must fail")
	}
	if !strings.Contains(err.Error(), "missing expected iSCSI paths") {
		t.Fatalf("unexpected multipath convergence error: %v", err)
	}
}

func TestNormalizeWWIDAcceptsBackendSysfsAndMultipathForms(t *testing.T) {
	want := "589c005636424757595041e52a73d6d3"
	for _, raw := range []string{want, "0x" + want, "naa." + want, "3" + want} {
		if got := normalizeWWID(raw); got != want {
			t.Fatalf("normalizeWWID(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDisconnectRetriesPartialPortalCleanup(t *testing.T) {
	exit21 := exec.Command("sh", "-c", "exit 21").Run()
	exit6 := exec.Command("sh", "-c", "exit 6").Run()
	firstDeleted := false
	secondAttempts := 0
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.Contains(cmd, "-m session") {
			return nil, exit21
		}
		if strings.Contains(cmd, "-p 10.0.0.1 -o delete") {
			if firstDeleted {
				return nil, exit21
			}
			firstDeleted = true
		}
		if strings.Contains(cmd, "-p 10.0.0.1") && !strings.Contains(cmd, "-o delete") && firstDeleted && !strings.HasSuffix(cmd, " -u") {
			return nil, exit21
		}
		if strings.Contains(cmd, "-p 10.0.0.2 -o delete") {
			secondAttempts++
			if secondAttempts == 1 {
				return nil, exit6
			}
		}
		return nil, nil
	}}
	portals := []string{"10.0.0.1", "10.0.0.2"}
	if err := c.Disconnect(context.Background(), portals, "iqn.fixture", "589c0053475a3152564332cc948439eb"); err == nil || !strings.Contains(err.Error(), "exit status 6") {
		t.Fatalf("first failure must be retained: %v", err)
	}
	if err := c.Disconnect(context.Background(), portals, "iqn.fixture", "589c0053475a3152564332cc948439eb"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if secondAttempts != 2 {
		t.Fatalf("second portal attempts = %d", secondAttempts)
	}
}

func TestLogoutRejectsRemainingExactSession(t *testing.T) {
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "-m session" {
			return []byte("tcp: [1] 10.0.0.1:3260,1 iqn.fixture\n"), nil
		}
		return nil, nil
	}}
	if err := c.Logout("10.0.0.1", "iqn.fixture"); err == nil {
		t.Fatal("live session must prevent success")
	}
}

func TestLogoutSerializesAcrossConnectors(t *testing.T) {
	var active, overlap atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &Connector{Exec: func(_ string, args ...string) ([]byte, error) {
				cmd := strings.Join(args, " ")
				if strings.HasSuffix(cmd, " -u") {
					if active.Add(1) != 1 {
						overlap.Store(1)
					}
					time.Sleep(time.Millisecond)
				}
				if cmd == "-m session" {
					active.Add(-1)
				}
				return nil, nil
			}}
			if err := c.Logout("10.0.0.1", "iqn.fixture"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if overlap.Load() != 0 || active.Load() != 0 {
		t.Fatal("session cleanup operations overlapped")
	}
}

func TestLogoutRejectsIPv6Session(t *testing.T) {
	c := NewConnector()
	c.Exec = func(_ string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "session" {
			return []byte("tcp: [1] [2001:db8::1]:3260,1 iqn.fixture\n"), nil
		}
		return nil, nil
	}
	if err := c.Logout("2001:db8::1", "iqn.fixture"); err == nil {
		t.Fatal("expected an active IPv6 session to prevent logout completion")
	}
}

func TestDisconnectFlushFailureRequiresMapAbsence(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			sys := t.TempDir()
			blocks := filepath.Join(sys, "class", "block")
			if err := os.MkdirAll(blocks, 0700); err != nil {
				t.Fatal(err)
			}
			if present {
				p := filepath.Join(blocks, "dm-0", "dm")
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "uuid"), []byte("mpath-3589c0053475a3152564332cc948439eb"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			logouts := 0
			c := &Connector{SYSFSPath: sys, Exec: func(name string, args ...string) ([]byte, error) {
				if name == "multipath" {
					return nil, fmt.Errorf("flush failed")
				}
				if strings.HasSuffix(strings.Join(args, " "), " -u") {
					logouts++
				}
				return nil, nil
			}}
			err := c.Disconnect(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.fixture", "589c0053475a3152564332cc948439eb")
			if present && (err == nil || logouts != 0) {
				t.Fatalf("present map: error=%v logouts=%d", err, logouts)
			}
			if !present && (err != nil || logouts != 2) {
				t.Fatalf("absent map: error=%v logouts=%d", err, logouts)
			}
		})
	}
}

func TestDisconnectFlushesExactMapAlias(t *testing.T) {
	sys := t.TempDir()
	dir := filepath.Join(sys, "class", "block", "dm-7", "dm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uuid"), []byte("mpath-3589c0053475a3152564332cc948439eb"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte("mpathkx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var commands []string
	c := &Connector{SYSFSPath: sys, Exec: func(name string, args ...string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		commands = append(commands, cmd)
		if strings.HasPrefix(cmd, "multipath -f 3") {
			return []byte("not a multipath device"), fmt.Errorf("exit status 1")
		}
		if cmd == "multipath -f mpathkx" {
			if err := os.RemoveAll(filepath.Dir(dir)); err != nil {
				t.Fatal(err)
			}
		}
		return nil, nil
	}}
	if err := c.Disconnect(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.fixture", "589c0053475a3152564332cc948439eb"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "multipath -f mpathkx") {
		t.Fatalf("alias flush missing: %v", commands)
	}
}

func TestFlushBusyMapRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		busy      int
		other     bool
		wantCalls int
		wantErr   bool
	}{
		{name: "eventual success", busy: 2, wantCalls: 3},
		{name: "remains busy", busy: 10, wantCalls: 10, wantErr: true},
		{name: "other failure", other: true, wantCalls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := t.TempDir()
			dir := filepath.Join(sys, "class", "block", "dm-0", "dm")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "uuid"), []byte("mpath-wwid"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "name"), []byte("mpatha"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := &Connector{SYSFSPath: sys, cleanupRetryInterval: time.Microsecond, Exec: func(_ string, args ...string) ([]byte, error) {
				calls++
				if strings.Join(args, " ") != "-f mpatha" {
					t.Fatalf("unexpected map: %v", args)
				}
				if tc.other {
					return []byte("permission denied"), fmt.Errorf("failed")
				}
				if calls <= tc.busy {
					return []byte("mpatha: map in use"), fmt.Errorf("failed")
				}
				return nil, nil
			}}
			err := c.flushBusyMap(context.Background(), "wwid", "mpatha", 10)
			if (err != nil) != tc.wantErr || calls != tc.wantCalls {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestFlushBusyMapCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := &Connector{cleanupRetryInterval: time.Hour, Exec: func(_ string, _ ...string) ([]byte, error) {
		calls++
		cancel()
		return []byte("map in use"), fmt.Errorf("failed")
	}}
	if err := c.flushBusyMap(ctx, "wwid", "mpatha", 10); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestFlushBusyMapStopsWaitingOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})
	c := &Connector{cleanupRetryInterval: time.Hour, Exec: func(_ string, _ ...string) ([]byte, error) {
		close(called)
		return []byte("map in use"), fmt.Errorf("failed")
	}}
	result := make(chan error, 1)
	go func() { result <- c.flushBusyMap(ctx, "wwid", "mpatha", 10) }()
	<-called
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not stop after cancellation")
	}
}

func TestDisconnectBusyMapKeepsSessions(t *testing.T) {
	sys := t.TempDir()
	dir := filepath.Join(sys, "class", "block", "dm-0", "dm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uuid"), []byte("mpath-3589c0053475a3152564332cc948439eb"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte("mpatha"), 0600); err != nil {
		t.Fatal(err)
	}
	aliases, logouts := 0, 0
	c := &Connector{SYSFSPath: sys, cleanupRetryInterval: time.Microsecond, Exec: func(name string, args ...string) ([]byte, error) {
		if name == "iscsiadm" {
			logouts++
			return nil, nil
		}
		if args[1] == "mpatha" {
			aliases++
			return []byte("mpatha: map in use"), fmt.Errorf("busy")
		}
		return []byte("not a multipath device"), fmt.Errorf("failed")
	}}
	err := c.Disconnect(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "iqn.fixture", "589c0053475a3152564332cc948439eb")
	if err == nil || aliases != 10 || logouts != 0 {
		t.Fatalf("error=%v attempts=%d logouts=%d", err, aliases, logouts)
	}
}

func TestFlushBusyMapRejectsChangedAlias(t *testing.T) {
	sys := t.TempDir()
	dir := filepath.Join(sys, "class", "block", "dm-0", "dm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uuid"), []byte("mpath-wwid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte("new-name"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &Connector{SYSFSPath: sys, cleanupRetryInterval: time.Microsecond, Exec: func(_ string, _ ...string) ([]byte, error) {
		calls++
		return []byte("map in use"), fmt.Errorf("busy")
	}}
	err := c.flushBusyMap(context.Background(), "wwid", "old-name", 10)
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "changed name") {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestLogoutFailsClosedWhenSessionInspectionFails(t *testing.T) {
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "-m session" {
			return nil, fmt.Errorf("inspection failed")
		}
		return nil, nil
	}}
	if err := c.Logout("10.0.0.1", "iqn.fixture"); err == nil {
		t.Fatal("inspection failure must prevent success")
	}
}
func TestLogoutIgnoresOtherTargetSession(t *testing.T) {
	c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "-m session" {
			return []byte("tcp: [2] 10.0.0.1:3260,1 iqn.other\n"), nil
		}
		return nil, nil
	}}
	if err := c.Logout("10.0.0.1", "iqn.fixture"); err != nil {
		t.Fatal(err)
	}
}
