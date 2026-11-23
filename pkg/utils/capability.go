package utils

import (
	"os"
	"strconv"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

// RequestedMountPermissions resolves the publish-context mount permissions,
// defaulting to 0o777 when absent or malformed.
func RequestedMountPermissions(publishContext map[string]string) os.FileMode {
	raw := publishContext["mount_permissions"]
	if raw == "" {
		return 0o777
	}
	parsed, err := strconv.ParseUint(raw, 8, 9)
	if err != nil || parsed == 0 {
		return 0o777
	}
	return os.FileMode(parsed)
}

// ReaderOnlyCapability reports whether the access mode is definitionally
// read-only (single- or multi-node reader-only).
func ReaderOnlyCapability(capability *csi.VolumeCapability) bool {
	if capability == nil || capability.GetAccessMode() == nil {
		return false
	}
	switch capability.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		return true
	default:
		return false
	}
}

// IsBlockVolume reports whether the capability requests raw block access.
func IsBlockVolume(capability *csi.VolumeCapability) bool {
	return capability != nil && capability.GetBlock() != nil
}
