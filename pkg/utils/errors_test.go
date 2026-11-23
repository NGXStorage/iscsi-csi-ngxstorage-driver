package utils

import (
	"errors"
	"testing"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
)

func TestClassifyError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"configured pool absent", ngxstorage.ErrPoolNotFound, ErrorNotFound},
		{"wrapped pool absence", errors.Join(errors.New("refresh failed"), ngxstorage.ErrPoolNotFound), ErrorNotFound},
		{"cluster unavailable", ngxstorage.ErrClusterNotReady, ErrorUnavailable},
		{"wrapped cluster unavailable", errors.Join(errors.New("refresh failed"), ngxstorage.ErrClusterNotReady), ErrorUnavailable},
		{"transport failure", ngxstorage.NewTransportError("GET", "https://controller/api/v2/share", errors.New("connection refused")), ErrorUnavailable},
		{"wrapped SDK transport failure", errors.Join(errors.New("request failed"), ngxstorage.NewTransportError("GET", "https://controller/api/v2/share", errors.New("connection refused"))), ErrorUnavailable},
		{"not found", &ngxstorage.APIError{StatusCode: 404}, ErrorNotFound},
		{"semantic not found over HTTP 500", &ngxstorage.APIError{Code: 5011, StatusCode: 500}, ErrorNotFound},
		{"duplicate", &ngxstorage.APIError{StatusCode: 409}, ErrorAlreadyExists},
		{"duplicate canonical code", &ngxstorage.APIError{Code: 710, StatusCode: 200}, ErrorAlreadyExists},
		{"wrapped SDK semantic error", errors.Join(errors.New("request failed"), &ngxstorage.APIError{Code: ngxstorage.CodeISCSITargetExists, StatusCode: 200}), ErrorAlreadyExists},
		{"busy", &ngxstorage.APIError{Code: 725, StatusCode: 200}, ErrorUnavailable},
		{"attached busy", &ngxstorage.APIError{Code: 534, StatusCode: 200}, ErrorUnavailable},
		{"bad request", &ngxstorage.APIError{StatusCode: 400}, ErrorInvalidArgument},
		{"server", &ngxstorage.APIError{StatusCode: 503}, ErrorUnavailable},
		{"unknown local error", errors.New("malformed response"), ErrorInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyError(test.err); got != test.want {
				t.Fatalf("ClassifyError() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestReadinessSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ngxstorage.ErrClusterNotReady, ngxstorage.ErrPoolNotFound) || errors.Is(ngxstorage.ErrPoolNotFound, ngxstorage.ErrClusterNotReady) {
		t.Fatal("readiness sentinel errors must remain distinct")
	}
}
