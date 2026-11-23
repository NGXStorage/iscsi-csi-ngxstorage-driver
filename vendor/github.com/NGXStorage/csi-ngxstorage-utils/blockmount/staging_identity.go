package blockmount

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	mount "k8s.io/mount-utils"
)

// StagingMountMatchesDevice reports whether the staging path is already
// mounted from the expected block device. A repeat NodeStageVolume is
// idempotent only for the same resolved device: a mount-point inspection
// error is never ignored, and a mount from an unverified source fails closed
// instead of being accepted as staged. It is used by the filesystem staging
// path; raw-block staging is verified by BlockStagingMatchesDevice.
func StagingMountMatchesDevice(mounter mount.Interface, stagingPath, devicePath string) (bool, error) {
	notMounted, err := mounter.IsLikelyNotMountPoint(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing exists at the path yet: the first stage creates it. Any
			// other inspection failure is not ignored.
			return false, nil
		}
		return false, fmt.Errorf("inspect staging mount point: %w", err)
	}
	if notMounted {
		return false, nil
	}
	source, _, err := mount.GetDeviceNameFromMount(mounter, stagingPath)
	if err != nil {
		return false, fmt.Errorf("resolve staging mount source: %w", err)
	}
	if source == "" {
		return false, fmt.Errorf("staging path %s is mounted but its source is unresolved", stagingPath)
	}
	if !SameBlockDevice(source, devicePath) {
		return false, fmt.Errorf("staging path %s is mounted from %s, expected %s", stagingPath, source, devicePath)
	}
	return true, nil
}

// BlockStagingMatchesDevice reports whether the raw-block staging path is
// already bind-mounted from the expected device. A bind mount of a device node
// reports the containing filesystem (e.g. devtmpfs) as its mount source, so
// identity is proven by the device node behind the target — never by source
// strings. A missing type, a regular file, a different rdev, or any
// inspection/stat error returns a clear error and never an idempotent
// success. An absent/not-mounted staging path returns false so the first
// bind mount proceeds.
func BlockStagingMatchesDevice(mounter mount.Interface, stagingPath, devicePath string) (bool, error) {
	notMounted, err := mounter.IsLikelyNotMountPoint(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing exists at the path yet: the first stage creates it. Any
			// other inspection failure is not ignored.
			return false, nil
		}
		return false, fmt.Errorf("inspect staging mount point: %w", err)
	}
	if notMounted {
		return false, nil
	}
	stagingInfo, err := StatFile(stagingPath)
	if err != nil {
		return false, fmt.Errorf("stat staging target: %w", err)
	}
	deviceInfo, err := StatFile(devicePath)
	if err != nil {
		return false, fmt.Errorf("stat expected device: %w", err)
	}
	stagingRdev, ok := DeviceIdentity(stagingInfo)
	if !ok {
		return false, fmt.Errorf("staging target %s is not a device node", stagingPath)
	}
	deviceRdev, ok := DeviceIdentity(deviceInfo)
	if !ok {
		return false, fmt.Errorf("expected path %s is not a device node", devicePath)
	}
	if stagingRdev != deviceRdev {
		return false, fmt.Errorf("staging target %s is device %d, expected device %d", stagingPath, stagingRdev, deviceRdev)
	}
	return true, nil
}

// DeviceIdentity returns the Linux device identity (rdev) of a device node.
// Regular files, directories, missing syscall metadata and a zero rdev are not
// device identities.
func DeviceIdentity(info os.FileInfo) (uint64, bool) {
	if info == nil || info.Mode()&os.ModeDevice == 0 {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Rdev == 0 {
		return 0, false
	}
	return uint64(stat.Rdev), true
}

// SameBlockDevice compares two block-device paths, resolving /dev/mapper
// symlinks to their /dev/dm-N targets so multipath device aliases compare
// equal.
func SameBlockDevice(a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// StatFile is os.Stat, replaceable in tests to inject device metadata without
// real host devices.
var StatFile = os.Stat
