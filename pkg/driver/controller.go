package driver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	ngxnames "github.com/NGXStorage/csi-ngxstorage-utils/names"
	helper "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/helper"
	"github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/iscsi"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"
	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// volumeIDPrefix prefixes CSI volume IDs so they are unique per driver.
const volumeIDPrefix = "iscsi.csi.ngxstorage.com."

// CreateVolume provisions a new LUN on the configured pool.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	d.log().Info("CreateVolume(): init")
	d.log().Debug("CreateVolume(): request",
		zap.String("name", req.GetName()),
		zap.Reflect("capacity_range", req.GetCapacityRange()),
		zap.Reflect("volume_capabilities", req.GetVolumeCapabilities()),
		zap.Reflect("parameters", req.GetParameters()),
		zap.Bool("has_content_source", req.GetVolumeContentSource() != nil),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetName() == "" {
		d.log().Error("CreateVolume(): volume name is required")
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}

	// Mutable parameters are the optional VolumeAttributesClass surface; they
	// must be a supported, well-typed set or the request is InvalidArgument.
	if len(req.GetMutableParameters()) > 0 {
		if _, err := helper.NormalizeLUNMutableParameters(req.GetMutableParameters()); err != nil {
			d.log().Error("CreateVolume(): mutable parameters rejected", zap.Error(err))
			return nil, err
		}
	}

	if req.GetCapacityRange().GetRequiredBytes() < 0 || req.GetCapacityRange().GetLimitBytes() < 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity values must not be negative")
	}
	// Resolve the requested size in bytes; default to 1 GiB.
	capacityBytes := helper.MinVolumeBytes
	if req.GetCapacityRange() != nil && req.GetCapacityRange().GetRequiredBytes() > 0 {
		capacityBytes = req.GetCapacityRange().GetRequiredBytes()
	}
	const gib = int64(1 << 30)
	sizeGB := (capacityBytes-1)/gib + 1
	if sizeGB > (1<<63-1)/gib {
		return nil, status.Error(codes.OutOfRange, "capacity exceeds whole-GiB range")
	}
	if sizeGB < 1 {
		sizeGB = 1
	}
	if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && sizeGB*gib > limit {
		return nil, status.Errorf(codes.OutOfRange,
			"requested capacity cannot be satisfied within limit: provisioned=%d limit=%d", sizeGB*gib, limit)
	}

	// Validate every requested capability against the advertised access modes.
	if len(req.GetVolumeCapabilities()) == 0 {
		d.log().Error("CreateVolume(): volume capabilities are required")
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	var fsType string
	for _, capability := range req.GetVolumeCapabilities() {
		if err := utils.ValidateVolumeCapability(capability, d.AccessModes); err != nil {
			d.log().Error("CreateVolume(): volume capability is invalid",
				zap.Reflect("volume_capability", capability),
				zap.Error(err),
			)
			return nil, err
		}
		if mount := capability.GetMount(); mount != nil {
			if mount.GetFsType() != "" {
				fsType = mount.GetFsType()
			}
		}
	}
	if fsType == "" {
		fsType = "ext4"
	}

	// Snapshot clone path: VolumeContentSource.Snapshot.
	var sourceSnapshotID string
	if src := req.GetVolumeContentSource(); src != nil {
		if snap := src.GetSnapshot(); snap != nil {
			sourceSnapshotID = snap.GetSnapshotId()
		} else if src.GetVolume() != nil {
			d.log().Warn("CreateVolume(): direct volume cloning is unsupported")
			return nil, status.Error(codes.Unimplemented, "NGX Storage does not support direct volume cloning")
		}
	}

	name := ngxnames.ModifyVolName(req.GetName())
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "create volume coordination is unavailable")
	}

	lockKeys := []string{"create-volume:" + name}
	if sourceSnapshotID != "" {
		lockKeys = append(lockKeys, "snapshot:"+sourceSnapshotID)
	}

	var lock coord.Lock
	var lockSet coord.LockSet
	var err error
	if sourceSnapshotID == "" {
		lock, err = d.coordination.Acquire(ctx, lockKeys[0], coord.AcquireOptions{
			Safety: coord.SafetyOrdinary, Operation: coord.OperationCreate,
		})
	} else {
		lockSet, err = d.coordination.AcquireMany(ctx, lockKeys, coord.AcquireOptions{
			Safety: coord.SafetyOrdinary, Operation: coord.OperationCreate,
		})
	}
	if err != nil {
		return nil, utils.CoordinationError("acquire create volume lock", err)
	}

	var response *csi.CreateVolumeResponse
	criticalErr := func() error {
		assertOwned := func() error {
			if lockSet != nil {
				return lockSet.AssertOwned(ctx)
			}
			return lock.AssertOwned(ctx)
		}
		if err := assertOwned(); err != nil {
			return utils.CoordinationError("verify create volume lock ownership", err)
		}

		var sourceSnapshot *ngxstorage.Snapshot
		if sourceSnapshotID != "" {
			sourceSnapshot, err = d.client.Snapshots().Get(ctx, sourceSnapshotID)
			if err != nil {
				if ngxstorage.IsNotFound(err) {
					return status.Errorf(codes.NotFound, "source snapshot %s not found", sourceSnapshotID)
				}
				return utils.BackendStatusError("get source snapshot", err)
			}
			if sourceSnapshot == nil || sourceSnapshot.ID != sourceSnapshotID {
				return status.Error(codes.NotFound, "source snapshot not found")
			}
		}

		// The same-name idempotency check must be authoritative and happen under
		// the pre-ID name lock. Backend list failure is never interpreted as absent.
		existing, err := d.reconciler().LUNByNameStrict(ctx, name)
		if err != nil {
			return status.Errorf(codes.Unavailable, "list LUNs for create idempotency: %v", err)
		}
		if existing != nil {
			existing, err = d.reconciler().ResizeRestoredLUN(ctx, existing, req, sourceSnapshot, assertOwned)
			if err != nil {
				return err
			}
			if err := helper.ValidateExistingCreateVolume(existing, req, sourceSnapshot); err != nil {
				return err
			}
			d.log().Info("CreateVolume(): volume already exists (idempotent)",
				zap.String("name", name), zap.String("lun_id", existing.ID))
			response = helper.CreateVolumeResponse(volumeIDPrefix, existing.ID, int64(existing.Size), name, fsType, d.poolOwner, req)
			return nil
		}

		if err := assertOwned(); err != nil {
			return utils.CoordinationError("verify create volume lock before backend mutation", err)
		}
		var lunID string
		if sourceSnapshotID != "" {
			lunID, err = d.client.Snapshots().Clone(ctx, sourceSnapshotID, name)
			if err != nil {
				return utils.BackendStatusError("clone snapshot", err)
			}
		} else {
			opts, optsErr := helper.LUNCreateOptionsFromParams(req.GetParameters())
			if optsErr != nil {
				return optsErr
			}
			lun, createErr := d.reconciler().CreateLUNReconciled(ctx, name, sizeGB, opts)
			if createErr != nil {
				d.log().Error("CreateVolume(): backend rejected LUN create",
					zap.String("name", name),
					zap.String("pool_name", d.client.PoolName()),
					zap.Error(createErr),
				)
				return utils.BackendStatusError("create LUN", createErr)
			}
			if lun == nil || lun.ID == "" {
				return status.Error(codes.Internal, "create LUN: backend returned an invalid LUN")
			}
			lunID = lun.ID
		}

		// Keep both the name lock and source snapshot lock until the created LUN
		// identity/content contract has been read back and verified.
		created, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("read back created LUN", err)
		}
		if created == nil || created.ID != lunID || created.Name != name {
			return status.Errorf(codes.Internal,
				"created LUN readback mismatch: id=%q name=%q expected_id=%q expected_name=%q",
				created.ID, created.Name, lunID, name)
		}
		created, err = d.reconciler().ResizeRestoredLUN(ctx, created, req, sourceSnapshot, assertOwned)
		if err != nil {
			return err
		}
		if err := helper.ValidateExistingCreateVolume(created, req, sourceSnapshot); err != nil {
			return status.Errorf(codes.Internal, "created LUN is incompatible with request: %v", err)
		}
		response = helper.CreateVolumeResponse(volumeIDPrefix, created.ID, int64(created.Size), name, fsType, d.poolOwner, req)
		d.log().Info("CreateVolume(): volume created", zap.String("lun_id", created.ID))
		return nil
	}()

	var releaseErr error
	if lockSet != nil {
		releaseErr = utils.ReleaseOperationLockSet(lockSet)
	} else {
		releaseErr = utils.ReleaseOperationLock(lock)
	}
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release create volume lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release create volume lock", releaseErr)
	}
	return response, nil
}

// DeleteVolume deletes a LUN after ensuring it has no snapshots and is not
// mapped to any iSCSI target. Success is reported only after the backend
// confirms the LUN is absent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	d.log().Info("DeleteVolume(): init")
	d.log().Debug("DeleteVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetVolumeId() == "" {
		d.log().Error("DeleteVolume(): volume ID is required")
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// CSI idempotency: a volume that is not this driver's (or does not
			// exist) is already deleted; never touch the backend for it.
			d.log().Info("DeleteVolume(): volume not owned by this driver; idempotent OK", zap.String("volume_id", req.GetVolumeId()))
			return &csi.DeleteVolumeResponse{}, nil
		}
		d.log().Error("DeleteVolume(): invalid volume ID", zap.String("volume_id", req.GetVolumeId()))
		return nil, err
	}
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "delete volume coordination is unavailable")
	}
	lock, err := d.coordination.Acquire(ctx, "volume:"+req.GetVolumeId(), coord.AcquireOptions{
		Safety: coord.SafetyDestructive, Operation: coord.OperationDelete,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire delete volume lock", err)
	}
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify delete volume lock ownership", err)
		}

		remaining, err := helper.RemainderNodeIDs(ctx, d.attachmentLookup, d.Name, req.GetVolumeId(), "", d.log())
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return status.Errorf(codes.FailedPrecondition,
				"volume still has active attachments: %s", strings.Join(remaining, ","))
		}

		lun, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			if ngxstorage.IsNotFound(err) {
				if cleanupErr := d.reconciler().CleanupOrphanedVolumeScopedAuthGroup(ctx, lock, "delete volume lock", req.GetVolumeId()); cleanupErr != nil {
					return cleanupErr
				}
				return nil
			}
			return utils.BackendStatusError("get LUN before delete", err)
		}
		if lun == nil || lun.ID != lunID {
			return status.Error(codes.Internal, "get LUN before delete: backend response has an invalid id")
		}

		if targetID, mapped, err := d.reconciler().FindMappedISCSITarget(ctx, lunID); err != nil {
			return status.Errorf(codes.Unavailable, "resolve iSCSI mapping before delete: %v", err)
		} else if mapped {
			return status.Errorf(codes.FailedPrecondition,
				"volume is still mapped to iSCSI target %s", targetID)
		}
		if strings.TrimSpace(lun.Protocols["iscsi"]) != "" {
			return status.Errorf(codes.FailedPrecondition,
				"volume still reports iSCSI protocol mapping %q", lun.Protocols["iscsi"])
		}

		snaps, err := d.client.Snapshots().ListDetail(ctx, lunID)
		if err != nil {
			return status.Errorf(codes.Unavailable, "list snapshots before delete: %v", err)
		}
		for _, snap := range snaps {
			if snap.VolumeID == lunID {
				return status.Errorf(codes.FailedPrecondition,
					"volume still has snapshot %s", snap.ID)
			}
		}

		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify delete volume lock before backend mutation", err)
		}
		if err := d.client.LUNs().Delete(ctx, lunID); err != nil {
			return utils.BackendStatusError("delete LUN", err)
		}
		// Read-after-delete: a 200-success delete response is not proof of
		// backend absence. Verify authoritative NotFound before claiming CSI
		// success or running cleanup; a still-present LUN must surface as an
		// error so the caller retries instead of attesting a deletion that
		// never happened.
		if err := d.reconciler().VerifyLUNDeleted(ctx, lunID); err != nil {
			return err
		}
		d.log().Info("DeleteVolume(): volume deleted (backend absence verified)", zap.String("lun_id", lunID))
		if err := d.reconciler().CleanupOrphanedVolumeScopedAuthGroup(ctx, lock, "delete volume lock", req.GetVolumeId()); err != nil {
			return err
		}
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release delete volume lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release delete volume lock", releaseErr)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume creates a per-node iSCSI target (auth group +
// target) and maps the LUN to it, returning the target IQN, portal IPs, SCSI
// LUN number, and SCSI ID in the publish context.
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	d.log().Info("ControllerPublishVolume(): init")
	d.log().Debug("ControllerPublishVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("node_id", req.GetNodeId()),
		zap.Bool("readonly", req.GetReadonly()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetVolumeId() == "" || req.GetNodeId() == "" {
		d.log().Error("ControllerPublishVolume(): volume ID and node ID are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and node ID are required")
	}
	if req.GetVolumeCapability() == nil {
		d.log().Error("ControllerPublishVolume(): volume capability is required")
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	// The referenced volume and node must exist before any backend mutation;
	// CSI requires NotFound for either missing side.
	lun, err := d.client.LUNs().Get(ctx, lunID)
	if err != nil {
		return nil, utils.BackendStatusError("lookup volume", err)
	}
	if lun == nil || lun.ID == "" || lun.ID != lunID {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	if err := helper.AssertNodeExists(ctx, d.attachmentLookup, req.GetNodeId()); err != nil {
		return nil, err
	}

	// Resolve the portal group before the target create: the backend requires
	// both auth_group_id and portal_group_id when creating an iSCSI target.
	// A configured portal group that no longer exists fails closed with its
	// own status (NotFound); only unclassified errors become Internal.
	portalGroupID, portals, err := d.reconciler().SelectPortalGroup(ctx)
	if err != nil {
		d.log().Error("ControllerPublishVolume(): portal group resolution failed", zap.Error(err))
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "resolve portal group: %v", err)
	}

	// The backend keeps every LUN mapped to a target on the SAME controller
	// ("all volumes in the scsi target must belong to the same controller").
	// LUN placement is pool-scoped, so the pool owner (validated against the
	// portal group owner at startup) is the owner for the target create and
	// LUN map. The volume context may carry an explicit owner from create.
	owner := req.GetVolumeContext()["owner"]
	if owner == "" {
		owner = d.poolOwner
	}
	if owner == "" {
		d.log().Error("ControllerPublishVolume(): cannot resolve pool owner")
		return nil, status.Errorf(codes.Internal, "resolve pool owner")
	}

	// Create (or reuse) a per-volume iSCSI target and map the LUN to it. The
	// entire target/auth-group/LUN lifecycle is volume-scoped and serialized
	// across controller replicas; existing targets keep their current auth group.
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "iSCSI publish coordination is unavailable")
	}
	volumeLock, err := d.coordination.Acquire(ctx, "volume:"+req.GetVolumeId(), coord.AcquireOptions{
		Safety: coord.SafetyOrdinary, Operation: coord.OperationPublish,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire iSCSI publish volume lock", err)
	}
	var response *csi.ControllerPublishVolumeResponse
	criticalErr := func() error {
		if err := volumeLock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify iSCSI publish lock ownership", err)
		}

		targetName := ngxnames.ModifyVolName(req.GetVolumeId())
		target, authGroupID, err := d.reconciler().ResolveOrCreateSharedISCSITarget(ctx, targetName, portalGroupID, owner)
		if err != nil {
			d.log().Error("ControllerPublishVolume(): iSCSI target creation failed", zap.Error(err))
			if status.Code(err) != codes.Unknown {
				return err
			}
			return status.Errorf(codes.Internal, "create iSCSI target: %v", err)
		}
		if err := volumeLock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify iSCSI publish lock before LUN mapping", err)
		}
		if err := d.reconciler().AddLUNToTargetReconciled(ctx, target.ID, lunID, owner); err != nil {
			d.log().Error("ControllerPublishVolume(): LUN mapping failed",
				zap.String("target_id", target.ID),
				zap.String("lun_id", lunID),
				zap.Error(err),
			)
			return status.Errorf(codes.Internal, "map LUN to iSCSI target: %v", err)
		}

		targetDetail, err := d.client.ISCSITargets().Get(ctx, target.ID)
		if err != nil {
			d.log().Error("ControllerPublishVolume(): iSCSI target get failed", zap.Error(err))
			return status.Errorf(codes.Internal, "get iSCSI target: %v", err)
		}
		lunNum, err := iscsi.TargetLUNNumber(targetDetail, lunID)
		if err != nil {
			d.log().Error("ControllerPublishVolume(): LUN number lookup failed", zap.Error(err))
			return status.Errorf(codes.Internal, "get LUN number: %v", err)
		}

		lun, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			d.log().Error("ControllerPublishVolume(): LUN detail lookup failed", zap.Error(err))
			return status.Errorf(codes.Internal, "get LUN detail: %v", err)
		}

		// An existing target keeps the auth group it was created with: on a reused
		// per-volume target every node registers its initiator IQN into that exact
		// group. New 11.1 groups are deterministic volume-scoped resources.
		targetAuthGroupID := targetDetail.AuthGroupID
		if targetAuthGroupID == "" {
			targetAuthGroupID = authGroupID
		}

		publishContext := map[string]string{
			"target_iqn":        targetDetail.TargetIQN,
			"portals":           strings.Join(portals, ","),
			"lun_number":        fmt.Sprintf("%d", lunNum),
			"scsi_id":           lun.ScsiID,
			"auth_group":        targetAuthGroupID,
			"fstype":            req.GetVolumeContext()["fstype"],
			"mount_permissions": req.GetVolumeContext()["mount_permissions"],
		}
		d.log().Info("ControllerPublishVolume(): volume published",
			zap.String("volume_id", req.GetVolumeId()),
			zap.String("target_id", target.ID),
			zap.Int("lun_number", lunNum),
		)
		response = &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(volumeLock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release iSCSI publish volume lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release iSCSI publish volume lock", releaseErr)
	}
	return response, nil
}

// ControllerUnpublishVolume unmaps the LUN and removes its per-volume iSCSI
// target only after Kubernetes VolumeAttachment state proves this is the last
// publisher. Backend login/initiator timing is diagnostic only and is not used
// as the correctness source for shared-target lifetime.
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	d.log().Info("ControllerUnpublishVolume(): init")
	d.log().Debug("ControllerUnpublishVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("node_id", req.GetNodeId()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetVolumeId() == "" {
		d.log().Error("ControllerUnpublishVolume(): volume ID is required")
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// CSI idempotency: nothing of this driver's to unpublish.
			d.log().Info("ControllerUnpublishVolume(): volume not owned by this driver; idempotent OK", zap.String("volume_id", req.GetVolumeId()))
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, err
	}
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "iSCSI unpublish coordination is unavailable")
	}
	volumeLock, err := d.coordination.Acquire(ctx, "volume:"+req.GetVolumeId(), coord.AcquireOptions{
		Safety: coord.SafetyDestructive, Operation: coord.OperationDelete,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire iSCSI unpublish volume lock", err)
	}

	criticalErr := func() error {
		if err := volumeLock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify iSCSI unpublish lock ownership", err)
		}

		remaining, err := helper.RemainderNodeIDs(ctx, d.attachmentLookup, d.Name, req.GetVolumeId(), req.GetNodeId(), d.log())
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			d.log().Info("ControllerUnpublishVolume(): retaining shared target for remaining VolumeAttachments",
				zap.String("lun_id", lunID), zap.String("node_id", req.GetNodeId()),
				zap.Strings("remaining_nodes", remaining))
			return nil
		}

		target, found, err := d.reconciler().FindISCSITargetForVolume(ctx, req.GetVolumeId(), lunID)
		if err != nil {
			return status.Errorf(codes.Internal, "find iSCSI target for volume: %v", err)
		}
		if !found {
			if cleanupErr := d.reconciler().CleanupOrphanedVolumeScopedAuthGroup(ctx, volumeLock, "iSCSI unpublish lock", req.GetVolumeId()); cleanupErr != nil {
				return cleanupErr
			}
			d.log().Info("ControllerUnpublishVolume(): volume already unmapped", zap.String("lun_id", lunID))
			return nil
		}

		// Re-read VolumeAttachment state immediately before the destructive backend
		// mutation. A concurrent ControllerPublish is serialized by the same volume
		// Lease; a newly-created attachment will therefore either retain this target
		// here or recreate it through the blocked publish path after cleanup.
		remaining, err = helper.RemainderNodeIDs(ctx, d.attachmentLookup, d.Name, req.GetVolumeId(), req.GetNodeId(), d.log())
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return nil
		}
		if err := volumeLock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify iSCSI unpublish lock before backend mutation", err)
		}

		mapped := false
		for _, lun := range target.Luns {
			if lun.ID == lunID {
				mapped = true
				break
			}
		}
		if mapped {
			if err := d.reconciler().RemoveLUNReconciled(ctx, target.ID, lunID); err != nil {
				return status.Errorf(codes.Internal, "unmap LUN from iSCSI target: %v", err)
			}
			// A 200 unmap ack is not proof the mapping is gone. Wait for the
			// backend readback to converge before the destructive target
			// delete; a silent no-op unmap must fail closed here, never
			// strand a live mapping under a deleted target.
			if err := d.reconciler().WaitForISCSIUnmap(ctx, target.ID, lunID); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return status.FromContextError(ctxErr).Err()
				}
				return status.Errorf(codes.Internal, "verify iSCSI LUN unmap before target delete: %v", err)
			}
		}
		if err := d.client.ISCSITargets().Delete(ctx, target.ID); err != nil {
			return status.Errorf(codes.Internal, "delete iSCSI target: %v", err)
		}
		// Read-after-delete: a 200 target DELETE ack is not proof of backend
		// absence. Verify authoritative NotFound before claiming unpublish
		// success or cleaning up the auth group; a still-present target must
		// surface as an error so the caller retries.
		if err := d.reconciler().VerifyTargetDeleted(ctx, target.ID); err != nil {
			return err
		}

		// Only 11.1 volume-scoped auth groups are owned by this target lifecycle.
		// Legacy node-scoped groups may be shared by older targets, so never infer
		// ownership from the target reference alone.
		if target.AuthGroupID != "" {
			group, groupErr := d.client.AuthGroups().Get(ctx, target.AuthGroupID)
			if groupErr != nil && !ngxstorage.IsNotFound(groupErr) {
				return status.Errorf(codes.Internal, "get iSCSI auth group before cleanup: %v", groupErr)
			}
			targetName := ngxnames.ModifyVolName(req.GetVolumeId())
			if groupErr == nil && group != nil && group.Name == targetName {
				inUse, err := d.reconciler().AuthGroupReferencedByOtherTarget(ctx, target.AuthGroupID, target.ID)
				if err != nil {
					return status.Errorf(codes.Internal, "verify iSCSI auth group references: %v", err)
				}
				if !inUse {
					if err := volumeLock.AssertOwned(ctx); err != nil {
						return utils.CoordinationError("verify iSCSI unpublish lock before auth group cleanup", err)
					}
					if err := d.client.AuthGroups().Delete(ctx, target.AuthGroupID); err != nil {
						return status.Errorf(codes.Internal, "delete iSCSI auth group: %v", err)
					}
					// Read-after-delete: a 200 auth-group DELETE ack is not
					// proof of absence. A surviving group retains stale CHAP
					// credentials and IQNs, so unpublish success requires
					// authoritative NotFound.
					if err := d.reconciler().VerifyAuthGroupDeleted(ctx, target.AuthGroupID); err != nil {
						return err
					}
				}
			}
		}
		d.log().Info("ControllerUnpublishVolume(): volume unpublished",
			zap.String("lun_id", lunID), zap.String("target_id", target.ID))
		return nil
	}()

	releaseErr := utils.ReleaseOperationLock(volumeLock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release iSCSI unpublish volume lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release iSCSI unpublish volume lock", releaseErr)
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// GetSnapshot returns one snapshot by canonical ID. Context/input checks and
// gRPC result decisions live here; backend lookup/proto preparation is owned
// by the reconcile helper.
func (d *Driver) GetSnapshot(ctx context.Context, req *csi.GetSnapshotRequest) (*csi.GetSnapshotResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if req == nil || req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
	}
	snap, err := d.reconciler().SnapshotByID(ctx, req.GetSnapshotId())
	if err != nil {
		if ngxstorage.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "snapshot %s not found", req.GetSnapshotId())
		}
		return nil, utils.BackendStatusError("get snapshot", err)
	}
	if snap == nil {
		return nil, status.Errorf(codes.NotFound, "snapshot %s not found", req.GetSnapshotId())
	}
	return &csi.GetSnapshotResponse{Snapshot: snap}, nil
}

// CreateSnapshot provisions a snapshot of a LUN.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	d.log().Info("CreateSnapshot(): init")
	d.log().Debug("CreateSnapshot(): request",
		zap.String("name", req.GetName()),
		zap.String("source_volume_id", req.GetSourceVolumeId()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetSourceVolumeId() == "" || req.GetName() == "" {
		d.log().Error("CreateSnapshot(): source volume ID and name are required")
		return nil, status.Error(codes.InvalidArgument, "source volume ID and name are required")
	}
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetSourceVolumeId())
	if err != nil {
		return nil, err
	}
	name := ngxnames.ModifyVolName(req.GetName())
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "create snapshot coordination is unavailable")
	}
	lockSet, err := d.coordination.AcquireMany(ctx, []string{
		"snapshot-name:" + name,
		"volume:" + req.GetSourceVolumeId(),
	}, coord.AcquireOptions{Safety: coord.SafetyOrdinary, Operation: coord.OperationSnapshot})
	if err != nil {
		return nil, utils.CoordinationError("acquire create snapshot locks", err)
	}

	var response *csi.CreateSnapshotResponse
	criticalErr := func() error {
		if err := lockSet.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify create snapshot lock ownership", err)
		}
		lun, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("get source LUN", err)
		}
		if lun == nil || lun.ID != lunID {
			return status.Error(codes.NotFound, "source volume not found")
		}

		snaps, err := d.client.Snapshots().ListDetail(ctx, "")
		if err != nil {
			return status.Errorf(codes.Unavailable, "list snapshots for create idempotency: %v", err)
		}
		for i := range snaps {
			if snaps[i].Name != name {
				continue
			}
			if snaps[i].VolumeID != lunID {
				return status.Errorf(codes.AlreadyExists,
					"snapshot name %q already exists for source volume %q", name, snaps[i].VolumeID)
			}
			if snaps[i].ID == "" {
				return status.Errorf(codes.Unavailable, "same-name snapshot %q has an empty backend id", name)
			}
			readback, err := d.client.Snapshots().Get(ctx, snaps[i].ID)
			if err != nil {
				return utils.BackendStatusError("read back existing snapshot", err)
			}
			if readback == nil || readback.ID != snaps[i].ID || readback.Name != name || readback.VolumeID != lunID {
				return status.Error(codes.Unavailable, "same-name snapshot readback identity mismatch")
			}
			response = helper.CreateSnapshotResponse(volumeIDPrefix, readback, req.GetSourceVolumeId(), int64(lun.Size))
			d.log().Info("CreateSnapshot(): snapshot already exists (idempotent)",
				zap.String("snapshot_id", readback.ID), zap.String("name", name))
			return nil
		}

		if err := lockSet.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify create snapshot locks before backend mutation", err)
		}
		created, err := d.client.Snapshots().Create(ctx, lunID, name)
		if err != nil {
			return utils.BackendStatusError("create snapshot", err)
		}
		if created == nil || created.ID == "" {
			return status.Error(codes.Internal, "create snapshot: backend returned an invalid snapshot")
		}
		readback, err := d.client.Snapshots().Get(ctx, created.ID)
		if err != nil {
			return utils.BackendStatusError("read back created snapshot", err)
		}
		if readback == nil || readback.ID != created.ID || readback.Name != name || readback.VolumeID != lunID {
			return status.Errorf(codes.Internal,
				"created snapshot readback mismatch: id=%q name=%q source=%q expected_id=%q expected_name=%q expected_source=%q",
				readback.ID, readback.Name, readback.VolumeID, created.ID, name, lunID)
		}
		response = helper.CreateSnapshotResponse(volumeIDPrefix, readback, req.GetSourceVolumeId(), int64(lun.Size))
		d.log().Info("CreateSnapshot(): snapshot created", zap.String("snapshot_id", readback.ID))
		return nil
	}()
	releaseErr := utils.ReleaseOperationLockSet(lockSet)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release create snapshot locks", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release create snapshot locks", releaseErr)
	}
	return response, nil
}

// DeleteSnapshot deletes a snapshot. Idempotent.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	d.log().Info("DeleteSnapshot(): init")
	d.log().Debug("DeleteSnapshot(): request",
		zap.String("snapshot_id", req.GetSnapshotId()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetSnapshotId() == "" {
		d.log().Error("DeleteSnapshot(): snapshot ID is required")
		return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
	}
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "delete snapshot coordination is unavailable")
	}
	lock, err := d.coordination.Acquire(ctx, "snapshot:"+req.GetSnapshotId(), coord.AcquireOptions{
		Safety: coord.SafetyDestructive, Operation: coord.OperationSnapshot,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire delete snapshot lock", err)
	}
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify delete snapshot lock ownership", err)
		}
		if _, err := d.client.Snapshots().Get(ctx, req.GetSnapshotId()); err != nil {
			if ngxstorage.IsNotFound(err) {
				return nil
			}
			return utils.BackendStatusError("get snapshot before delete", err)
		}
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify delete snapshot lock before backend mutation", err)
		}
		if err := d.client.Snapshots().Delete(ctx, req.GetSnapshotId()); err != nil {
			return utils.BackendStatusError("delete snapshot", err)
		}
		// Read-after-delete: a 200-success delete response is not proof of
		// backend absence. Verify authoritative NotFound before claiming CSI
		// success; a still-present snapshot must surface as an error so the
		// caller retries instead of attesting a deletion that never happened.
		// An in-use snapshot (clone dependency) is rejected by the delete
		// itself and mapped to FailedPrecondition before this point.
		if err := d.reconciler().VerifySnapshotDeleted(ctx, req.GetSnapshotId()); err != nil {
			return err
		}
		d.log().Info("DeleteSnapshot(): snapshot deleted (backend absence verified)", zap.String("snapshot_id", req.GetSnapshotId()))
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release delete snapshot lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release delete snapshot lock", releaseErr)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots lists snapshots. A nil response lists all snapshots; a
// snapshot-ID response lists a single snapshot.
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	d.log().Info("ListSnapshots(): init")
	d.log().Debug("ListSnapshots(): request",
		zap.String("snapshot_id", req.GetSnapshotId()),
		zap.String("source_volume_id", req.GetSourceVolumeId()),
		zap.Int32("max_entries", req.GetMaxEntries()),
	)

	if req.GetMaxEntries() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_entries must not be negative")
	}
	if req.GetSnapshotId() != "" {
		snap, err := d.client.Snapshots().Get(ctx, req.GetSnapshotId())
		if err != nil {
			if ngxstorage.IsNotFound(err) {
				return &csi.ListSnapshotsResponse{}, nil
			}
			d.log().Error("ListSnapshots(): snapshot get failed", zap.Error(err))
			return nil, utils.BackendStatusError("get snapshot", err)
		}
		if snap == nil || snap.ID == "" {
			return &csi.ListSnapshotsResponse{}, nil
		}
		return &csi.ListSnapshotsResponse{
			Entries: []*csi.ListSnapshotsResponse_Entry{{
				Snapshot: helper.SnapshotProto(volumeIDPrefix, snap),
			}},
		}, nil
	}
	// The detail endpoint carries created/reserved; the summary endpoint omits
	// them, which would leave every snapshot with an empty creation time.
	snaps, err := d.client.Snapshots().ListDetail(ctx, "")
	if err != nil {
		d.log().Error("ListSnapshots(): snapshot list failed", zap.Error(err))
		return nil, utils.BackendStatusError("list snapshots", err)
	}
	// CSI requires source filtering and opaque pagination. Tokens are stable
	// canonical snapshot IDs prefixed with s1: so concurrent creates/deletes
	// between pages cannot duplicate or omit entries (an index token shifts
	// with every mutation).
	if sourceID := req.GetSourceVolumeId(); sourceID != "" {
		backendID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, sourceID)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				// CSI: a source volume that is not this driver's has no snapshots.
				return &csi.ListSnapshotsResponse{}, nil
			}
			return nil, err
		}
		filtered := snaps[:0]
		for _, snap := range snaps {
			if snap.VolumeID == backendID {
				filtered = append(filtered, snap)
			}
		}
		snaps = filtered
	}
	entries := make([]*csi.ListSnapshotsResponse_Entry, 0, len(snaps))
	for i := range snaps {
		if snaps[i].ID == "" {
			d.log().Error("ListSnapshots(): backend record is missing id, skipping")
			continue
		}
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{Snapshot: helper.SnapshotProto(volumeIDPrefix, &snaps[i])})
	}
	// Sort by canonical snapshot ID so cursor tokens remain deterministic.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].GetSnapshot().GetSnapshotId() < entries[j].GetSnapshot().GetSnapshotId()
	})
	start := 0
	const tokenPrefix = "s1:"
	if token := req.GetStartingToken(); token != "" {
		if !strings.HasPrefix(token, tokenPrefix) || len(token) == len(tokenPrefix) {
			return nil, status.Error(codes.Aborted, "invalid starting token")
		}
		cursor := strings.TrimPrefix(token, tokenPrefix)
		for start < len(entries) && entries[start].GetSnapshot().GetSnapshotId() <= cursor {
			start++
		}
	}
	end := len(entries)
	if max := req.GetMaxEntries(); max > 0 && int64(start)+int64(max) < int64(end) {
		end = start + int(max)
	}
	d.log().Info("ListSnapshots(): snapshots listed", zap.Int("count", end-start))
	response := &csi.ListSnapshotsResponse{Entries: entries[start:end]}
	if end < len(entries) {
		response.NextToken = tokenPrefix + entries[end-1].GetSnapshot().GetSnapshotId()
	}
	return response, nil
}

// ControllerGetVolume returns the current canonical LUN identity and size.
func (d *Driver) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if req == nil || req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	backendID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	lun, err := d.client.LUNs().Get(ctx, backendID)
	if err != nil {
		return nil, utils.BackendStatusError("get volume", err)
	}
	if lun == nil || lun.ID == "" || lun.ID != backendID {
		return nil, status.Error(codes.Internal, "get volume: backend response has an invalid id")
	}
	return &csi.ControllerGetVolumeResponse{
		Volume: &csi.Volume{VolumeId: helper.VolumeID(volumeIDPrefix, lun.ID), CapacityBytes: int64(lun.Size)},
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{},
	}, nil
}

// ControllerExpandVolume grows a LUN. The node performs the filesystem resize.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	d.log().Info("ControllerExpandVolume(): init")
	d.log().Debug("ControllerExpandVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.Reflect("capacity_range", req.GetCapacityRange()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetVolumeId() == "" || req.GetCapacityRange() == nil {
		d.log().Error("ControllerExpandVolume(): volume ID and capacity range are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and capacity range are required")
	}
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	newBytes := req.GetCapacityRange().GetRequiredBytes()
	if newBytes <= 0 {
		d.log().Error("ControllerExpandVolume(): required capacity must be positive")
		return nil, status.Error(codes.InvalidArgument, "required capacity must be positive")
	}
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "controller expand coordination is unavailable")
	}
	lock, err := d.coordination.Acquire(ctx, "volume:"+req.GetVolumeId(), coord.AcquireOptions{
		Safety:    coord.SafetyOrdinary,
		Operation: coord.OperationExpand,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire controller expand lock", err)
	}
	var response *csi.ControllerExpandVolumeResponse
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify controller expand lock ownership", err)
		}
		lun, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("get LUN before expand", err)
		}
		if lun == nil || lun.ID != lunID {
			return status.Error(codes.NotFound, "volume not found")
		}
		if lun.Size <= 0 {
			return status.Error(codes.Internal, "get LUN before expand: backend response has invalid size")
		}
		const gib = int64(1 << 30)
		targetGB := (newBytes + gib - 1) / gib
		currentBytes := int64(lun.Size)
		// Shrink is rejected at byte granularity: the whole-GiB idempotency
		// check below would otherwise accept a smaller request that rounds
		// to the same GiB as a no-op success.
		if newBytes < currentBytes {
			d.log().Warn("ControllerExpandVolume(): volume shrink is not supported",
				zap.String("lun_id", lunID),
				zap.Int64("current_bytes", currentBytes),
				zap.Int64("requested_bytes", newBytes),
			)
			return status.Error(codes.InvalidArgument, "requested capacity must not shrink the volume")
		}
		currentGB := (currentBytes + gib - 1) / gib
		extendGB := targetGB - currentGB
		if extendGB <= 0 {
			d.log().Info("ControllerExpandVolume(): LUN already satisfies requested capacity",
				zap.String("lun_id", lunID),
				zap.Int64("current_bytes", currentBytes),
				zap.Int64("requested_bytes", newBytes),
			)
			response = &csi.ControllerExpandVolumeResponse{
				CapacityBytes:         currentBytes,
				NodeExpansionRequired: true,
			}
			return nil
		}
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify controller expand lock before backend mutation", err)
		}
		if err := d.client.LUNs().Expand(ctx, lunID, extendGB); err != nil {
			return status.Errorf(codes.Internal, "expand LUN: %v", err)
		}
		finalLUN, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("get LUN after expand", err)
		}
		finalBytes := int64(finalLUN.Size)
		finalGB := (finalBytes + gib - 1) / gib
		if finalBytes < newBytes || finalGB != targetGB {
			return status.Errorf(codes.Internal,
				"expand LUN readback mismatch: requested=%d target_gb=%d final_bytes=%d final_gb=%d",
				newBytes, targetGB, finalBytes, finalGB)
		}
		d.log().Info("ControllerExpandVolume(): LUN expanded",
			zap.String("lun_id", lunID),
			zap.Int64("current_gb", currentGB),
			zap.Int64("target_gb", targetGB),
			zap.Int64("extend_gb", extendGB),
			zap.Int64("final_bytes", finalBytes),
		)
		response = &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         finalBytes,
			NodeExpansionRequired: true,
		}
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release controller expand lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release controller expand lock", releaseErr)
	}
	return response, nil
}

// ListVolumes lists all provisioned LUNs. Tokens are stable canonical volume
// IDs prefixed with v1: so concurrent creates/deletes between pages cannot
// duplicate or omit entries (an index token shifts with every mutation).
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	d.log().Info("ListVolumes(): init")
	if req.GetMaxEntries() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_entries must not be negative")
	}
	luns, err := d.client.LUNs().List(ctx)
	if err != nil {
		d.log().Error("ListVolumes(): LUN list failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "list LUNs: %v", err)
	}
	entries := make([]*csi.ListVolumesResponse_Entry, 0, len(luns))
	for i := range luns {
		if luns[i].ID == "" {
			d.log().Error("ListVolumes(): backend record is missing id, skipping")
			continue
		}
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{VolumeId: helper.VolumeID(volumeIDPrefix, luns[i].ID), CapacityBytes: int64(luns[i].Size)},
		})
	}
	// Sort by stable canonical ID so cursor tokens remain deterministic.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].GetVolume().GetVolumeId() < entries[j].GetVolume().GetVolumeId()
	})
	start := 0
	const tokenPrefix = "v1:"
	if token := req.GetStartingToken(); token != "" {
		if !strings.HasPrefix(token, tokenPrefix) || len(token) == len(tokenPrefix) {
			return nil, status.Error(codes.Aborted, "invalid starting token")
		}
		cursor := strings.TrimPrefix(token, tokenPrefix)
		for start < len(entries) && entries[start].GetVolume().GetVolumeId() <= cursor {
			start++
		}
	}
	end := len(entries)
	if max := req.GetMaxEntries(); max > 0 && int64(start)+int64(max) < int64(end) {
		end = start + int(max)
	}
	d.log().Info("ListVolumes(): volumes listed", zap.Int("count", end-start))
	response := &csi.ListVolumesResponse{Entries: entries[start:end]}
	if end < len(entries) {
		response.NextToken = tokenPrefix + entries[end-1].GetVolume().GetVolumeId()
	}
	return response, nil
}

// GetCapacity returns the configured pool's available capacity in bytes.
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	d.log().Info("GetCapacity(): init")
	avail, _, err := d.client.Pools().GetConfiguredCapacity(ctx)
	if err != nil {
		d.log().Error("GetCapacity(): capacity lookup failed",
			zap.String("pool_name", d.client.PoolName()),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Unavailable,
			"configured pool %q capacity lookup failed: %v", d.client.PoolName(), err)
	}
	d.log().Info("GetCapacity(): capacity returned", zap.Int64("available_bytes", avail))
	return &csi.GetCapacityResponse{AvailableCapacity: avail}, nil
}

// ValidateVolumeCapabilities validates a requested capability against the
// driver's advertised access modes and mount/block types.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	d.log().Info("ValidateVolumeCapabilities(): init")
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if req.GetVolumeId() == "" {
		d.log().Error("ValidateVolumeCapabilities(): volume ID is required")
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		d.log().Error("ValidateVolumeCapabilities(): at least one capability is required")
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	// The volume must exist; an empty Confirmed alone would confirm
	// capabilities for a volume the backend does not have.
	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	lun, err := d.client.LUNs().Get(ctx, lunID)
	if err != nil {
		return nil, utils.BackendStatusError("lookup volume", err)
	}
	if lun == nil || lun.ID == "" {
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	for _, capability := range req.GetVolumeCapabilities() {
		if err := utils.ValidateVolumeCapability(capability, d.AccessModes); err != nil {
			d.log().Info("ValidateVolumeCapabilities(): capability not confirmed", zap.Error(err))
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}
	d.log().Info("ValidateVolumeCapabilities(): capabilities confirmed", zap.String("volume_id", req.GetVolumeId()))
	response := &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}
	if d.poolOwner != "" {
		response.Confirmed.VolumeContext = map[string]string{"owner": d.poolOwner}
	}
	return response, nil
}

// ControllerModifyVolume applies one explicit set of supported mutable NGX
// LUN attributes. It is the CSI VolumeAttributesClass update path; omitted
// fields are never defaulted or changed. Immutable create parameters such as
// blocksize are rejected.
//
// Backend:
//   - Reads GET /api/v2/lun/{id} to resolve the canonical LUN.
//   - Writes PATCH /api/v2/lun/{id} with only the validated mutable fields.
//   - Accepts deduplication, compression, thin_provision, flash_cache,
//     dram_cache, io_type, and qos_priority.
func (d *Driver) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	d.log().Info("ControllerModifyVolume(): init")
	if err := ctx.Err(); err != nil {
		d.log().Error("ControllerModifyVolume(): context ended", zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	if req == nil || req.GetVolumeId() == "" {
		d.log().Error("ControllerModifyVolume(): volume ID is required")
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	mutable, err := helper.NormalizeLUNMutableParameters(req.GetMutableParameters())
	if err != nil {
		d.log().Error("ControllerModifyVolume(): mutable parameters rejected",
			zap.String("volume_id", req.GetVolumeId()),
			zap.Error(err),
		)
		return nil, err
	}

	lunID, err := helper.LUNIDFromVolumeIDChecked(volumeIDPrefix, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if d.coordination == nil {
		return nil, status.Error(codes.Unavailable, "controller modify coordination is unavailable")
	}
	lock, err := d.coordination.Acquire(ctx, "volume:"+req.GetVolumeId(), coord.AcquireOptions{
		Safety: coord.SafetyOrdinary, Operation: coord.OperationModify,
	})
	if err != nil {
		return nil, utils.CoordinationError("acquire controller modify lock", err)
	}
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify controller modify lock ownership", err)
		}
		lun, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("lookup volume", err)
		}
		if lun == nil || lun.ID == "" || lun.ID != lunID {
			return status.Error(codes.NotFound, "volume not found")
		}
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify controller modify lock before backend mutation", err)
		}
		if err := d.client.LUNs().Modify(ctx, lunID, mutable); err != nil {
			return utils.BackendStatusError("modify LUN", err)
		}
		readback, err := d.client.LUNs().Get(ctx, lunID)
		if err != nil {
			return utils.BackendStatusError("read back modified LUN", err)
		}
		if readback == nil || readback.ID != lunID {
			return status.Error(codes.Internal, "modified LUN readback has an invalid id")
		}
		if err := helper.VerifyLUNMutableReadback(readback, mutable); err != nil {
			return err
		}
		d.log().Info("ControllerModifyVolume(): volume modified", zap.String("lun_id", lunID))
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(criticalErr, utils.CoordinationError("release controller modify lock", releaseErr))
		}
		return nil, criticalErr
	}
	if releaseErr != nil {
		return nil, utils.CoordinationError("release controller modify lock", releaseErr)
	}
	return &csi.ControllerModifyVolumeResponse{}, nil
}

// ControllerGetCapabilities returns the controller service capabilities
// configured during NewDriver. Sidecar containers (provisioner, attacher,
// resizer) call this on startup to discover the driver's feature set.
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	d.log().Info("ControllerGetCapabilities(): init")
	d.log().Debug("ControllerGetCapabilities(): request",
		zap.Bool("request_present", req != nil),
	)
	if err := ctx.Err(); err != nil {
		d.log().Error("ControllerGetCapabilities(): context ended", zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	d.log().Info("ControllerGetCapabilities(): capabilities returned",
		zap.Reflect("capabilities", d.csCap),
	)
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: d.csCap}, nil
}

// LUN/target reconciliation (relocated from reconcile.go).
// The NGX management API can drop a connection while a mutation is in flight
// (observed live as `EOF` on PATCH/POST calls during management-service
// restarts). A dropped response leaves the outcome ambiguous: the mutation may
// or may not have been applied. The SDK only replays idempotent HTTP methods
// automatically, so the driver reconciles the few name- or membership-keyed
// mutations it performs: after a transport failure it re-reads the backend
// state and only retries when the mutation is still absent.
