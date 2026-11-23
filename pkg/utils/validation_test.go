package utils

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func mountCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
	}
}

func TestValidateVolumeCapability(t *testing.T) {
	modes := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	}
	if err := ValidateVolumeCapability(mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER), modes); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []*csi.VolumeCapability{
		nil,
		mountCapability(csi.VolumeCapability_AccessMode_UNKNOWN),
	} {
		if err := ValidateVolumeCapability(capability, modes); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("ValidateVolumeCapability(%v) code = %s", capability, status.Code(err))
		}
	}
}

func TestValidateVolumeCapabilityBlock(t *testing.T) {
	capability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		},
	}
	allowed := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	}
	if err := ValidateVolumeCapability(capability, allowed); err != nil {
		t.Fatalf("raw block capability rejected: %v", err)
	}
}

func TestBackendStatusErrorPreservesContextStatus(t *testing.T) {
	for _, test := range []struct {
		err      error
		wantCode codes.Code
	}{
		{err: context.Canceled, wantCode: codes.Canceled},
		{err: context.DeadlineExceeded, wantCode: codes.DeadlineExceeded},
	} {
		if err := BackendStatusError("backend request", test.err); status.Code(err) != test.wantCode {
			t.Fatalf("BackendStatusError(%v) code = %s, want %s", test.err, status.Code(err), test.wantCode)
		}
	}
}

func TestBackendStatusErrorMapsReadinessFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{name: "configured pool absent", err: ngxstorage.ErrPoolNotFound, wantCode: codes.NotFound},
		{name: "snapshot dependency", err: ngxstorage.ErrSnapshotInUse, wantCode: codes.FailedPrecondition},
		{name: "cluster unavailable", err: ngxstorage.ErrClusterNotReady, wantCode: codes.Unavailable},
		{name: "wrapped cluster unavailable", err: errors.Join(errors.New("refresh failed"), ngxstorage.ErrClusterNotReady), wantCode: codes.Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := BackendStatusError("backend request", test.err)
			if status.Code(err) != test.wantCode {
				t.Fatalf("BackendStatusError(%v) code = %s, want %s", test.err, status.Code(err), test.wantCode)
			}
		})
	}
}
