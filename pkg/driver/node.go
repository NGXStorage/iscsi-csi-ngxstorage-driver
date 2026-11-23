package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	blockmount "github.com/NGXStorage/csi-ngxstorage-utils/blockmount"
	helper "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/helper"
	"github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/iscsi"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	"golang.org/x/sys/unix"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
)

// newISCSIConnector builds the host connector for node RPCs. Tests override
// it to inject a fake iscsiadm/device environment.
var newISCSIConnector = iscsi.NewConnector

// deviceStageError maps a connector failure to the CSI status its cause
// supports. Context end propagates unchanged; a permanent login/auth
// rejection is FailedPrecondition (credentials or initiator registration
// must change first), malformed input is InvalidArgument, a genuine
// bounded-wait expiry is DeadlineExceeded, and every other host/fabric
// failure is Unavailable so the CO retries a transient condition instead of
// seeing a fake timeout.
func deviceStageError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	switch iscsi.ClassifyDeviceError(err) {
	case iscsi.DeviceCauseLoginRejected:
		return status.Errorf(codes.FailedPrecondition, "%s: %v", operation, err)
	case iscsi.DeviceCauseInvalidRequest:
		return status.Errorf(codes.InvalidArgument, "%s: %v", operation, err)
	case iscsi.DeviceCauseWaitTimeout:
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", operation, err)
	default:
		return status.Errorf(codes.Unavailable, "%s: %v", operation, err)
	}
}

// NodeGetInfo returns the node ID. The driver does not advertise topology.
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	d.log().Info("NodeGetInfo(): init")
	return &csi.NodeGetInfoResponse{NodeId: d.NodeId}, nil
}

// NodeGetCapabilities returns the advertised node capabilities.
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	d.log().Info("NodeGetCapabilities(): init")
	return &csi.NodeGetCapabilitiesResponse{Capabilities: d.nsCap}, nil
}

// NodeStageVolume attaches the iSCSI device, formats it (filesystem volumes),
// and mounts it to the staging path.
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	d.log().Info("NodeStageVolume(): init")
	d.log().Debug("NodeStageVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("staging_target_path", req.GetStagingTargetPath()),
		zap.Reflect("volume_capability", req.GetVolumeCapability()),
		zap.Reflect("publish_context", req.GetPublishContext()),
		zap.Bool("has_secrets", len(req.GetSecrets()) != 0),
	)

	if req.GetStagingTargetPath() == "" || req.GetVolumeId() == "" {
		d.log().Error("NodeStageVolume(): volume ID and staging path are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging path are required")
	}
	volumeLock, err := d.nodeState.LockVolume(ctx, req.GetVolumeId())
	if err != nil {
		d.log().Error("NodeStageVolume(): context ended while waiting for volume lock",
			zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	defer volumeLock.Unlock()
	publishContext := req.GetPublishContext()
	targetIQN := publishContext["target_iqn"]
	portals := publishContext["portals"]
	scsiID := strings.TrimSpace(publishContext["scsi_id"])
	fstype := publishContext["fstype"]
	lunNum, err := strconv.Atoi(strings.TrimSpace(publishContext["lun_number"]))
	if err != nil || lunNum < 0 {
		d.log().Error("NodeStageVolume(): publish context lun_number is not numeric",
			zap.String("volume_id", req.GetVolumeId()),
			zap.String("lun_number", publishContext["lun_number"]),
		)
		return nil, status.Error(codes.InvalidArgument, "publish context lun_number must be numeric")
	}
	if targetIQN == "" || portals == "" {
		d.log().Error("NodeStageVolume(): publish context missing target_iqn or portals")
		return nil, status.Error(codes.InvalidArgument, "publish context missing target_iqn or portals")
	}
	if scsiID == "" {
		d.log().Error("NodeStageVolume(): publish context missing scsi_id")
		return nil, status.Error(codes.InvalidArgument, "publish context missing scsi_id")
	}
	authGroupID := strings.TrimSpace(publishContext["auth_group"])
	if authGroupID == "" {
		d.log().Error("NodeStageVolume(): publish context missing auth_group")
		return nil, status.Error(codes.InvalidArgument, "publish context missing auth_group")
	}

	// Log in to every advertised portal and discover either the single SCSI
	// device or the stable multipath map (bounded).
	connector := newISCSIConnector()
	portalList := strings.Split(portals, ",")

	// Register the node's initiator IQN on the auth group before login: the
	// backend authorizes iSCSI logins only for registered IQNs (login error
	// 24). The auth group ID is carried in the publish context from
	// ControllerPublishVolume. An already-registered IQN is treated as success.
	iqn, err := connector.DiscoverIQN()
	if err != nil {
		d.log().Error("NodeStageVolume(): initiator IQN discovery failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "discover initiator IQN: %v", err)
	}
	alias := iscsi.IQNAlias(d.NodeId)
	if err := d.initiatorRegistrar().Register(ctx, req.GetVolumeId(), authGroupID, iqn, alias); err != nil {
		d.log().Error("NodeStageVolume(): initiator IQN registration failed",
			zap.String("iqn", iqn),
			zap.String("auth_group", authGroupID),
			zap.Error(err),
		)
		return nil, err
	}
	d.log().Info("NodeStageVolume(): initiator IQN registered",
		zap.String("iqn", iqn),
		zap.String("auth_group", authGroupID),
	)

	// Persist the session state BEFORE Connect (pending state). If Connect
	// fails partway, or WaitForMappedDevice fails after a successful Connect,
	// NodeUnstageVolume can still find the portals/IQN and disconnect: the
	// state file is the per-volume cleanup responsibility record. Connect
	// rolls back only the sessions it newly created; whatever it cannot roll
	// back stays recoverable through this state (never counted as success).
	if err := helper.SaveISCSISessionState(d.sessionStateDir, req.GetVolumeId(), helper.ISCSISessionState{
		TargetIQN: targetIQN,
		Portals:   portalList,
		SCSIID:    scsiID,
	}); err != nil {
		d.log().Error("NodeStageVolume(): session state persistence failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "persist iSCSI session state: %v", err)
	}
	_, err = connector.Connect(ctx, portalList, targetIQN, lunNum, scsiID)
	if err != nil {
		d.log().Error("NodeStageVolume(): device discovery failed",
			zap.String("target_iqn", targetIQN),
			zap.Error(err),
		)
		return nil, deviceStageError("connect iSCSI target", err)
	}
	devicePath, err := connector.WaitForMappedDevice(ctx, portalList, targetIQN, lunNum, scsiID)
	if err != nil {
		d.log().Error("NodeStageVolume(): iSCSI device identity/convergence failed",
			zap.String("target_iqn", targetIQN),
			zap.String("scsi_id", scsiID),
			zap.Error(err),
		)
		return nil, deviceStageError("wait for mapped iSCSI device", err)
	}
	d.log().Info("NodeStageVolume(): iSCSI device discovered", zap.String("device", devicePath))

	// Determine block vs mount access.
	if utils.IsBlockVolume(req.GetVolumeCapability()) {
		if err := blockmount.PrepareBlockTarget(req.GetStagingTargetPath()); err != nil {
			d.log().Error("NodeStageVolume(): block staging target creation failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "create block staging target: %v", err)
		}
		// CSI idempotency: a repeat stage of an already-staged block volume is
		// a success (the second bind mount would fail EBUSY). The existing
		// bind must expose the expected device node; an unverified or
		// mismatched target fails closed.
		if alreadyStaged, err := blockmount.BlockStagingMatchesDevice(blockmount.Mounter, req.GetStagingTargetPath(), devicePath); err != nil {
			d.log().Error("NodeStageVolume(): block staging mount verification failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "verify block staging mount: %v", err)
		} else if alreadyStaged {
			d.log().Info("NodeStageVolume(): block volume already staged",
				zap.String("staging_path", req.GetStagingTargetPath()))
			return &csi.NodeStageVolumeResponse{}, nil
		}
		// Raw block: bind-mount the device to the staging path.
		if err := blockmount.MountVolume(devicePath, req.GetStagingTargetPath(), "", false, nil); err != nil {
			d.log().Error("NodeStageVolume(): block mount failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "mount block device: %v", err)
		}
		d.log().Info("NodeStageVolume(): block volume staged", zap.String("device", devicePath))
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Filesystem: shared/destructive initialization is coordinated across nodes;
	// the final staging mount happens only after initialization is complete.
	if err := os.MkdirAll(req.GetStagingTargetPath(), 0o750); err != nil {
		d.log().Error("NodeStageVolume(): staging dir creation failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "create staging dir: %v", err)
	}
	if fstype == "" {
		fstype = "ext4"
	}
	revalidate := func(revalidateCtx context.Context) (string, error) {
		resolved, err := connector.WaitForMappedDevice(revalidateCtx, portalList, targetIQN, lunNum, scsiID)
		if err != nil {
			return "", fmt.Errorf("revalidate iSCSI mapped device: %w", err)
		}
		return resolved, nil
	}
	if err := d.filesystemStager().Stage(ctx, req.GetVolumeId(), devicePath, req.GetStagingTargetPath(), fstype,
		req.GetVolumeCapability(), utils.RequestedMountPermissions(publishContext), revalidate); err != nil {
		d.log().Error("NodeStageVolume(): filesystem staging failed",
			zap.String("fstype", fstype), zap.Error(err))
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "stage filesystem volume: %v", err)
	}
	if !utils.ReaderOnlyCapability(req.GetVolumeCapability()) {
		if err := d.filesystemStager().ResizeCoordinated(ctx, req.GetVolumeId(), devicePath, req.GetStagingTargetPath()); err != nil {
			d.log().Error("NodeStageVolume(): filesystem growth failed", zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
			if status.Code(err) != codes.Unknown {
				return nil, err
			}
			return nil, status.Errorf(codes.Internal, "grow staged filesystem: %v", err)
		}
	}
	d.log().Info("NodeStageVolume(): volume staged",
		zap.String("device", devicePath),
		zap.String("fstype", fstype),
	)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the staging path and removes the iSCSI device.
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	d.log().Info("NodeUnstageVolume(): init")
	d.log().Debug("NodeUnstageVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("staging_target_path", req.GetStagingTargetPath()),
	)

	if req.GetStagingTargetPath() == "" || req.GetVolumeId() == "" {
		d.log().Error("NodeUnstageVolume(): volume ID and staging path are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging path are required")
	}
	volumeLock, err := d.nodeState.LockVolume(ctx, req.GetVolumeId())
	if err != nil {
		d.log().Error("NodeUnstageVolume(): context ended while waiting for volume lock",
			zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	defer volumeLock.Unlock()
	if err := blockmount.UnmountVolume(req.GetStagingTargetPath()); err != nil {
		d.log().Error("NodeUnstageVolume(): unmount failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "unmount staging path: %v", err)
	}
	state, err := helper.LoadISCSISessionState(d.sessionStateDir, req.GetVolumeId())
	if err != nil && !os.IsNotExist(err) {
		d.log().Error("NodeUnstageVolume(): session state lookup failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "load iSCSI session state: %v", err)
	}
	if err == nil {
		connector := newISCSIConnector()
		if err := connector.Disconnect(ctx, state.Portals, state.TargetIQN, state.SCSIID); err != nil {
			d.log().Error("NodeUnstageVolume(): iSCSI disconnect failed", zap.Error(err))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, status.FromContextError(err).Err()
			}
			return nil, status.Errorf(codes.Internal, "disconnect iSCSI sessions: %v", err)
		}
		if err := helper.DeleteISCSISessionState(d.sessionStateDir, req.GetVolumeId()); err != nil {
			d.log().Error("NodeUnstageVolume(): session state removal failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "remove iSCSI session state: %v", err)
		}
	}
	d.log().Info("NodeUnstageVolume(): volume unstaged", zap.String("staging_path", req.GetStagingTargetPath()))
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the staged volume to the target path.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	d.log().Info("NodePublishVolume(): init")
	d.log().Debug("NodePublishVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("staging_target_path", req.GetStagingTargetPath()),
		zap.String("target_path", req.GetTargetPath()),
		zap.Bool("readonly", req.GetReadonly()),
	)

	if req.GetTargetPath() == "" || req.GetStagingTargetPath() == "" || req.GetVolumeId() == "" {
		d.log().Error("NodePublishVolume(): volume ID, staging, and target paths are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging, and target paths are required")
	}
	volumeLock, err := d.nodeState.LockVolume(ctx, req.GetVolumeId())
	if err != nil {
		d.log().Error("NodePublishVolume(): context ended while waiting for volume lock",
			zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	defer volumeLock.Unlock()

	if utils.IsBlockVolume(req.GetVolumeCapability()) {
		if err := blockmount.PrepareBlockTarget(req.GetTargetPath()); err != nil {
			d.log().Error("NodePublishVolume(): block target creation failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "create block target: %v", err)
		}
	} else if err := os.MkdirAll(req.GetTargetPath(), 0o750); err != nil {
		d.log().Error("NodePublishVolume(): target dir creation failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "create target dir: %v", err)
	}

	// Reader-only access modes are definitionally read-only and their staging
	// mount is ro: the CO readonly flag is not authoritative for them.
	// A bind of an ro superblock is ro-effective either way and chmod on it
	// can never succeed, so reader-only publishes are enforced ro with no
	// permission rewrite (fresh filesystems received permissions at format
	// time).
	publishReadOnly := req.GetReadonly() || utils.ReaderOnlyCapability(req.GetVolumeCapability())

	// CSI contract: NodePublishVolume is idempotent. The kubelet retries
	// publish on a target that is already bind-mounted; returning an error
	// would loop forever on a volume that is already usable. Idempotency
	// requires compatible state, not merely an existing mount: the target
	// must expose the staged volume with a compatible read-only state, and
	// a first publish must never bind an unmounted staging directory (an
	// empty-dir fake success after lost staging).
	if d.mounter != nil {
		matched, err := blockmount.PublishTargetMatchesStaging(d.mounter, req.GetTargetPath(), req.GetStagingTargetPath(),
			utils.IsBlockVolume(req.GetVolumeCapability()), publishReadOnly)
		if err != nil {
			d.log().Error("NodePublishVolume(): published target verification failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "verify published target: %v", err)
		}
		if matched {
			d.log().Info("NodePublishVolume(): target already published (identity verified)",
				zap.String("target_path", req.GetTargetPath()))
			return &csi.NodePublishVolumeResponse{}, nil
		}
		if err := blockmount.StagingReadyForPublish(d.mounter, req.GetStagingTargetPath(), utils.IsBlockVolume(req.GetVolumeCapability())); err != nil {
			d.log().Error("NodePublishVolume(): staging not ready for publish", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "staging not ready for publish: %v", err)
		}
	}

	// Bind-mount staging → target.
	mountFlags := []string{"bind"}
	if publishReadOnly {
		mountFlags = append(mountFlags, "ro")
	}
	if err := d.bindMount(req.GetStagingTargetPath(), req.GetTargetPath(), "", mountFlags); err != nil {
		d.log().Error("NodePublishVolume(): bind mount failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "bind mount volume: %v", err)
	}
	// A readonly bind mount roots a read-only superblock; chmod on the mount
	// root fails with EROFS. Permissions were applied to the staging mount
	// in NodeStageVolume, so skip them for readonly publishes.
	if !publishReadOnly {
		if err := d.chmodForTest(req.GetTargetPath(), utils.RequestedMountPermissions(req.GetPublishContext())); err != nil {
			return nil, status.Errorf(codes.Internal, "set published volume permissions: %v", err)
		}
	} else if !req.GetReadonly() {
		d.log().Info("NodePublishVolume(): enforced read-only publish for reader-only access mode",
			zap.String("target_path", req.GetTargetPath()))
	}
	d.log().Info("NodePublishVolume(): volume published", zap.String("target_path", req.GetTargetPath()))
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) chmodForTest(path string, mode os.FileMode) error {
	if d.nodeChmod != nil {
		return d.nodeChmod(path, mode)
	}
	return os.Chmod(path, mode)
}

// NodeUnpublishVolume unmounts the target path.
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	d.log().Info("NodeUnpublishVolume(): init")
	d.log().Debug("NodeUnpublishVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("target_path", req.GetTargetPath()),
	)

	if req.GetTargetPath() == "" || req.GetVolumeId() == "" {
		d.log().Error("NodeUnpublishVolume(): volume ID and target path are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	volumeLock, err := d.nodeState.LockVolume(ctx, req.GetVolumeId())
	if err != nil {
		d.log().Error("NodeUnpublishVolume(): context ended while waiting for volume lock",
			zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	defer volumeLock.Unlock()
	if err := blockmount.UnmountVolume(req.GetTargetPath()); err != nil {
		d.log().Error("NodeUnpublishVolume(): unmount failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "unmount target path: %v", err)
	}
	// CSI requires the SP to delete the file or directory it created at the
	// target path. The unmount above left an empty directory (or block file).
	if err := os.Remove(req.GetTargetPath()); err != nil && !os.IsNotExist(err) {
		d.log().Error("NodeUnpublishVolume(): target path removal failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "remove target path: %v", err)
	}
	d.log().Info("NodeUnpublishVolume(): volume unpublished", zap.String("target_path", req.GetTargetPath()))
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume resizes the filesystem after a controller-side LUN expand.
func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	d.log().Info("NodeExpandVolume(): init")
	d.log().Debug("NodeExpandVolume(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("volume_path", req.GetVolumePath()),
	)

	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		d.log().Error("NodeExpandVolume(): volume ID and volume path are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and volume path are required")
	}
	volumeLock, err := d.nodeState.LockVolume(ctx, req.GetVolumeId())
	if err != nil {
		d.log().Error("NodeExpandVolume(): context ended while waiting for volume lock",
			zap.String("volume_id", req.GetVolumeId()), zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}
	defer volumeLock.Unlock()
	// A volume that was never staged on this node does not exist here: report
	// NotFound before validating the resize request or touching the fabric.
	if _, err := helper.LoadISCSISessionState(d.sessionStateDir, req.GetVolumeId()); err != nil {
		if os.IsNotExist(err) {
			d.log().Error("NodeExpandVolume(): volume was not found",
				zap.String("volume_id", req.GetVolumeId()))
			return nil, status.Errorf(codes.NotFound, "volume %s was not found", req.GetVolumeId())
		}
		d.log().Error("NodeExpandVolume(): iSCSI state lookup failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "load iSCSI session state: %v", err)
	}
	if req.GetCapacityRange() == nil || req.GetCapacityRange().GetRequiredBytes() <= 0 {
		d.log().Error("NodeExpandVolume(): required capacity must be positive")
		return nil, status.Error(codes.InvalidArgument, "required capacity must be positive")
	}
	// A block-mode volume path is the empty regular file kubelet bind-mounts
	// to the device in the kubelet's mount namespace; the driver container
	// only sees the unbound file, so no device can be resolved from it. The
	// session rescan below refreshes the kernel paths after the backend
	// expand, and the bind mount then exposes the new size automatically.
	volumePath := req.GetVolumePath()
	blockMode := false
	if info, err := os.Stat(volumePath); err == nil && !info.IsDir() {
		blockMode = true
	}
	if !blockMode {
		// Resolve the real block device from the mount table, then resize the
		// filesystem on that device (mount path alone is not a device).
		devicePath, _, err := mount.GetDeviceNameFromMount(d.mounter, volumePath)
		if err != nil {
			d.log().Error("NodeExpandVolume(): device resolution failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "resolve device: %v", err)
		}
		volumePath = devicePath
	}
	connector := newISCSIConnector()
	if blockMode {
		// A block-mode volume path is the empty regular file kubelet bind-mounts
		// to the device in the kubelet's mount namespace; the driver container
		// only sees the unbound file, so the session rescan below is the only
		// way to refresh the kernel path. The fs path verifies the size; for
		// block there is nothing to verify here so we return success after
		// one rescan.
		if err := connector.RescanSessions(); err != nil {
			d.log().Error("NodeExpandVolume(): iSCSI session rescan failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "rescan iSCSI sessions: %v", err)
		}
		d.log().Info("NodeExpandVolume(): block volume expanded",
			zap.String("volume_path", req.GetVolumePath()))
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	if err := connector.RescanSessions(); err != nil {
		d.log().Error("NodeExpandVolume(): iSCSI session rescan failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "rescan iSCSI sessions: %v", err)
	}
	// Session rescan grows the kernel paths, but the existing multipath map
	// keeps the old size until it is reloaded; wait below then sees 1Gi
	// forever without this.
	if strings.HasPrefix(volumePath, "/dev/mapper/") {
		if err := iscsi.NewMultipath().ReloadDevice(volumePath); err != nil {
			d.log().Error("NodeExpandVolume(): multipath map reload failed",
				zap.String("device_path", volumePath), zap.Error(err))
			return nil, status.Errorf(codes.Internal, "reload multipath map: %v", err)
		}
	}
	if err := connector.WaitForDeviceSize(ctx, volumePath, req.GetCapacityRange().GetRequiredBytes()); err != nil {
		d.log().Error("NodeExpandVolume(): expanded device size not visible", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "wait for expanded device: %v", err)
	}
	if !blockMode {
		if err := d.filesystemStager().ResizeCoordinated(ctx, req.GetVolumeId(), volumePath, req.GetVolumePath()); err != nil {
			d.log().Error("NodeExpandVolume(): filesystem resize failed", zap.Error(err))
			if status.Code(err) != codes.Unknown {
				return nil, err
			}
			return nil, status.Errorf(codes.Internal, "resize filesystem: %v", err)
		}
	}
	d.log().Info("NodeExpandVolume(): volume expanded",
		zap.String("device_path", volumePath),
		zap.String("volume_path", req.GetVolumePath()),
		zap.Bool("block_mode", blockMode),
	)
	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetVolumeStats returns filesystem usage stats for a mounted volume.
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	d.log().Info("NodeGetVolumeStats(): init")
	d.log().Debug("NodeGetVolumeStats(): request",
		zap.String("volume_id", req.GetVolumeId()),
		zap.String("volume_path", req.GetVolumePath()),
	)

	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		d.log().Error("NodeGetVolumeStats(): volume ID and path are required")
		return nil, status.Error(codes.InvalidArgument, "volume ID and path are required")
	}
	if d.mounter == nil {
		d.log().Error("NodeGetVolumeStats(): mounter unavailable")
		return nil, status.Error(codes.Internal, "mounter unavailable")
	}
	// Require an active mount so an ordinary host directory is never reported
	// as storage capacity for the requested CSI volume.
	notMountPoint, err := d.mounter.IsLikelyNotMountPoint(req.GetVolumePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			d.log().Error("NodeGetVolumeStats(): volume path does not exist",
				zap.String("volume_path", req.GetVolumePath()), zap.Error(err))
			return nil, status.Errorf(codes.NotFound, "volume path is not mounted: %s", req.GetVolumePath())
		}
		d.log().Error("NodeGetVolumeStats(): mount point inspection failed",
			zap.String("volume_path", req.GetVolumePath()), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "checking mount point: %v", err)
	}
	if notMountPoint {
		d.log().Error("NodeGetVolumeStats(): volume path is not mounted",
			zap.String("volume_path", req.GetVolumePath()))
		return nil, status.Errorf(codes.NotFound, "volume path is not mounted: %s", req.GetVolumePath())
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(req.GetVolumePath(), &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			d.log().Error("NodeGetVolumeStats(): volume does not exist at path",
				zap.String("volume_path", req.GetVolumePath()))
			return nil, status.Errorf(codes.NotFound, "volume does not exist at path %q", req.GetVolumePath())
		}
		d.log().Error("NodeGetVolumeStats(): statfs failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "get volume stats: %v", err)
	}
	totalBytes := int64(stat.Blocks) * int64(stat.Bsize)
	availableBytes := int64(stat.Bavail) * int64(stat.Bsize)
	usedBytes := (int64(stat.Blocks) - int64(stat.Bfree)) * int64(stat.Bsize)
	d.log().Info("NodeGetVolumeStats(): stats returned",
		zap.Int64("available_bytes", availableBytes),
		zap.Int64("total_bytes", totalBytes),
		zap.Int64("used_bytes", usedBytes),
	)
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{{
			Available: availableBytes,
			Total:     totalBytes,
			Used:      usedBytes,
			Unit:      csi.VolumeUsage_BYTES,
		}},
	}, nil
}

// Filesystem operations (relocated from filesystem.go).
