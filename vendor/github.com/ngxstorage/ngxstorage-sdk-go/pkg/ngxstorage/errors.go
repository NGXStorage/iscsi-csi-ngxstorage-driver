package ngxstorage

import (
	"errors"
	"fmt"
	"strings"
)

// Backend error codes returned by the NGX Storage Manager API v2. These are
// the canonical numeric codes observed in live responses.
const (
	// CodeBusy marks the pool as busy under heavy I/O. Retried with bounded
	// exponential backoff.
	CodeBusy = 725
	// CodeNotFound marks a referenced resource as absent. Idempotent delete
	// operations treat it as success.
	CodeNotFound = 5011
	// CodeAlreadyExists marks a create of a name that already exists.
	CodeAlreadyExists = 710
	// CodeAuthGroupExists is returned when an auth group create collides with
	// an existing group of the same name (backend uses a distinct code from
	// the generic already-exists).
	CodeAuthGroupExists = 519
	// CodeISCSITargetExists is returned when an iSCSI target create collides
	// with an existing target of the same name (distinct from the generic
	// already-exists code).
	CodeISCSITargetExists = 525
	// CodeAttachedBusy marks a resource that cannot be mutated while attached.
	CodeAttachedBusy = 534
	// CodeIQNNotFound / CodeIQNExists / CodeTargetDeleted are iSCSI-specific
	// ignorable codes.
	CodeIQNNotFound   = 521
	CodeIQNExists     = 522
	CodeTargetDeleted = 611
	// CodeUnauthorized is returned when the API key is rejected.
	CodeUnauthorized = 5000
)

// APIError describes a semantic backend error returned by the NGX API v2.
// It carries the HTTP status, the canonical NGX error code and the server
// message. Raw response bodies are never retained, and any occurrence of the
// configured API key inside a server-provided message is redacted so a
// server/proxy that echoes request material cannot leak credentials through
// the error surface.
type APIError struct {
	StatusCode int
	Code       int
	Message    string
	Method     string
	Endpoint   string
	// Body is the raw response body.
	//
	// Deprecated: never populated; always nil.
	Body []byte
}

// redactSecret replaces every occurrence of secret in text with a marker.
// It is a no-op for an empty secret.
func redactSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}

// Error renders the API status, NGX code, message, and the request method and
// endpoint. It carries no credentials: the SDK never places secrets in the URL.
func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("ngxstorage api error: method=%s endpoint=%s http_status=%d code=%d message=%q", e.Method, e.Endpoint, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("ngxstorage api error: method=%s endpoint=%s http_status=%d message=%q", e.Method, e.Endpoint, e.StatusCode, e.Message)
}

// TransportError describes a network-level failure reaching the backend.
type TransportError struct {
	Method   string
	Endpoint string
	Err      error
}

// Error renders the transport failure with the request method and endpoint.
func (e *TransportError) Error() string {
	return fmt.Sprintf("ngxstorage transport error: method=%s endpoint=%s: %v", e.Method, e.Endpoint, e.Err)
}

// Unwrap exposes the underlying network error to errors.Is and errors.As.
func (e *TransportError) Unwrap() error { return e.Err }

// NewTransportError constructs a TransportError for a request-level failure.
func NewTransportError(method, endpoint string, err error) error {
	return &TransportError{Method: method, Endpoint: endpoint, Err: err}
}

// Sentinel errors for backend lifecycle and readiness.
var (
	// ErrSnapshotInUse means a clone still depends on the snapshot.
	ErrSnapshotInUse = errors.New("ngxstorage: snapshot is in use")
	// ErrPoolNotFound means every required pool list confirmed the configured
	// pool is absent.
	ErrPoolNotFound = errors.New("ngxstorage: pool not found")
	// ErrClusterNotReady means controller roles do not form a serviceable work
	// mode, or placement is ambiguous.
	ErrClusterNotReady = errors.New("ngxstorage: cluster not ready")
)

// Classify maps an APIError to a driver-friendly kind. Drivers use this to
// translate backend errors into CSI status codes without string matching.
func Classify(err error) (kind string, code int, ok bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return "", 0, false
	}
	// Some released endpoints signal absence with HTTP 404 but omit the NGX
	// numeric error code. Treat that transport-level semantic as not_found so
	// idempotent cleanup works consistently across endpoint variants.
	if apiErr.StatusCode == 404 && apiErr.Code == 0 {
		return "not_found", 0, true
	}
	return ClassifyCode(apiErr.Code), apiErr.Code, true
}

// ClassifyCode maps a canonical NGX error code to a stable kind string.
func ClassifyCode(code int) string {
	switch code {
	case CodeBusy:
		return "busy"
	case CodeNotFound, CodeIQNNotFound, CodeTargetDeleted:
		return "not_found"
	case CodeAlreadyExists, CodeIQNExists, CodeAuthGroupExists, CodeISCSITargetExists:
		return "already_exists"
	case CodeAttachedBusy:
		return "attached_busy"
	case CodeUnauthorized:
		return "unauthorized"
	default:
		return "unknown"
	}
}

// IsNotFound reports whether the error carries an idempotent not-found code.
func IsNotFound(err error) bool {
	kind, _, ok := Classify(err)
	return ok && kind == "not_found"
}

// IsCanonicalNotFoundError reports whether err is the canonical NGX not-found:
// an APIError carrying code 5011. A code-less HTTP 404 is deliberately NOT
// matched. Operations on pool-owned resources document that a canonical 5011
// can mean the request reached the stale controller; that recovery belongs to
// the driver operation, which knows the postcondition. A code-less 404 is a
// routing/object error and must never be turned into a write on another
// controller.
func IsCanonicalNotFoundError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == CodeNotFound
}

// IsAlreadyExists reports whether the error carries an already-exists code.
func IsAlreadyExists(err error) bool {
	kind, _, ok := Classify(err)
	return ok && kind == "already_exists"
}

// IsBusy reports whether the error carries the pool-busy code.
func IsBusy(err error) bool {
	kind, _, ok := Classify(err)
	return ok && kind == "busy"
}

// IsUnauthorized reports whether the error carries an auth failure.
func IsUnauthorized(err error) bool {
	kind, _, ok := Classify(err)
	return ok && kind == "unauthorized"
}

// IsTransportError reports whether err is (or wraps) a TransportError.
func IsTransportError(err error) bool {
	var t *TransportError
	return errors.As(err, &t)
}

// HTTPStatusFromError returns the HTTP status code carried by an APIError, or
// 0 for non-API errors.
func HTTPStatusFromError(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}
