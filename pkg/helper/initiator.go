package helper

import (
	"context"
	"errors"

	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"

	zap "go.uber.org/zap"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// InitiatorRegistrar serializes auth-group IQN membership with the
// controller's per-volume target cleanup. The Lease is held only around the
// backend mutation; login and device discovery stay outside the shared lock.
type InitiatorRegistrar struct {
	Coord coord.Coordinator
	Recon *Reconciler
	Log   *zap.Logger
}

func (ir *InitiatorRegistrar) log() *zap.Logger {
	if ir.Log != nil {
		return ir.Log
	}
	return zap.NewNop()
}

// registerInitiatorIQN serializes auth-group membership with the controller's
// per-volume target cleanup. The Lease is intentionally held only around the
// backend auth-group mutation; iSCSI login and device discovery do not need to
// occupy the shared cluster lock.
func (ir *InitiatorRegistrar) Register(ctx context.Context, volumeID, authGroupID, iqn, alias string) error {
	if ir.Coord == nil {
		return status.Error(codes.Unavailable, "iSCSI initiator registration coordination is unavailable")
	}
	lock, err := ir.Coord.Acquire(ctx, "volume:"+volumeID, coord.AcquireOptions{
		Safety: coord.SafetyOrdinary, Operation: coord.OperationPublish,
	})
	if err != nil {
		return utils.CoordinationError("acquire iSCSI initiator registration lock", err)
	}
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify iSCSI initiator registration lock ownership", err)
		}
		if err := ir.Recon.RegisterIQNReconciled(ctx, authGroupID, iqn, alias); err != nil {
			return status.Errorf(codes.Internal, "register initiator IQN: %v", err)
		}
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return errors.Join(criticalErr, utils.CoordinationError("release iSCSI initiator registration lock", releaseErr))
		}
		return criticalErr
	}
	if releaseErr != nil {
		return utils.CoordinationError("release iSCSI initiator registration lock", releaseErr)
	}
	return nil
}
