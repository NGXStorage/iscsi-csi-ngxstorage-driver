package iscsi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLoginClassifiesPermanentRejection pins the login error taxonomy: exit
// 19 (authentication failure) and exit 24 (initiator not authorized) are
// permanent rejections; other exit codes stay unclassified (transient), and
// exit 15 remains the already-logged-in no-op.
func TestLoginClassifiesPermanentRejection(t *testing.T) {
	for _, code := range []int{19, 24} {
		t.Run(fmt.Sprintf("exit %d is a permanent rejection", code), func(t *testing.T) {
			exitErr := realExitError(t, code)
			c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), " -l") {
					return nil, exitErr
				}
				return nil, nil
			}}
			created, err := c.Login("10.0.0.1:3260", "iqn.test")
			if created || err == nil {
				t.Fatalf("Login = %v, %v, want a failed login", created, err)
			}
			var rejected *LoginRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("exit %d must classify as LoginRejectedError, got %T: %v", code, err, err)
			}
			if rejected.ExitCode != code || rejected.Portal != "10.0.0.1:3260" || rejected.IQN != "iqn.test" {
				t.Fatalf("rejection detail = %+v, want exit %d for the exact portal/IQN", rejected, code)
			}
			if !errors.Is(err, exitErr) {
				t.Fatalf("rejection must wrap the underlying iscsiadm error: %v", err)
			}
		})
	}

	t.Run("transient exit stays unclassified", func(t *testing.T) {
		exitErr := realExitError(t, 11)
		c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), " -l") {
				return nil, exitErr
			}
			return nil, nil
		}}
		_, err := c.Login("10.0.0.1:3260", "iqn.test")
		var rejected *LoginRejectedError
		if err == nil || errors.As(err, &rejected) {
			t.Fatalf("exit 11 must stay a plain transient login error, got %v", err)
		}
	})

	t.Run("already logged in stays a no-op", func(t *testing.T) {
		exitErr := realExitError(t, 15)
		c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), " -l") {
				return nil, exitErr
			}
			return nil, nil
		}}
		created, err := c.Login("10.0.0.1:3260", "iqn.test")
		if created || err != nil {
			t.Fatalf("Login = %v, %v, want (false, nil) for a preexisting session", created, err)
		}
	})

	t.Run("discovery failure stays transient even with an auth exit code", func(t *testing.T) {
		// Conservative classification: only the login step's exit code is
		// authoritative for a permanent rejection; discovery failures keep
		// the retryable default.
		exitErr := realExitError(t, 19)
		c := &Connector{Exec: func(name string, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "--discover") {
				return nil, exitErr
			}
			return nil, nil
		}}
		_, err := c.Login("10.0.0.1:3260", "iqn.test")
		var rejected *LoginRejectedError
		if err == nil || errors.As(err, &rejected) {
			t.Fatalf("discovery failure must stay transient, got %v", err)
		}
	})
}

// TestClassifyDeviceError pins the cause classification, including errors
// wrapped by Connect's portal context and joined with rollback failures.
func TestClassifyDeviceError(t *testing.T) {
	rejected := &LoginRejectedError{Portal: "10.0.0.1:3260", IQN: "iqn.test", ExitCode: 19, Err: realExitError(t, 19)}
	cases := []struct {
		name string
		err  error
		want DeviceErrorCause
	}{
		{"direct rejection", rejected, DeviceCauseLoginRejected},
		{"wrapped rejection", fmt.Errorf("login portal 10.0.0.1:3260: %w", rejected), DeviceCauseLoginRejected},
		{"rejection joined with rollback error", errors.Join(fmt.Errorf("login portal 10.0.0.1:3260: %w", rejected), errors.New("rollback logout portal 10.0.0.2: refused")), DeviceCauseLoginRejected},
		{"invalid request", fmt.Errorf("%w: at least one iSCSI portal is required", ErrInvalidDeviceRequest), DeviceCauseInvalidRequest},
		{"wait timeout", fmt.Errorf("wait for portal 10.0.0.1:3260: %w: timed out waiting for device iqn.test lun 4", ErrDeviceWaitTimeout), DeviceCauseWaitTimeout},
		{"transient exec failure", fmt.Errorf("iscsiadm login: %w", realExitError(t, 11)), DeviceCauseTransient},
		{"unknown failure", errors.New("unexpected host condition"), DeviceCauseTransient},
		{"nil error", nil, DeviceCauseTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyDeviceError(tc.err); got != tc.want {
				t.Fatalf("ClassifyDeviceError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConnectorInputAndTimeoutMarkers pins that malformed input and genuine
// wait expiries carry their classification markers at the source.
func TestConnectorInputAndTimeoutMarkers(t *testing.T) {
	t.Run("connect without portals is invalid input", func(t *testing.T) {
		c := &Connector{}
		_, err := c.Connect(context.Background(), nil, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
		if !errors.Is(err, ErrInvalidDeviceRequest) {
			t.Fatalf("err = %v, want ErrInvalidDeviceRequest", err)
		}
	})

	t.Run("mapped-device validation is invalid input", func(t *testing.T) {
		c := &Connector{}
		for _, tc := range []struct {
			portals []string
			iqn     string
			lun     int
			scsiID  string
		}{
			{nil, "iqn.test", 4, "589c005636424757595041e52a73d6d3"},
			{[]string{"10.0.0.1"}, "", 4, "589c005636424757595041e52a73d6d3"},
			{[]string{"10.0.0.1"}, "iqn.test", -1, "589c005636424757595041e52a73d6d3"},
			{[]string{"10.0.0.1"}, "iqn.test", 4, ""},
			{[]string{" "}, "iqn.test", 4, "589c005636424757595041e52a73d6d3"},
		} {
			_, err := c.WaitForMappedDevice(context.Background(), tc.portals, tc.iqn, tc.lun, tc.scsiID)
			if !errors.Is(err, ErrInvalidDeviceRequest) {
				t.Fatalf("WaitForMappedDevice(%v, %q, %d, %q) = %v, want ErrInvalidDeviceRequest", tc.portals, tc.iqn, tc.lun, tc.scsiID, err)
			}
		}
	})

	t.Run("multipath without SCSI ID is invalid input", func(t *testing.T) {
		c := &Connector{}
		_, err := c.WaitForMultipathDevice(context.Background(), "")
		if !errors.Is(err, ErrInvalidDeviceRequest) {
			t.Fatalf("err = %v, want ErrInvalidDeviceRequest", err)
		}
	})

	t.Run("device wait expiry is a wait timeout", func(t *testing.T) {
		c := &Connector{DEVRoot: t.TempDir(), WaitInterval: time.Millisecond, WaitTimeout: 10 * time.Millisecond}
		_, err := c.WaitForDevice(context.Background(), "1.2.3.4:3260", "iqn.tgt", 1)
		if !errors.Is(err, ErrDeviceWaitTimeout) {
			t.Fatalf("err = %v, want ErrDeviceWaitTimeout", err)
		}
	})

	t.Run("connect empty portal is invalid input", func(t *testing.T) {
		c := &Connector{Exec: func(string, ...string) ([]byte, error) { return nil, nil }}
		_, err := c.Connect(context.Background(), []string{" "}, "iqn.test", 4, "589c005636424757595041e52a73d6d3")
		if !errors.Is(err, ErrInvalidDeviceRequest) {
			t.Fatalf("err = %v, want ErrInvalidDeviceRequest", err)
		}
	})
}

// TestLoginRejectedErrorSurvivesExecErrorType pins that the rejection marker
// composes with the concrete process exit error used by production exec.
func TestLoginRejectedErrorSurvivesExecErrorType(t *testing.T) {
	exitErr := realExitError(t, 24)
	rejected := &LoginRejectedError{Portal: "10.0.0.1:3260", IQN: "iqn.test", ExitCode: 24, Err: exitErr}
	var exit *exec.ExitError
	if !errors.As(rejected, &exit) || exit.ExitCode() != 24 {
		t.Fatalf("rejection must expose the underlying exit error: %v", rejected)
	}
	if !strings.Contains(rejected.Error(), "exit status 24") {
		t.Fatalf("rejection message must carry the exit detail: %s", rejected.Error())
	}
}
