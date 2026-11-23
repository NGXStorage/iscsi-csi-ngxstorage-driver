package helper

import (
	"context"
	"errors"
	"strings"

	kubeutils "github.com/NGXStorage/csi-ngxstorage-utils/kubernetes"

	zap "go.uber.org/zap"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

func IsAmbiguousPV(err error) bool {
	var ambiguous kubeutils.ErrAmbiguousVolumePV
	return errors.As(err, &ambiguous)
}

// volumeID builds a CSI volume ID from a backend LUN ID (SHA1).
func VolumeID(prefix, id string) string { return prefix + id }

// LUNIDFromVolumeIDChecked extracts the backend LUN ID from a CSI volume ID.
// A bare backend LUN ID (no driver prefix) is NOT a volume of this driver:
// the error is NotFound so CSI error semantics hold (Publish/Validate/Modify/
// Expand report a non-existent volume; idempotent DeleteVolume/Unpublish turn
// NotFound into OK; ListSnapshots turns it into an empty list). The bare ID
// never reaches a backend mutation. An empty ID is InvalidArgument.
func LUNIDFromVolumeIDChecked(prefix, id string) (string, error) {
	lunID := strings.TrimPrefix(id, prefix)
	if id == "" {
		return "", status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if lunID == "" || lunID == id {
		return "", status.Error(codes.NotFound, "volume not found: ID does not carry this driver's prefix")
	}
	return lunID, nil
}

// assertNodeExists fails with NotFound when the target Kubernetes node is
// absent, so ControllerPublish/Unpublish never mutate the backend for an
// unknown node. An unreadable cluster is Unavailable, never NotFound.
func AssertNodeExists(ctx context.Context, attachments kubeutils.AttachmentLookup, nodeID string) error {
	lookup, ok := attachments.(kubeutils.NodeLookup)
	if !ok || lookup == nil {
		return nil
	}
	exists, err := lookup.NodeExists(ctx, nodeID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "lookup node %s: %v", nodeID, err)
	}
	if !exists {
		return status.Errorf(codes.NotFound, "node %s not found", nodeID)
	}
	return nil
}

// remainderNodeIDs returns other Kubernetes nodes that still have a
// VolumeAttachment for this CSI volume. API uncertainty is fail-closed because
// deleting a shared target while another node is attaching is destructive.
func RemainderNodeIDs(ctx context.Context, lookup kubeutils.AttachmentLookup, driverName, volumeID, excludingNodeID string, log *zap.Logger) ([]string, error) {
	if lookup == nil {
		return nil, status.Error(codes.Unavailable, "controller attachment lookup unavailable")
	}
	nodes, err := lookup.RemainingNodes(ctx, driverName, volumeID, excludingNodeID)
	if err != nil {
		if IsAmbiguousPV(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "ambiguous volume: %v", err)
		}
		log.Error("remainderNodeIDs(): VolumeAttachment lookup failed; retaining target",
			zap.String("volume_id", volumeID), zap.String("node_id", excludingNodeID), zap.Error(err))
		return nil, status.Errorf(codes.Unavailable, "resolve remaining attachments: %v", err)
	}
	return nodes, nil
}
