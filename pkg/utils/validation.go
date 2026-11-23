package utils

import (
	"context"
	"errors"
	"fmt"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BackendStatusError translates a typed NGX API failure into the CSI status
// expected by the external provisioner. It never retries or changes storage.
func BackendStatusError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	if errors.Is(err, ngxstorage.ErrSnapshotInUse) {
		return status.Errorf(codes.FailedPrecondition, "%s: snapshot is in use", operation)
	}
	if grpcStatus, ok := status.FromError(err); ok && grpcStatus.Code() != codes.Unknown {
		return err
	}
	code := codes.Internal
	switch ClassifyError(err) {
	case ErrorNotFound:
		code = codes.NotFound
	case ErrorAlreadyExists:
		code = codes.AlreadyExists
	case ErrorUnavailable:
		code = codes.Unavailable
	case ErrorInvalidArgument:
		code = codes.InvalidArgument
	}
	return status.Error(code, fmt.Sprintf("%s: %v", operation, err))
}

// ValidateVolumeCapability accepts filesystem mount or raw-block capabilities
// whose access mode is advertised by this block-storage driver.
func ValidateVolumeCapability(capability *csi.VolumeCapability, allowed []csi.VolumeCapability_AccessMode_Mode) error {
	if capability == nil {
		return status.Error(codes.InvalidArgument, "volume capability is required")
	}
	hasMount := capability.GetMount() != nil
	hasBlock := capability.GetBlock() != nil
	if hasMount == hasBlock {
		return status.Error(codes.InvalidArgument, "exactly one mount or block volume capability is required")
	}
	mode := capability.GetAccessMode().GetMode()
	if capability.GetAccessMode() == nil || mode == csi.VolumeCapability_AccessMode_UNKNOWN {
		return status.Error(codes.InvalidArgument, "volume capability access mode is required")
	}
	for _, candidate := range allowed {
		if candidate == mode {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "unsupported volume capability access mode: %s", mode.String())
}
