package utils

import (
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

func TestRequestedMountPermissions(t *testing.T) {
	if got := RequestedMountPermissions(nil); got != 0o777 {
		t.Fatalf("default = %o", got)
	}
	if got := RequestedMountPermissions(map[string]string{"mount_permissions": "0750"}); got != 0o750 {
		t.Fatalf("parsed = %o", got)
	}
	for _, bad := range []string{"abc", "0", "9999"} {
		if got := RequestedMountPermissions(map[string]string{"mount_permissions": bad}); got != 0o777 {
			t.Fatalf("bad %q -> %o", bad, got)
		}
	}
}

func TestReaderOnlyCapability(t *testing.T) {
	if ReaderOnlyCapability(nil) {
		t.Fatal("nil is not reader-only")
	}
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	} {
		cap := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
		if !ReaderOnlyCapability(cap) {
			t.Fatalf("mode %v must be reader-only", mode)
		}
	}
	cap := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}
	if ReaderOnlyCapability(cap) {
		t.Fatal("writer is not reader-only")
	}
}

func TestIsBlockVolume(t *testing.T) {
	if IsBlockVolume(nil) {
		t.Fatal("nil is not block")
	}
	block := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}}
	if !IsBlockVolume(block) {
		t.Fatal("block capability must be block")
	}
	mount := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}
	if IsBlockVolume(mount) {
		t.Fatal("mount capability is not block")
	}
}
