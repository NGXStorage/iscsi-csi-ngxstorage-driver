package ngxstorage

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config configures a Client. The API key is never logged by the SDK.
type Config struct {
	// Controllers is the list of 1-2 controller IPs (or hostnames).
	Controllers []string
	// APIKey is the bearer token. Never logged.
	APIKey string
	// PoolName is the canonical configured pool (optional; drivers that only
	// query status/hardware may leave it empty).
	PoolName string

	// InsecureSkipVerify defaults to true because NGX Storage Arrays use
	// self-signed certificates and most customers have no private CA/DNS.
	// Set false and provide RootCAs to enforce a customer trust chain.
	InsecureSkipVerify bool
	// RootCAs is an optional customer CA bundle. When set, TLS verification
	// uses it (and InsecureSkipVerify should be false).
	RootCAs *x509.CertPool

	// RoundTrippers is an optional middleware chain wrapped around the SDK
	// transport, outermost-first.
	RoundTrippers []RoundTripper

	// Logger is the pluggable logger. Defaults to NopLogger.
	Logger Logger

	// HTTPClient is an optional test/advanced transport override.
	HTTPClient *http.Client

	// Timeout is the per-request timeout. Defaults to 60s.
	Timeout time.Duration
	// MaxRetries bounds backend-busy (725) attempts. Defaults to 6.
	MaxRetries int
	// BaseDelay and MaxDelay control bounded exponential busy backoff.
	// They default to 10s and 30s.
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Client is the thread-safe entry point for the API v2 operations with
// current consumers and verified contracts. It
// owns the controller selection, bounded 725-busy retry, and transport
// failover. Service accessors return lazily-bound per-resource services.
type Client struct {
	cfg        Config
	apiKey     string
	poolName   string
	logger     Logger
	httpClient *http.Client

	// controller selection state
	controllers     []string
	selectionMu     sync.RWMutex
	controllerIndex int
	lastRefresh     time.Time
	refreshGate     chan struct{}
}

// NewClient validates configuration and constructs a Client. It performs no
// network I/O; callers should invoke RefreshController before serving traffic
// when pool-scoped placement matters.
func NewClient(cfg Config) (*Client, error) {
	controllers := make([]string, 0, len(cfg.Controllers))
	for _, controller := range cfg.Controllers {
		controller = strings.TrimSpace(controller)
		if controller == "" {
			return nil, errors.New("ngxstorage: controller must not be empty")
		}
		controllers = append(controllers, controller)
	}
	if len(controllers) == 0 {
		return nil, errors.New("ngxstorage: at least one controller is required")
	}
	if len(controllers) > 2 {
		return nil, errors.New("ngxstorage: at most 2 controllers supported")
	}
	if len(controllers) == 2 && controllers[0] == controllers[1] {
		return nil, errors.New("ngxstorage: controllers must be unique")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("ngxstorage: api key is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = NopLogger{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 6
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 10 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 30 * time.Second
	}

	client := &Client{
		cfg:         cfg,
		apiKey:      cfg.APIKey,
		poolName:    cfg.PoolName,
		logger:      cfg.Logger,
		controllers: controllers,
		refreshGate: make(chan struct{}, 1),
	}
	client.refreshGate <- struct{}{}

	if cfg.HTTPClient != nil {
		client.httpClient = cfg.HTTPClient
	} else {
		client.httpClient = newHTTPClient(cfg)
	}
	return client, nil
}

// CurrentController returns the currently selected controller IP.
func (c *Client) CurrentController() string {
	c.selectionMu.RLock()
	defer c.selectionMu.RUnlock()
	return c.controllers[c.controllerIndex]
}

// PoolName returns the configured canonical pool name.
func (c *Client) PoolName() string {
	return c.poolName
}

// requestController performs one request against the currently selected
// controller. It is separated so transport failover can re-run the request
// after a refresh.
func (c *Client) requestController(ctx context.Context, method, link string, body interface{}) ([]byte, int, error) {
	return sendRequest(ctx, c.httpClient, c.logger, method, link, c.apiKey, body, retryConfig{
		MaxRetries: c.cfg.MaxRetries,
		BaseDelay:  c.cfg.BaseDelay,
		MaxDelay:   c.cfg.MaxDelay,
	})
}

// request rewrites the endpoint to the current controller and performs the
// request with single transport-failover replay for safe methods.
func (c *Client) request(ctx context.Context, name string, pathVars url.Values, body interface{}) ([]byte, int, error) {
	// Refresh a stale controller selection before serving the request, so a
	// recovered controller or a pool that moved between controllers is picked
	// up without waiting for an explicit failure.
	c.ensureRefresh(ctx)

	link := endpointURL(name, c.CurrentController(), pathVars)
	method := endpointMethod(name)

	resp, code, err := c.requestController(ctx, method, link, body)
	if err == nil {
		return resp, code, nil
	}
	// A deadline-exhausted context is terminal: no failover can succeed. Any
	// other transport/timeout failure triggers a refresh + replay below.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return resp, code, ctxErr
	}
	// Semantic errors return unchanged. The request layer knows the HTTP
	// method and an error code, but not the operation's idempotency key,
	// postcondition or resource ownership rules; a write must never be
	// replayed here. Operations that document "5011 can mean the stale
	// controller" own that recovery at the driver layer (see
	// IsCanonicalNotFoundError).
	if !IsTransportError(err) {
		return resp, code, err
	}
	// Only read and idempotent-delete operations are replayed after an
	// ambiguous transport failure. PUT/PATCH can mutate state just like POST.
	if !isTransportReplaySafe(method) {
		c.logger.Warnf("ngxstorage: suppressing unsafe transport replay for %s", method)
		return resp, code, err
	}
	// Safe methods (GET/DELETE) replay after a controller refresh. This is the
	// on-error controller re-selection path: a transport failure or read
	// timeout re-resolves work mode and fails over to the other controller.
	if refreshErr := c.RefreshController(ctx); refreshErr != nil {
		c.logger.Warnf("ngxstorage: refresh after transport failure: %v", refreshErr)
		return resp, code, fmt.Errorf("ngxstorage: refresh after transport failure: %v: %w", refreshErr, err)
	}
	link = endpointURL(name, c.CurrentController(), pathVars)
	return c.requestController(ctx, method, link, body)
}

func isTransportReplaySafe(method string) bool {
	return method == http.MethodGet || method == http.MethodDelete
}

// get is a convenience wrapper for GET requests with no body.
func (c *Client) get(ctx context.Context, name string, pathVars url.Values) ([]byte, error) {
	body, _, err := c.request(ctx, name, pathVars, nil)
	return body, err
}

// mutation performs a POST/PUT/PATCH/DELETE with a body and returns the raw
// response and a success flag.
func (c *Client) mutation(ctx context.Context, name string, pathVars url.Values, body interface{}) ([]byte, error) {
	resp, _, err := c.request(ctx, name, pathVars, body)
	return resp, err
}
