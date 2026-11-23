package ngxstorage

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultTimeout = 60 * time.Second

// RoundTripper is the middleware hook drivers use to wrap SDK HTTP requests.
// It matches net/http's http.RoundTripper so standard middleware (tracing,
// metrics, custom retry) composes naturally.
type RoundTripper interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

// newHTTPClient builds the SDK HTTP transport with TLS and middleware wiring.
func newHTTPClient(cfg Config) *http.Client {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // self-signed NGX Storage Arrays
	}
	if cfg.RootCAs != nil {
		tlsCfg.RootCAs = cfg.RootCAs
	}

	transport := &http.Transport{TLSClientConfig: tlsCfg}

	var rt RoundTripper = transport
	// Wrap middleware outermost-first: iterate in reverse so cfg order runs
	// outer-to-inner.
	for i := len(cfg.RoundTrippers) - 1; i >= 0; i-- {
		inner := rt
		outer := cfg.RoundTrippers[i]
		rt = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return outer.RoundTrip(withInner(req, inner))
		})
	}

	return &http.Client{Transport: rt, Timeout: cfg.Timeout}
}

// roundTripperFunc adapts a func to RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// withInner is a context-based escape hatch so a middleware can delegate to
// the next hop. Standard middleware wraps the RoundTripper directly; this
// helper is retained for advanced middleware that needs the inner transport.
func withInner(req *http.Request, inner RoundTripper) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), innerKey{}, inner))
}

type innerKey struct{}

// retryConfig bounds the 725-busy retry behavior. Transport failures are
// never retried here: retry/backoff for them belongs to the caller or driver.
type retryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// sendRequest performs one bounded request with 725-busy retry and returns
// the raw body, HTTP status, and a typed error for semantic failures. A
// transport failure is returned immediately (no generic retry).
func sendRequest(
	ctx context.Context,
	client *http.Client,
	logger Logger,
	method, urlStr, apiKey string,
	body interface{},
	cfg retryConfig,
) ([]byte, int, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("ngxstorage: marshal request body: %w", err)
		}
	}
	for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
		if err != nil {
			return nil, 0, fmt.Errorf("ngxstorage: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, ctxErr
			}
			// Transport failures are never retried here: a post-send failure
			// may already have been processed, and operation-specific retry
			// belongs to the caller or driver. Safe methods get at most one
			// cross-controller replay at the request layer instead.
			return nil, 0, NewTransportError(method, urlStr, err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, 0, NewTransportError(method, urlStr, readErr)
		}

		code, msg := responseError(respBody)
		if code == CodeBusy && attempt < cfg.MaxRetries {
			logger.Warnf("ngxstorage: pool busy (725), attempt %d/%d", attempt, cfg.MaxRetries)
			delay := cfg.BaseDelay
			for r := 1; r < attempt && delay < cfg.MaxDelay; r++ {
				delay *= 2
				if delay > cfg.MaxDelay {
					delay = cfg.MaxDelay
				}
			}
			if err := sleepContext(ctx, delay); err != nil {
				return respBody, resp.StatusCode, err
			}
			continue
		}

		// Released NGX endpoints return JSON. Empty/malformed bodies are
		// protocol failures.
		if len(bytes.TrimSpace(respBody)) == 0 || !json.Valid(respBody) {
			return respBody, resp.StatusCode, &APIError{
				StatusCode: resp.StatusCode,
				Method:     method,
				Endpoint:   urlStr,
			}
		}

		if resp.StatusCode >= 400 || code != 0 {
			return respBody, resp.StatusCode, &APIError{
				StatusCode: resp.StatusCode,
				Code:       code,
				Message:    redactSecret(msg, apiKey),
				Method:     method,
				Endpoint:   urlStr,
			}
		}

		return respBody, resp.StatusCode, nil
	}
	return nil, 0, NewTransportError(method, urlStr, errors.New("ngxstorage: retries exhausted"))
}

// responseError extracts the canonical {code, error} fields from a body.
func responseError(body []byte) (int, string) {
	var resp struct {
		Code  json.Number `json:"code"`
		Error string      `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, ""
	}
	code, _ := resp.Code.Int64()
	return int(code), resp.Error
}

// sleepContext waits for delay or returns when ctx ends.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
