package utils

import (
	"errors"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
)

type ErrorClass int

const (
	ErrorInternal ErrorClass = iota
	ErrorNotFound
	ErrorAlreadyExists
	ErrorUnavailable
	ErrorInvalidArgument
)

// ClassifyError maps confirmed NGX response semantics to a storage-level
// class. CSI gRPC mapping remains in BackendStatusError.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorInternal
	}
	if errors.Is(err, ngxstorage.ErrPoolNotFound) {
		return ErrorNotFound
	}
	if errors.Is(err, ngxstorage.ErrClusterNotReady) {
		return ErrorUnavailable
	}
	var transportErr *ngxstorage.TransportError
	if errors.As(err, &transportErr) {
		return ErrorUnavailable
	}
	var apiErr *ngxstorage.APIError
	if errors.As(err, &apiErr) {
		return classifyResponse(apiErr.Code, apiErr.StatusCode)
	}
	return ErrorInternal
}

func classifyResponse(code, statusCode int) ErrorClass {
	switch {
	case code == ngxstorage.CodeNotFound || statusCode == 404:
		return ErrorNotFound
	case code == ngxstorage.CodeAlreadyExists || code == ngxstorage.CodeAuthGroupExists ||
		code == ngxstorage.CodeISCSITargetExists || statusCode == 409:
		return ErrorAlreadyExists
	case code == ngxstorage.CodeBusy || code == ngxstorage.CodeAttachedBusy || statusCode == 429 || statusCode >= 500:
		return ErrorUnavailable
	case statusCode == 400:
		return ErrorInvalidArgument
	default:
		return ErrorInternal
	}
}
