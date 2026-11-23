package blockmount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// BlockMountState describes the effective state of one node target after
// inspecting the mount table and mount-point health for a block device.
type BlockMountState uint8

const (
	BlockMountAbsent BlockMountState = iota
	BlockMountCompatible
	BlockMountConflict
)

// Mounter is the shared mount-utils interface used by the node plugin.
var Mounter mount.Interface = mount.New("")

var SafeFormat = mount.NewSafeFormatAndMount(Mounter, utilexec.New())

var getDiskFormat = func(devicePath string) (string, error) {
	return SafeFormat.GetDiskFormat(devicePath)
}

var runFilesystemCommand = func(name string, args ...string) ([]byte, error) {
	return utilexec.New().Command(name, args...).CombinedOutput()
}

// FilesystemProbeState separates a confirmed empty block device from an
// existing filesystem. Probe failures are returned as errors and must never be
// interpreted as FilesystemEmpty.
type FilesystemProbeState uint8

const (
	FilesystemUnknown FilesystemProbeState = iota
	FilesystemEmpty
	FilesystemExisting
)

type FilesystemProbe struct {
	State  FilesystemProbeState
	FSType string
}

// ProbeFilesystem determines whether a block device is truly unformatted.
// mount-utils maps only blkid's explicit "no signature" exit status to an empty
// format; transport/I/O/command errors remain errors.
func ProbeFilesystem(devicePath string) (FilesystemProbe, error) {
	format, err := getDiskFormat(devicePath)
	if err != nil {
		return FilesystemProbe{}, fmt.Errorf("probe filesystem on %s: %w", devicePath, err)
	}
	if format == "" {
		return FilesystemProbe{State: FilesystemEmpty}, nil
	}
	return FilesystemProbe{State: FilesystemExisting, FSType: format}, nil
}

// FormatFilesystem formats a confirmed-empty block device without mounting it.
// Splitting format from mount allows callers to keep only the destructive format
// operation inside a distributed critical section.
func FormatFilesystem(devicePath, fsType string) error {
	if fsType == "" {
		fsType = "ext4"
	}
	if !validFilesystemType(fsType) {
		return fmt.Errorf("invalid filesystem type %q", fsType)
	}
	args := []string{devicePath}
	switch fsType {
	case "ext4", "ext3":
		args = []string{"-F", "-m0", devicePath}
	case "xfs":
		args = []string{"-f", devicePath}
	}
	output, err := runFilesystemCommand("mkfs."+fsType, args...)
	if err != nil {
		return fmt.Errorf("format filesystem %s on %s: %w: %s", fsType, devicePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func validFilesystemType(fsType string) bool {
	if fsType == "" {
		return false
	}
	for _, r := range fsType {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// VerifyFilesystem requires a post-format/readback signature matching the
// requested filesystem. It is also used after acquiring a first-format Lease to
// distinguish "another node already formatted it" from a still-empty device.
func VerifyFilesystem(devicePath, expectedFSType string) error {
	probe, err := ProbeFilesystem(devicePath)
	if err != nil {
		return err
	}
	if probe.State == FilesystemEmpty {
		return fmt.Errorf("filesystem verification on %s: device is still unformatted", devicePath)
	}
	if probe.FSType != expectedFSType {
		return fmt.Errorf("filesystem verification on %s: found %q, expected %q", devicePath, probe.FSType, expectedFSType)
	}
	return nil
}

// RepairFilesystem performs the mutating fsck step used only for non-readonly,
// single-writer staging of an existing filesystem. Shared/read-only staging must
// not call this helper.
func RepairFilesystem(devicePath string) error {
	output, err := runFilesystemCommand("fsck", "-a", devicePath)
	if err == nil || errors.Is(err, utilexec.ErrExecutableNotFound) {
		return nil
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitStatus() {
		case 1: // errors corrected
			return nil
		case 4: // errors left uncorrected
			return fmt.Errorf("fsck found uncorrected errors on %s: %s", devicePath, strings.TrimSpace(string(output)))
		default:
			// Preserve mount-utils behavior: unexpected fsck tool/runtime exit
			// codes are non-fatal for mounting. Only exit 4 proves uncorrected
			// filesystem errors and blocks the mount.
			return nil
		}
	}
	// Preserve mount-utils behavior for non-ExitError failures as well: fsck is
	// a best-effort repair step, not an availability gate unless it explicitly
	// reports uncorrected filesystem errors.
	return nil
}

// MountFilesystem mounts an already-verified filesystem without any implicit
// probe, formatting, or repair. Reader-only access modes pass readOnly=true so
// the staging mount itself is read-only, not merely the final bind mount.
func MountFilesystem(devicePath, targetPath, fsType string, readOnly bool, mountFlags []string) error {
	options := append([]string{}, mountFlags...)
	if readOnly {
		options = append(options, "ro")
	}
	options = append(options, "defaults")
	if err := Mounter.Mount(devicePath, targetPath, fsType, options); err != nil {
		return fmt.Errorf("mount filesystem %s on %s at %s: %w", fsType, devicePath, targetPath, err)
	}
	return nil
}

// PrepareBlockTarget creates the regular file required as a raw-block bind
// mount target. It replaces an empty directory left by an older failed retry.
func PrepareBlockTarget(targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return fmt.Errorf("create block target parent: %w", err)
	}
	info, err := os.Lstat(targetPath)
	if err == nil {
		if !info.IsDir() {
			return nil
		}
		if err := os.Remove(targetPath); err != nil {
			return fmt.Errorf("replace block target directory: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect block target: %w", err)
	}
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create block target file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close block target file: %w", err)
	}
	return nil
}

// MountVolume mounts a formatted device to the target path. For block (raw)
// volumes, the device is bind-mounted without formatting.
func MountVolume(devicePath, targetPath, fsType string, readOnly bool, mountFlags []string) error {
	options := append([]string{}, mountFlags...)
	if readOnly {
		options = append(options, "ro")
	}
	if fsType == "" || fsType == "block" {
		// Raw block mount: bind-mount the device node to the target path.
		return Mounter.Mount(devicePath, targetPath, "", append(options, "bind"))
	}
	return Mounter.Mount(devicePath, targetPath, fsType, options)
}

// UnmountVolume unmounts the target path if it is currently mounted.
func UnmountVolume(targetPath string) error {
	notMounted, err := Mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		// Path may not exist; treat as already unmounted.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("check mount point %s: %w", targetPath, err)
	}
	if notMounted {
		return nil
	}
	return Mounter.Unmount(targetPath)
}

// ResolveDevicePath resolves a /dev path from a WWID (SCSI VPD page 83
// identifier). The WWID is matched against /dev/disk/by-id/wwn-0x<wwid>.
func ResolveDevicePath(wwid string) string {
	if wwid == "" {
		return ""
	}
	// Normalize: strip 0x prefix if present, lowercase.
	clean := strings.TrimPrefix(strings.ToLower(wwid), "0x")
	byID := filepath.Join("/dev/disk/by-id", "wwn-0x"+clean)
	if resolved, err := filepath.EvalSymlinks(byID); err == nil {
		return resolved
	}
	return ""
}
