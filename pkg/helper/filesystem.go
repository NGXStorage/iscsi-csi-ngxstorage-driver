package helper

import (
	"context"
	"errors"
	"fmt"
	"os"

	blockmount "github.com/NGXStorage/csi-ngxstorage-utils/blockmount"
	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

type FilesystemOperations interface {
	Probe(string) (blockmount.FilesystemProbe, error)
	Format(string, string) error
	Verify(string, string) error
	Repair(string) error
	Mount(string, string, string, bool, []string) error
	Unmount(string) error
	Chmod(string, os.FileMode) error
}
type HostFilesystemOperations struct{}

func (HostFilesystemOperations) Probe(device string) (blockmount.FilesystemProbe, error) {
	return blockmount.ProbeFilesystem(device)
}
func (HostFilesystemOperations) Format(device, fsType string) error {
	return blockmount.FormatFilesystem(device, fsType)
}
func (HostFilesystemOperations) Verify(device, fsType string) error {
	return blockmount.VerifyFilesystem(device, fsType)
}
func (HostFilesystemOperations) Repair(device string) error {
	return blockmount.RepairFilesystem(device)
}
func (HostFilesystemOperations) Mount(device, target, fsType string, readOnly bool, flags []string) error {
	return blockmount.MountFilesystem(device, target, fsType, readOnly, flags)
}
func (HostFilesystemOperations) Unmount(target string) error { return blockmount.UnmountVolume(target) }
func (HostFilesystemOperations) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

// Node-side resize (relocated from resize.go).
type FilesystemResizer interface {
	NeedResize(devicePath, mountPath string) (bool, error)
	Resize(devicePath, mountPath string) (bool, error)
}
type HostFilesystemResizer struct{ resize *mount.ResizeFs }

func (h *HostFilesystemResizer) NeedResize(devicePath, mountPath string) (bool, error) {
	return h.resize.NeedResize(devicePath, mountPath)
}
func (h *HostFilesystemResizer) Resize(devicePath, mountPath string) (bool, error) {
	return h.resize.Resize(devicePath, mountPath)
}
func NewHostFilesystemResizer() FilesystemResizer {
	return &HostFilesystemResizer{resize: mount.NewResizeFs(utilexec.New())}
}

// stageFilesystem performs filesystem initialization independently from the
// final staging mount. The distributed lock protects only shared/destructive
// initialization state; the final mount happens after the Lease is released.
func (s *FilesystemStager) Stage(
	ctx context.Context,
	volumeID string,
	devicePath string,
	stagingPath string,
	fsType string,
	capability *csi.VolumeCapability,
	permissions os.FileMode,
	revalidate func(context.Context) (string, error),
) error {
	if fsType == "" {
		fsType = "ext4"
	}
	ops := s.ops()
	readerOnly := utils.ReaderOnlyCapability(capability)

	// CSI idempotency: a repeat NodeStageVolume for a volume already staged at
	// this path is a success. Without this the second mount fails EBUSY
	// (csi-sanity "should be idempotent").
	if notMounted, err := blockmount.Mounter.IsLikelyNotMountPoint(stagingPath); err == nil && !notMounted {
		s.log().Info("Stage(): staging path is already mounted",
			zap.String("staging_path", stagingPath))
		return nil
	}

	probe, err := ops.Probe(devicePath)
	if err != nil {
		return fmt.Errorf("probe filesystem: %w", err)
	}
	formattedHere := false

	// Reader-only staging always enters the short coordination critical section.
	// This prevents a second reader from observing a just-created signature while
	// the formatter still owns initialization (e.g. initial root permissions).
	needCoordination := readerOnly || probe.State == blockmount.FilesystemEmpty
	if needCoordination {
		if s.Coord == nil {
			return status.Error(codes.Unavailable, "filesystem initialization coordination is unavailable")
		}
		lock, err := s.Coord.Acquire(ctx, "volume:"+volumeID, coord.AcquireOptions{
			Safety:    coord.SafetyDestructive,
			Operation: coord.OperationFilesystem,
		})
		if err != nil {
			return utils.CoordinationError("acquire filesystem initialization lock", err)
		}

		criticalErr := func() error {
			if err := lock.AssertOwned(ctx); err != nil {
				return utils.CoordinationError("verify filesystem initialization lock ownership", err)
			}
			if revalidate != nil {
				resolved, err := revalidate(ctx)
				if err != nil {
					return fmt.Errorf("revalidate block device under filesystem lock: %w", err)
				}
				if resolved == "" {
					return errors.New("revalidate block device under filesystem lock returned empty path")
				}
				devicePath = resolved
			}
			probe, err = ops.Probe(devicePath)
			if err != nil {
				return fmt.Errorf("re-probe filesystem under lock: %w", err)
			}
			if probe.State == blockmount.FilesystemExisting {
				if probe.FSType != fsType {
					return fmt.Errorf("existing filesystem type %q does not match requested %q", probe.FSType, fsType)
				}
				s.log().Info("filesystem initialization skipped after coordinated re-probe",
					zap.String("volume_id", volumeID),
					zap.String("device", devicePath),
					zap.String("fs_type", probe.FSType),
				)
				return nil
			}
			if probe.State != blockmount.FilesystemEmpty {
				return fmt.Errorf("filesystem state is unknown under initialization lock")
			}

			if err := lock.AssertOwned(ctx); err != nil {
				return utils.CoordinationError("verify filesystem lock before format", err)
			}
			if err := ops.Format(devicePath, fsType); err != nil {
				return fmt.Errorf("format filesystem: %w", err)
			}
			if err := ops.Verify(devicePath, fsType); err != nil {
				return fmt.Errorf("verify formatted filesystem: %w", err)
			}
			s.log().Info("filesystem formatted under distributed coordination",
				zap.String("volume_id", volumeID),
				zap.String("device", devicePath),
				zap.String("fs_type", fsType),
			)
			formattedHere = true

			if readerOnly {
				// Preserve the existing mount_permissions contract without leaving
				// a shared reader volume staged RW. Only the single formatter, while
				// still holding the distributed initialization lock, performs this
				// private one-shot RW init mount.
				if err := lock.AssertOwned(ctx); err != nil {
					return utils.CoordinationError("verify filesystem lock before permission init", err)
				}
				if err := ops.Mount(devicePath, stagingPath, fsType, false, nil); err != nil {
					return fmt.Errorf("mount fresh filesystem for permission initialization: %w", err)
				}
				chmodErr := ops.Chmod(stagingPath, permissions)
				unmountErr := ops.Unmount(stagingPath)
				if chmodErr != nil || unmountErr != nil {
					return errors.Join(
						utils.WrapOptional("set fresh filesystem permissions", chmodErr),
						utils.WrapOptional("unmount fresh filesystem initialization mount", unmountErr),
					)
				}
			}
			return nil
		}()

		releaseErr := utils.ReleaseOperationLock(lock)
		if criticalErr != nil {
			if releaseErr != nil {
				return errors.Join(criticalErr, utils.CoordinationError("release filesystem initialization lock", releaseErr))
			}
			return criticalErr
		}
		if releaseErr != nil {
			return utils.CoordinationError("release filesystem initialization lock", releaseErr)
		}
	}

	// Outside the distributed critical section, never format. Re-probe the final
	// device path so a disappearance/change cannot be interpreted as permission to
	// create a new filesystem.
	probe, err = ops.Probe(devicePath)
	if err != nil {
		return fmt.Errorf("probe filesystem before staging mount: %w", err)
	}
	if probe.State != blockmount.FilesystemExisting {
		return fmt.Errorf("filesystem disappeared before staging mount")
	}
	if probe.FSType != fsType {
		return fmt.Errorf("filesystem type %q does not match requested %q", probe.FSType, fsType)
	}

	if !readerOnly && !formattedHere {
		if err := ops.Repair(devicePath); err != nil {
			return fmt.Errorf("repair existing filesystem: %w", err)
		}
	}
	if err := ops.Mount(devicePath, stagingPath, fsType, readerOnly, nil); err != nil {
		return fmt.Errorf("mount staging filesystem: %w", err)
	}
	if !readerOnly {
		if err := ops.Chmod(stagingPath, permissions); err != nil {
			return fmt.Errorf("set staging filesystem permissions: %w", err)
		}
	}
	return nil
}

// resizeFilesystemCoordinated serializes the actual filesystem mutation across
// nodes. Device/session rescans may happen before this function, but the
// authoritative NeedResize check is repeated while the per-volume Lease is held.
func (s *FilesystemStager) ResizeCoordinated(ctx context.Context, volumeID, devicePath, mountPath string) error {
	if s.Coord == nil {
		return utils.CoordinationError("filesystem resize coordination is unavailable", fmt.Errorf("coordinator is nil"))
	}
	lock, err := s.Coord.Acquire(ctx, "volume:"+volumeID, coord.AcquireOptions{
		Safety:    coord.SafetyOrdinary,
		Operation: coord.OperationExpand,
	})
	if err != nil {
		return utils.CoordinationError("acquire filesystem resize lock", err)
	}
	criticalErr := func() error {
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify filesystem resize lock ownership", err)
		}
		needResize, err := s.resizer().NeedResize(devicePath, mountPath)
		if err != nil {
			return fmt.Errorf("check filesystem resize requirement: %w", err)
		}
		if !needResize {
			return nil
		}
		if err := lock.AssertOwned(ctx); err != nil {
			return utils.CoordinationError("verify filesystem resize lock before mutation", err)
		}
		if _, err := s.resizer().Resize(devicePath, mountPath); err != nil {
			return fmt.Errorf("resize filesystem: %w", err)
		}
		return nil
	}()
	releaseErr := utils.ReleaseOperationLock(lock)
	if criticalErr != nil {
		if releaseErr != nil {
			return fmt.Errorf("%w; release filesystem resize lock: %v", criticalErr, releaseErr)
		}
		return criticalErr
	}
	if releaseErr != nil {
		return utils.CoordinationError("release filesystem resize lock", releaseErr)
	}
	return nil
}

// FilesystemStager orchestrates filesystem initialization and coordinated
// resize for one staged volume. Ops/Resizer are test seams; nil selects the
// host implementations. Coord serializes shared/destructive initialization.
type FilesystemStager struct {
	Ops     FilesystemOperations
	Resizer FilesystemResizer
	Coord   coord.Coordinator
	Log     *zap.Logger
}

func (s *FilesystemStager) ops() FilesystemOperations {
	if s.Ops != nil {
		return s.Ops
	}
	return HostFilesystemOperations{}
}

func (s *FilesystemStager) resizer() FilesystemResizer {
	if s.Resizer != nil {
		return s.Resizer
	}
	return NewHostFilesystemResizer()
}

func (s *FilesystemStager) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}
