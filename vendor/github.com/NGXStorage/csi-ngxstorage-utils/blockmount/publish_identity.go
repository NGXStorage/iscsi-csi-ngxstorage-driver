package blockmount

import (
	"fmt"
	"os"

	mount "k8s.io/mount-utils"
)

// PublishTargetMatchesStaging reports whether an already-mounted publish
// target exposes the same volume as the staging path with a compatible
// read-only state. CSI idempotency requires compatible state, not merely an
// existing mount: a target bound from a different volume, a writable mount
// when read-only is requested (or vice versa), and an unresolved mount source
// all fail closed instead of being accepted as published.
//
// For raw block, identity is the device node behind the target (rdev), never
// the mount source string (a device bind reports devtmpfs). For filesystems,
// identity is the mounted device of the staging mount compared through
// SameBlockDevice (mapper aliases resolve equal); a missing staging mount is
// an error because the published bind cannot be verified against it.
//
// A not-mounted (or absent) target returns (false, nil) so the first publish
// bind proceeds. Any inspection/list/stat error is returned, never ignored.
func PublishTargetMatchesStaging(mounter mount.Interface, targetPath, stagingPath string, blockVolume, wantReadOnly bool) (bool, error) {
	notMounted, err := mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect publish target mount point: %w", err)
	}
	if notMounted {
		return false, nil
	}
	points, err := mounter.List()
	if err != nil {
		return false, fmt.Errorf("list mount points: %w", err)
	}
	var targetMP, stagingMP *mount.MountPoint
	for i := range points {
		if points[i].Path == targetPath {
			targetMP = &points[i]
		}
		if points[i].Path == stagingPath {
			stagingMP = &points[i]
		}
	}
	if targetMP == nil {
		return false, fmt.Errorf("publish target %s is mounted but has no mount entry", targetPath)
	}
	if blockVolume {
		matches, err := BlockStagingMatchesDevice(mounter, targetPath, stagingPath)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, fmt.Errorf("publish target %s is not bound from the staging device %s", targetPath, stagingPath)
		}
	} else {
		if stagingMP == nil {
			return false, fmt.Errorf("staging path %s is not mounted; published target cannot be verified", stagingPath)
		}
		if targetMP.Device == "" || stagingMP.Device == "" {
			return false, fmt.Errorf("publish target %s or staging %s has an unresolved mount source", targetPath, stagingPath)
		}
		if !SameBlockDevice(targetMP.Device, stagingMP.Device) {
			return false, fmt.Errorf("publish target %s is mounted from %s, expected staging device %s", targetPath, targetMP.Device, stagingMP.Device)
		}
	}
	if mountOptsReadOnly(targetMP.Opts) != wantReadOnly {
		return false, fmt.Errorf("publish target %s read-only state is incompatible with the requested publish (mounted ro=%v, requested ro=%v)",
			targetPath, mountOptsReadOnly(targetMP.Opts), wantReadOnly)
	}
	return true, nil
}

// StagingReadyForPublish verifies the staging path is actually mounted before
// a publish bind mount proceeds. Binding an unmounted staging directory would
// expose an empty directory as the volume (fake success after lost staging).
// For raw block the staging path must additionally be a live device node.
func StagingReadyForPublish(mounter mount.Interface, stagingPath string, blockVolume bool) error {
	notMounted, err := mounter.IsLikelyNotMountPoint(stagingPath)
	if err != nil {
		return fmt.Errorf("inspect staging mount point: %w", err)
	}
	if notMounted {
		return fmt.Errorf("staging path %s is not mounted", stagingPath)
	}
	if blockVolume {
		info, err := StatFile(stagingPath)
		if err != nil {
			return fmt.Errorf("stat staging target: %w", err)
		}
		if _, ok := DeviceIdentity(info); !ok {
			return fmt.Errorf("staging target %s is not a device node", stagingPath)
		}
	}
	return nil
}

func mountOptsReadOnly(opts []string) bool {
	for _, opt := range opts {
		if opt == "ro" {
			return true
		}
	}
	return false
}
