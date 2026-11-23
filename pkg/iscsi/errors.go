package iscsi

import (
	"errors"
	"fmt"
	"os/exec"
)

// iscsiadm exit codes that report a permanent login rejection: 19 is
// authentication failure (CHAP/credentials rejected); 24 is the observed
// rejection when the initiator IQN is not registered/authorized on the
// target. Neither can succeed on retry until credentials or initiator
// registration change.
const (
	exitLoginAuthFailed   = 19
	exitLoginUnauthorized = 24
)

// ErrInvalidDeviceRequest marks malformed connector input (missing portals,
// IQN or SCSI ID, negative LUN number). The request itself must change; a
// retry of the same input cannot succeed.
var ErrInvalidDeviceRequest = errors.New("invalid iSCSI device request")

// ErrDeviceWaitTimeout marks a genuine bounded-wait expiry: the device or
// multipath map did not appear within the connector's wait budget.
var ErrDeviceWaitTimeout = errors.New("iSCSI device wait timed out")

// LoginRejectedError reports a permanent iSCSI login/auth rejection by the
// target. It wraps the underlying iscsiadm failure so the exit detail stays
// inspectable while the cause is classifiable without string matching.
type LoginRejectedError struct {
	Portal   string
	IQN      string
	ExitCode int
	Err      error
}

func (e *LoginRejectedError) Error() string {
	return fmt.Sprintf("iscsiadm login rejected for %s@%s (exit status %d): %v", e.IQN, e.Portal, e.ExitCode, e.Err)
}

// Unwrap exposes the underlying iscsiadm error to errors.Is and errors.As.
func (e *LoginRejectedError) Unwrap() error { return e.Err }

// loginExitCode extracts the iscsiadm exit code from a genuine process exit
// error. Non-exit failures (binary missing, fork failure) carry no code.
func loginExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// isLoginRejected reports whether the login failure is a permanent auth or
// initiator-authorization rejection rather than a transient condition.
func isLoginRejected(err error) bool {
	code, ok := loginExitCode(err)
	return ok && (code == exitLoginAuthFailed || code == exitLoginUnauthorized)
}

// DeviceErrorCause classifies a Connect/WaitForMappedDevice failure by its
// cause so the RPC layer can pick the matching CSI status.
type DeviceErrorCause int

const (
	// DeviceCauseTransient is a retryable host/fabric condition: discovery
	// or login transport failure, device not yet visible, multipath not yet
	// converged. Unknown failures stay in this class.
	DeviceCauseTransient DeviceErrorCause = iota
	// DeviceCauseLoginRejected is a permanent login/auth rejection; the
	// credentials or initiator registration must change before retry.
	DeviceCauseLoginRejected
	// DeviceCauseInvalidRequest is malformed connector input.
	DeviceCauseInvalidRequest
	// DeviceCauseWaitTimeout is a genuine bounded-wait expiry.
	DeviceCauseWaitTimeout
)

// ClassifyDeviceError classifies a connector failure by cause. Context
// cancellation/deadline is not a device cause: callers check errors.Is(err,
// context.Canceled/DeadlineExceeded) first and propagate it unchanged.
// Classification is conservative: only explicit markers produce a permanent
// or timeout class; everything else stays transient so the CO retries an
// uncertain condition instead of treating it as terminal.
func ClassifyDeviceError(err error) DeviceErrorCause {
	var rejected *LoginRejectedError
	switch {
	case errors.As(err, &rejected):
		return DeviceCauseLoginRejected
	case errors.Is(err, ErrInvalidDeviceRequest):
		return DeviceCauseInvalidRequest
	case errors.Is(err, ErrDeviceWaitTimeout):
		return DeviceCauseWaitTimeout
	default:
		return DeviceCauseTransient
	}
}
