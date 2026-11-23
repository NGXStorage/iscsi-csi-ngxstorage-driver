package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	blockmount "github.com/NGXStorage/csi-ngxstorage-utils/blockmount"
	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	helper "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/helper"
	"github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/iscsi"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"
)

func TestNodeMutationRPCsSerializePerVolume(t *testing.T) {
	const volumeID = "iscsi.csi.ngxstorage.com.lock-test"
	d := &Driver{}
	held, err := d.nodeState.LockVolume(context.Background(), volumeID)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()

	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "stage",
			call: func(ctx context.Context) error {
				_, err := d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
					VolumeId: volumeID, StagingTargetPath: "/unused/stage",
				})
				return err
			},
		},
		{
			name: "unstage",
			call: func(ctx context.Context) error {
				_, err := d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
					VolumeId: volumeID, StagingTargetPath: "/unused/stage",
				})
				return err
			},
		},
		{
			name: "publish",
			call: func(ctx context.Context) error {
				_, err := d.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
					VolumeId: volumeID, StagingTargetPath: "/unused/stage", TargetPath: "/unused/target",
				})
				return err
			},
		},
		{
			name: "unpublish",
			call: func(ctx context.Context) error {
				_, err := d.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
					VolumeId: volumeID, TargetPath: "/unused/target",
				})
				return err
			},
		},
		{
			name: "expand",
			call: func(ctx context.Context) error {
				_, err := d.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{
					VolumeId: volumeID, VolumePath: "/unused/volume",
					CapacityRange: &csi.CapacityRange{RequiredBytes: 1},
				})
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := test.call(ctx); status.Code(err) != codes.DeadlineExceeded {
				t.Fatalf("status = %s, want DeadlineExceeded; err=%v", status.Code(err), err)
			}
		})
	}
}

func TestSanitizeIQNAlias(t *testing.T) {
	cases := []struct{ in, want string }{
		{"worker0.ocp.example.com", "worker0ocpexamplecom"},
		{"worker-1", "worker1"},
		{"host name_2", "hostname2"},
		{"---", ""},
		{"alreadyfine42", "alreadyfine42"},
	}
	for _, c := range cases {
		if got := iscsi.SanitizeIQNAlias(c.in); got != c.want {
			t.Errorf("iscsi.SanitizeIQNAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAdvertisedCapabilitiesHaveImplementedHealthSurface(t *testing.T) {
	for _, capability := range supportedNodeCapabilities {
		if capability == csi.NodeServiceCapability_RPC_GET_VOLUME_HEALTH || capability == csi.NodeServiceCapability_RPC_GET_STORAGE_HEALTH {
			t.Fatalf("iSCSI advertises unimplemented node health RPC: %s", capability)
		}
	}
	for _, capability := range supportedControllerCapabilities {
		if capability == csi.ControllerServiceCapability_RPC_GET_VOLUME_HEALTH || capability == csi.ControllerServiceCapability_RPC_LIST_VOLUME_HEALTH {
			t.Fatalf("iSCSI advertises unimplemented controller health RPC: %s", capability)
		}
	}
}

func TestNodePublishVolumeIdempotentWhenAlreadyBound(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	for _, path := range []string{staging, target} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Fixture models the real staged+published state: staging is mounted
	// from the volume device and the publish bind reports that same device
	// (FakeMounter resolves bind sources to the original device).
	mounter := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/dm-test", Path: staging},
		{Device: "/dev/dm-test", Path: target, Opts: []string{"bind"}},
	})
	d := &Driver{mounter: mounter}
	request := &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.0001", StagingTargetPath: staging,
		TargetPath: target, VolumeCapability: &csi.VolumeCapability{}, PublishContext: map[string]string{},
	}
	if _, err := d.NodePublishVolume(context.Background(), request); err != nil {
		t.Fatalf("repeat NodePublishVolume on a bound target: got %v, want idempotent success", err)
	}
}

func TestNodePublishVolumeReadonlyDoesNotFailOnEROFS(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fixture models production: staging is already mounted before publish.
	mounter := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/dm-test", Path: staging}})
	d := &Driver{mounter: mounter, bindMount: mounter.Mount}
	chmod := os.Chmod
	d.nodeChmod = func(name string, mode os.FileMode) error {
		points, _ := mounter.List()
		for _, point := range points {
			if point.Path == name {
				return &os.PathError{Op: "chmod", Path: name, Err: syscall.EROFS}
			}
		}
		return chmod(name, mode)
	}
	request := &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.0002", StagingTargetPath: staging, TargetPath: target,
		Readonly: true, VolumeCapability: &csi.VolumeCapability{}, PublishContext: map[string]string{},
	}
	if _, err := d.NodePublishVolume(context.Background(), request); err != nil {
		t.Fatalf("readonly publish must survive EROFS on the mounted root: %v", err)
	}
	if len(mounter.GetLog()) < 1 {
		t.Fatal("no mount action recorded")
	}
}

func TestIQNAlias(t *testing.T) {
	for _, nodeID := range []string{"worker0.ocp.example.com", "host name_2", "---", "x"} {
		a1 := iscsi.IQNAlias(nodeID)
		if len(a1) > 30 {
			t.Errorf("iscsi.IQNAlias(%q) = %q, length %d exceeds backend max 30", nodeID, a1, len(a1))
		}
		if a1 == "" {
			t.Fatalf("iscsi.IQNAlias(%q) returned empty alias", nodeID)
		}
		for _, r := range a1 {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
				t.Errorf("iscsi.IQNAlias(%q) = %q contains disallowed char %q", nodeID, a1, r)
			}
		}
		a2 := iscsi.IQNAlias(nodeID)
		if a1 == a2 {
			t.Errorf("iscsi.IQNAlias(%q) not randomized: %q == %q", nodeID, a1, a2)
		}
	}
	base := iscsi.SanitizeIQNAlias("worker0.ocp.example.com")
	if !strings.HasPrefix(iscsi.IQNAlias("worker0.ocp.example.com"), base[:20]) {
		t.Errorf("iscsi.IQNAlias should keep the sanitized node ID prefix")
	}
}

func TestNodeStageVolumeRejectsMalformedLunNumber(t *testing.T) {
	d := &Driver{}
	// A complete, otherwise-valid publish context isolates the lun_number
	// validation: before the fix the driver skipped past it and reached the
	// backend connector with LUN 0.
	for _, lun := range []string{"", "abc", "12x", "-1"} {
		_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
			VolumeId:          "iscsi.csi.ngxstorage.com.0000",
			StagingTargetPath: "/tmp/stage",
			PublishContext: map[string]string{
				"target_iqn": "iqn.2015-03.com.ngxio.test:tgt",
				"portals":    "10.0.0.1:3260",
				"lun_number": lun,
			},
			VolumeCapability: &csi.VolumeCapability{},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("lun_number=%q: want InvalidArgument, got %v", lun, err)
			continue
		}
		if !strings.Contains(status.Convert(err).Message(), "lun_number") {
			t.Errorf("lun_number=%q: error %q should mention lun_number", lun, status.Convert(err).Message())
		}
	}
}

// Regression: the CO may send
// readonly=false for a reader-only access mode, while the staging mount
// is ro. The publish must then enforce ro with no chmod attempt (chmod on an
// ro superblock fails EROFS and fails the whole kubelet SetUp).
func TestNodePublishVolumeReaderOnlyModeEnforcesReadOnly(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fixture models production: staging is already mounted before publish.
	mounter := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/dm-test", Path: staging}})
	var gotFlags []string
	bind := func(source, target, fstype string, options []string) error {
		gotFlags = append([]string(nil), options...)
		return mounter.Mount(source, target, fstype, options)
	}
	d := &Driver{mounter: mounter, bindMount: bind}
	chmodCalls := 0
	d.nodeChmod = func(name string, mode os.FileMode) error {
		chmodCalls++
		return &os.PathError{Op: "chmod", Path: name, Err: syscall.EROFS}
	}
	rox := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
	}
	request := &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.0003", StagingTargetPath: staging, TargetPath: target,
		Readonly: false, VolumeCapability: rox, PublishContext: map[string]string{},
	}
	if _, err := d.NodePublishVolume(context.Background(), request); err != nil {
		t.Fatalf("reader-only publish with CO readonly=false must succeed without chmod: %v", err)
	}
	if chmodCalls != 0 {
		t.Fatalf("chmod called %d times for reader-only publish, want 0", chmodCalls)
	}
	hasRO := false
	for _, flag := range gotFlags {
		if flag == "ro" {
			hasRO = true
		}
	}
	if !hasRO {
		t.Fatalf("reader-only bind flags %v miss ro", gotFlags)
	}
}

func TestNodeUnpublishVolumeRemovesTargetPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Driver{}
	if _, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.0004", TargetPath: target,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target path %q was not removed (stat err=%v)", target, err)
	}
}

func TestNodeGetVolumeStatsMissingPathReturnsNotFound(t *testing.T) {
	// Fixture models production: the node driver always has a mounter; the
	// absent path is reported by the mount-point inspection, not by a nil
	// mounter (which is now an Internal misconfiguration).
	d := &Driver{mounter: mount.NewFakeMounter(nil)}
	_, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "iscsi.csi.ngxstorage.com.0005",
		VolumePath: filepath.Join(t.TempDir(), "not-mounted"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("status code = %s, want %s; err=%v", status.Code(err), codes.NotFound, err)
	}
}

func TestNodeExpandVolumeUnknownVolumeReturnsNotFound(t *testing.T) {
	d := &Driver{sessionStateDir: t.TempDir()}
	_, err := d.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "iscsi.csi.ngxstorage.com.0006",
		VolumePath: "/some/path",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("status code = %s, want %s; err=%v", status.Code(err), codes.NotFound, err)
	}
}

// The controller keeps no per-node publish state, so a read-only flip cannot
// be told apart from an idempotent repeat. Advertising PUBLISH_READONLY would
// promise controller-side read-only enforcement the driver does not do
// (csi-sanity "already published but incompatible"); read-only is enforced by
// the node plugin's readonly bind mounts.
func TestPublishReadonlyCapabilityNotAdvertised(t *testing.T) {
	for _, capability := range supportedControllerCapabilities {
		if capability == csi.ControllerServiceCapability_RPC_PUBLISH_READONLY {
			t.Fatal("PUBLISH_READONLY must not be advertised without per-node publish state")
		}
	}
}

// Filesystem operations tests (relocated from filesystem_test.go).
type fakeFilesystemOps struct {
	mu          sync.Mutex
	state       blockmount.FilesystemProbe
	probeErr    error
	formatCount int
	repairCount int
	chmodCount  int
	mounts      []fakeFilesystemMount
	unmounts    int
}

type fakeFilesystemMount struct {
	readOnly bool
	target   string
}

func (f *fakeFilesystemOps) Probe(string) (blockmount.FilesystemProbe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.probeErr
}

func (f *fakeFilesystemOps) Format(_ string, fsType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.formatCount++
	f.state = blockmount.FilesystemProbe{State: blockmount.FilesystemExisting, FSType: fsType}
	return nil
}

func (f *fakeFilesystemOps) Verify(_ string, fsType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.State != blockmount.FilesystemExisting || f.state.FSType != fsType {
		return errors.New("filesystem verification failed")
	}
	return nil
}

func (f *fakeFilesystemOps) Repair(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repairCount++
	return nil
}

func (f *fakeFilesystemOps) Mount(_ string, target, _ string, readOnly bool, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = append(f.mounts, fakeFilesystemMount{readOnly: readOnly, target: target})
	return nil
}

func (f *fakeFilesystemOps) Unmount(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounts++
	return nil
}

func (f *fakeFilesystemOps) Chmod(string, os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chmodCount++
	return nil
}

type fakeCoordinator struct {
	gate         chan struct{}
	attempts     chan string
	acquireCount atomic.Int32
	assertCount  atomic.Int64
	failAssertAt atomic.Int64
	owned        atomic.Bool
	preflightErr error
	recordMu     sync.Mutex
	acquireKeys  []string
	multiKeys    [][]string
}

func newFakeCoordinator() *fakeCoordinator {
	return &fakeCoordinator{gate: make(chan struct{}, 1), attempts: make(chan string, 64)}
}

func (f *fakeCoordinator) Acquire(ctx context.Context, key string, _ coord.AcquireOptions) (coord.Lock, error) {
	f.recordMu.Lock()
	f.acquireKeys = append(f.acquireKeys, key)
	f.recordMu.Unlock()
	select {
	case f.attempts <- key:
	default:
	}
	select {
	case f.gate <- struct{}{}:
		f.acquireCount.Add(1)
		f.owned.Store(true)
		return &fakeLock{coordinator: f, lost: make(chan struct{})}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeCoordinator) AcquireMany(ctx context.Context, keys []string, _ coord.AcquireOptions) (coord.LockSet, error) {
	f.recordMu.Lock()
	f.multiKeys = append(f.multiKeys, append([]string(nil), keys...))
	f.recordMu.Unlock()
	return f.Acquire(ctx, "multi", coord.AcquireOptions{})
}

func (f *fakeCoordinator) Preflight(context.Context) error { return f.preflightErr }

type fakeLock struct {
	coordinator *fakeCoordinator
	lost        chan struct{}
	once        sync.Once
}

func (f *fakeLock) AssertOwned(context.Context) error {
	if at := f.coordinator.failAssertAt.Load(); at > 0 && f.coordinator.assertCount.Add(1) >= at {
		return errors.New("lock ownership lost")
	}
	if !f.coordinator.owned.Load() {
		return errors.New("lock ownership lost")
	}
	return nil
}

func (f *fakeLock) OwnershipLost() <-chan struct{} { return f.lost }

func (f *fakeLock) Release(context.Context) error {
	f.once.Do(func() {
		f.coordinator.owned.Store(false)
		<-f.coordinator.gate
	})
	return nil
}

func readerOnlyCapabilityForTest() *csi.VolumeCapability {
	return &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	}}
}

func writerCapabilityForTest() *csi.VolumeCapability {
	return &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}}
}

func TestStageFilesystemConcurrentReadersFormatExactlyOnce(t *testing.T) {
	ops := &fakeFilesystemOps{state: blockmount.FilesystemProbe{State: blockmount.FilesystemEmpty}}
	coord := newFakeCoordinator()
	d := &Driver{filesystemOps: ops, coordination: coord}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, target := range []string{"/stage-a", "/stage-b"} {
		target := target
		go func() {
			<-start
			errs <- d.filesystemStager().Stage(context.Background(), "vol-1", "/dev/dm-test", target, "ext4",
				readerOnlyCapabilityForTest(), 0o770,
				func(context.Context) (string, error) { return "/dev/dm-test", nil })
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("stageFilesystem contender %d: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent reader staging deadlocked")
		}
	}

	ops.mu.Lock()
	defer ops.mu.Unlock()
	if ops.formatCount != 1 {
		t.Fatalf("format count = %d, want exactly 1", ops.formatCount)
	}
	if ops.chmodCount != 1 || ops.unmounts != 1 {
		t.Fatalf("fresh readonly init chmod/unmount = %d/%d, want 1/1", ops.chmodCount, ops.unmounts)
	}
	var readonlyMounts, readwriteMounts int
	for _, mount := range ops.mounts {
		if mount.readOnly {
			readonlyMounts++
		} else {
			readwriteMounts++
		}
	}
	if readonlyMounts != 2 || readwriteMounts != 1 {
		t.Fatalf("mount counts ro/rw = %d/%d, want 2/1", readonlyMounts, readwriteMounts)
	}
	if got := coord.acquireCount.Load(); got != 2 {
		t.Fatalf("coordination acquire count = %d, want both readers to enter critical section", got)
	}
}

func TestStageFilesystemProbeErrorNeverFormats(t *testing.T) {
	ops := &fakeFilesystemOps{
		state:    blockmount.FilesystemProbe{State: blockmount.FilesystemUnknown},
		probeErr: errors.New("blkid I/O error"),
	}
	coord := newFakeCoordinator()
	d := &Driver{filesystemOps: ops, coordination: coord}
	err := d.filesystemStager().Stage(context.Background(), "vol-1", "/dev/dm-test", "/stage", "ext4",
		writerCapabilityForTest(), 0o770, nil)
	if err == nil {
		t.Fatal("probe error should fail staging")
	}
	if ops.formatCount != 0 {
		t.Fatalf("probe error triggered %d formats", ops.formatCount)
	}
	if got := coord.acquireCount.Load(); got != 0 {
		t.Fatalf("probe error acquired coordination lock %d times", got)
	}
}

func TestStageFilesystemExistingReaderSkipsRepairAndChmod(t *testing.T) {
	ops := &fakeFilesystemOps{state: blockmount.FilesystemProbe{State: blockmount.FilesystemExisting, FSType: "ext4"}}
	coord := newFakeCoordinator()
	d := &Driver{filesystemOps: ops, coordination: coord}
	if err := d.filesystemStager().Stage(context.Background(), "vol-1", "/dev/dm-test", "/stage", "ext4",
		readerOnlyCapabilityForTest(), 0o770, nil); err != nil {
		t.Fatal(err)
	}
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if ops.formatCount != 0 || ops.repairCount != 0 || ops.chmodCount != 0 {
		t.Fatalf("existing reader mutated filesystem: format/repair/chmod=%d/%d/%d",
			ops.formatCount, ops.repairCount, ops.chmodCount)
	}
	if len(ops.mounts) != 1 || !ops.mounts[0].readOnly {
		t.Fatalf("existing reader staging mounts = %+v, want one readonly mount", ops.mounts)
	}
	if got := coord.acquireCount.Load(); got != 1 {
		t.Fatalf("existing reader acquire count = %d, want 1", got)
	}
}

func TestStageFilesystemExistingWriterRepairsWithoutDistributedLock(t *testing.T) {
	ops := &fakeFilesystemOps{state: blockmount.FilesystemProbe{State: blockmount.FilesystemExisting, FSType: "ext4"}}
	coord := newFakeCoordinator()
	d := &Driver{filesystemOps: ops, coordination: coord}
	if err := d.filesystemStager().Stage(context.Background(), "vol-1", "/dev/dm-test", "/stage", "ext4",
		writerCapabilityForTest(), 0o770, nil); err != nil {
		t.Fatal(err)
	}
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if ops.formatCount != 0 || ops.repairCount != 1 || ops.chmodCount != 1 {
		t.Fatalf("existing writer operations format/repair/chmod=%d/%d/%d, want 0/1/1",
			ops.formatCount, ops.repairCount, ops.chmodCount)
	}
	if len(ops.mounts) != 1 || ops.mounts[0].readOnly {
		t.Fatalf("existing writer staging mounts = %+v, want one rw mount", ops.mounts)
	}
	if got := coord.acquireCount.Load(); got != 0 {
		t.Fatalf("existing writer unnecessarily acquired distributed lock %d times", got)
	}
}

// A repeat NodeStageVolume for a volume already staged at that path must
// succeed without mounting again (the second mount fails EBUSY).
func TestStageFilesystemIdempotentWhenAlreadyMounted(t *testing.T) {
	staging := t.TempDir()
	original := blockmount.Mounter
	blockmount.Mounter = mount.NewFakeMounter([]mount.MountPoint{{Path: staging}})
	t.Cleanup(func() { blockmount.Mounter = original })

	ops := &fakeFilesystemOps{state: blockmount.FilesystemProbe{State: blockmount.FilesystemExisting, FSType: "ext4"}}
	d := &Driver{filesystemOps: ops}
	if err := d.filesystemStager().Stage(context.Background(), "vol-1", "/dev/dm-test", staging, "ext4", nil, 0o777, nil); err != nil {
		t.Fatalf("idempotent stage: %v", err)
	}
	if len(ops.mounts) != 0 {
		t.Fatalf("mount called %d times, want 0 for an already-staged path", len(ops.mounts))
	}
}

// Node-side resize tests (relocated from resize_test.go).
type fakeFilesystemResizer struct {
	mu          sync.Mutex
	need        bool
	resizeCount int
}

func (f *fakeFilesystemResizer) NeedResize(string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.need, nil
}

func (f *fakeFilesystemResizer) Resize(string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.need {
		return false, nil
	}
	f.resizeCount++
	f.need = false
	return true, nil
}

func TestResizeFilesystemCoordinatedConcurrentCallsResizeOnce(t *testing.T) {
	coord := newFakeCoordinator()
	resizer := &fakeFilesystemResizer{need: true}
	d := &Driver{coordination: coord, filesystemResizer: resizer}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			errs <- d.filesystemStager().ResizeCoordinated(context.Background(), "vol-1", "/dev/dm-test", "/mnt/test")
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("filesystem resize contenders deadlocked")
		}
	}
	resizer.mu.Lock()
	defer resizer.mu.Unlock()
	if resizer.resizeCount != 1 {
		t.Fatalf("resize count = %d, want exactly 1", resizer.resizeCount)
	}
}

// The pending-state chain: NodeStage persists the session state BEFORE
// Connect, so a failed Connect (portal-B error) or a failed
// WaitForMappedDevice after a successful Connect stays recoverable through
// NodeUnstage. The state file is the per-volume cleanup responsibility
// record; Connect's rollback only covers the sessions the attempt itself
// created. These tests exercise the full RPC chain offline with a fake
// iscsiadm/device environment.

const (
	chainIQN     = "iqn.2026-11.com.example:chain-target"
	chainSCSIID  = "589c005636424757595041e52a73d6d3"
	chainPortalA = "10.9.0.1:3260"
	chainPortalB = "10.9.0.2:3260"
)

// chainHost fakes iscsiadm/multipath and records every command. Login results
// are per-portal exit codes (0 = newly created/owned, 15 = pre-existing,
// anything else = failure); logout failures are injectable per portal.
type chainHost struct {
	mu           sync.Mutex
	commands     []string
	loginExit    map[string]int
	discoverFail map[string]bool
	logoutFail   map[string]bool
}

func (h *chainHost) record(cmd string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, cmd)
}

func (h *chainHost) snapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.commands, "\n")
}

func chainPortalOf(args []string) string {
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func chainExitError(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

func (h *chainHost) exec(name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	h.record(cmd)
	if name != "iscsiadm" {
		return nil, nil // multipath -f and friends succeed
	}
	portal := chainPortalOf(args)
	switch {
	case strings.Contains(cmd, "--discover"):
		if h.discoverFail[portal] {
			return nil, chainExitError(11)
		}
	case strings.Contains(cmd, " -l"):
		if code := h.loginExit[portal]; code != 0 {
			return nil, chainExitError(code)
		}
	case strings.Contains(cmd, " -u"):
		if h.logoutFail[portal] {
			return nil, fmt.Errorf("simulated logout failure")
		}
	case strings.Contains(cmd, "-m session"):
		return []byte(""), nil // no remaining sessions
	}
	return nil, nil
}

// chainEnv wires a Driver to the fake host: initiator file, device root,
// session-state dir, fake mounter and the connector factory override.
type chainEnv struct {
	stateDir string
	devRoot  string
	sysfs    string
	driver   *Driver
}

func newChainEnv(t *testing.T, host *chainHost) *chainEnv {
	t.Helper()
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "auth_group") {
			w.Write([]byte(`{"success":true}`))
			return
		}
		http.NotFound(w, r)
	})
	d := newReconcileDriver(t, reconcileTransport(server, nil))
	env := &chainEnv{stateDir: t.TempDir(), devRoot: t.TempDir(), sysfs: t.TempDir()}
	d.sessionStateDir = env.stateDir

	iqnPath := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	if err := os.WriteFile(iqnPath, []byte("InitiatorName=iqn.2026-11.com.example:chain-node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.devRoot, "disk", "by-path"), 0o755); err != nil {
		t.Fatal(err)
	}
	origFactory := newISCSIConnector
	newISCSIConnector = func() *iscsi.Connector {
		return &iscsi.Connector{
			IQNPath:      iqnPath,
			SYSFSPath:    env.sysfs,
			DEVRoot:      env.devRoot,
			WaitInterval: time.Millisecond,
			WaitTimeout:  40 * time.Millisecond,
			Exec:         host.exec,
			ExecContext: func(_ context.Context, name string, args ...string) ([]byte, error) {
				return host.exec(name, args...)
			},
		}
	}
	t.Cleanup(func() { newISCSIConnector = origFactory })
	origMounter := blockmount.Mounter
	blockmount.Mounter = mount.NewFakeMounter(nil)
	t.Cleanup(func() { blockmount.Mounter = origMounter })
	env.driver = d
	return env
}

func (e *chainEnv) stageRequest(volumeID, staging string, portals ...string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: staging,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		},
		PublishContext: map[string]string{
			"target_iqn": chainIQN,
			"portals":    strings.Join(portals, ","),
			"scsi_id":    chainSCSIID,
			"lun_number": "4",
			"auth_group": "ag-chain",
		},
	}
}

func (e *chainEnv) unstageRequest(volumeID, staging string) *csi.NodeUnstageVolumeRequest {
	return &csi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: staging}
}

// chainDevice adds the by-path symlink Connect's per-portal WaitForDevice
// needs. No sysfs wwid is created, so WaitForMappedDevice still fails.
func (e *chainEnv) chainDevice(t *testing.T, portal string) {
	t.Helper()
	device := filepath.Join(e.devRoot, "sd-"+strings.ReplaceAll(portal, ":", "-"))
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.devRoot, "disk", "by-path", fmt.Sprintf("ip-%s-iscsi-%s-lun-4", portal, chainIQN))
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(device)), link); err != nil {
		t.Fatal(err)
	}
}

// chainMultipathDevice adds the dm-uuid symlink Connect's multipath
// convergence needs. No sysfs wwid entries exist, so NodeStage's
// WaitForMappedDevice (per-path WWID verification) still fails afterwards.
func (e *chainEnv) chainMultipathDevice(t *testing.T) {
	t.Helper()
	wwid, err := iscsi.MultipathWWID(chainSCSIID)
	if err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(e.devRoot, "dm-chain")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	byID := filepath.Join(e.devRoot, "disk", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(byID, "dm-uuid-mpath-"+wwid)
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(device)), link); err != nil {
		t.Fatal(err)
	}
}

func (e *chainEnv) loadState(t *testing.T, volumeID string) helper.ISCSISessionState {
	t.Helper()
	state, err := helper.LoadISCSISessionState(e.stateDir, volumeID)
	if err != nil {
		t.Fatalf("session state for %s: %v", volumeID, err)
	}
	return state
}

func (e *chainEnv) assertStateAssociation(t *testing.T, volumeID string, portals ...string) {
	t.Helper()
	state := e.loadState(t, volumeID)
	if state.TargetIQN != chainIQN || state.SCSIID != chainSCSIID {
		t.Fatalf("state identity mismatch: %+v", state)
	}
	if strings.Join(state.Portals, ",") != strings.Join(portals, ",") {
		t.Fatalf("state portals = %v, want %v", state.Portals, portals)
	}
}

func TestNodeStagePendingStateUnstageChain(t *testing.T) {
	t.Run("connect portal-B failure: state pre-persisted, unstage consumes it", func(t *testing.T) {
		host := &chainHost{loginExit: map[string]int{chainPortalB: 11}}
		env := newChainEnv(t, host)
		env.chainDevice(t, chainPortalA)
		// An unrelated volume's state must survive this volume's cleanup.
		other := helper.ISCSISessionState{TargetIQN: "iqn.other", Portals: []string{"10.9.9.9:3260"}, SCSIID: chainSCSIID}
		if err := helper.SaveISCSISessionState(env.stateDir, "volume-other", other); err != nil {
			t.Fatal(err)
		}
		staging := t.TempDir()
		_, err := env.driver.NodeStageVolume(context.Background(), env.stageRequest("volume-1", staging, chainPortalA, chainPortalB))
		// Login exit 11 is a transient transport-capability failure, not a
		// wait timeout: the CO must see Unavailable and retry.
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("stage error = %v, want Unavailable", err)
		}
		// State was persisted BEFORE Connect: correct volume/IQN/portal bond.
		env.assertStateAssociation(t, "volume-1", chainPortalA, chainPortalB)
		// Connect rolled back exactly its own new session (portal A).
		if !strings.Contains(host.snapshot(), "-p "+chainPortalA+" -u") {
			t.Fatalf("owned portal A not rolled back:\n%s", host.snapshot())
		}
		// Unstage consumes the persisted responsibility: both state portals.
		if _, err := env.driver.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-1", staging)); err != nil {
			t.Fatalf("unstage: %v", err)
		}
		joined := host.snapshot()
		if !strings.Contains(joined, "-p "+chainPortalB+" -u") {
			t.Fatalf("unstage did not disconnect state portal B:\n%s", joined)
		}
		if _, err := helper.LoadISCSISessionState(env.stateDir, "volume-1"); !os.IsNotExist(err) {
			t.Fatalf("state survived successful cleanup: %v", err)
		}
		// Per-volume association: the other volume's state is untouched.
		got := env.loadState(t, "volume-other")
		if got.TargetIQN != "iqn.other" {
			t.Fatalf("other volume's state mutated: %+v", got)
		}
		// Idempotent retry after consumed cleanup.
		if _, err := env.driver.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-1", staging)); err != nil {
			t.Fatalf("repeat unstage: %v", err)
		}
	})

	t.Run("wait-for-mapped-device failure after connect: state survives, unstage cleans", func(t *testing.T) {
		host := &chainHost{}
		env := newChainEnv(t, host)
		env.chainDevice(t, chainPortalA)
		env.chainDevice(t, chainPortalB)
		env.chainMultipathDevice(t)
		staging := t.TempDir()
		_, err := env.driver.NodeStageVolume(context.Background(), env.stageRequest("volume-2", staging, chainPortalA, chainPortalB))
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("stage error = %v, want DeadlineExceeded", err)
		}
		env.assertStateAssociation(t, "volume-2", chainPortalA, chainPortalB)
		// Connect succeeded: no rollback logout happened during stage.
		if strings.Contains(host.snapshot(), " -u") {
			t.Fatalf("successful Connect rolled back sessions:\n%s", host.snapshot())
		}
		if _, err := env.driver.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-2", staging)); err != nil {
			t.Fatalf("unstage: %v", err)
		}
		joined := host.snapshot()
		for _, portal := range []string{chainPortalA, chainPortalB} {
			if !strings.Contains(joined, "-p "+portal+" -u") {
				t.Fatalf("unstage missed portal %s:\n%s", portal, joined)
			}
		}
		if _, err := helper.LoadISCSISessionState(env.stateDir, "volume-2"); !os.IsNotExist(err) {
			t.Fatalf("state survived successful cleanup: %v", err)
		}
	})

	t.Run("failed cleanup keeps state; restart retry consumes it", func(t *testing.T) {
		host := &chainHost{loginExit: map[string]int{chainPortalB: 11}}
		env := newChainEnv(t, host)
		env.chainDevice(t, chainPortalA)
		staging := t.TempDir()
		if _, err := env.driver.NodeStageVolume(context.Background(), env.stageRequest("volume-3", staging, chainPortalA, chainPortalB)); err == nil {
			t.Fatal("stage succeeded despite portal-B login failure")
		}
		env.assertStateAssociation(t, "volume-3", chainPortalA, chainPortalB)
		// Cleanup fails on portal B: state must NOT be deleted.
		host.logoutFail = map[string]bool{chainPortalB: true}
		if _, err := env.driver.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-3", staging)); err == nil {
			t.Fatal("unstage succeeded despite logout failure")
		}
		env.assertStateAssociation(t, "volume-3", chainPortalA, chainPortalB)
		// Restart: a fresh Driver reads the same persisted state from disk.
		server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "auth_group") {
				w.Write([]byte(`{"success":true}`))
				return
			}
			http.NotFound(w, r)
		})
		restarted := newReconcileDriver(t, reconcileTransport(server, nil))
		restarted.sessionStateDir = env.stateDir
		host.logoutFail = nil
		if _, err := restarted.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-3", staging)); err != nil {
			t.Fatalf("restart retry unstage: %v", err)
		}
		if _, err := helper.LoadISCSISessionState(env.stateDir, "volume-3"); !os.IsNotExist(err) {
			t.Fatalf("state survived successful restart cleanup: %v", err)
		}
	})

	t.Run("preexisting session never rolled back; state still covers cleanup", func(t *testing.T) {
		host := &chainHost{loginExit: map[string]int{chainPortalA: 15, chainPortalB: 11}}
		env := newChainEnv(t, host)
		env.chainDevice(t, chainPortalA)
		staging := t.TempDir()
		if _, err := env.driver.NodeStageVolume(context.Background(), env.stageRequest("volume-4", staging, chainPortalA, chainPortalB)); err == nil {
			t.Fatal("stage succeeded despite portal-B login failure")
		}
		// Connect-attempt ownership: portal A pre-existed, never logged out.
		if strings.Contains(host.snapshot(), "-p "+chainPortalA+" -u") {
			t.Fatalf("preexisting portal A rolled back by Connect:\n%s", host.snapshot())
		}
		// Per-volume persistent responsibility is separate: state records
		// both portals and Unstage consumes exactly that record.
		env.assertStateAssociation(t, "volume-4", chainPortalA, chainPortalB)
		if _, err := env.driver.NodeUnstageVolume(context.Background(), env.unstageRequest("volume-4", staging)); err != nil {
			t.Fatalf("unstage: %v", err)
		}
		joined := host.snapshot()
		for _, portal := range []string{chainPortalA, chainPortalB} {
			if !strings.Contains(joined, "-p "+portal+" -u") {
				t.Fatalf("unstage missed state portal %s:\n%s", portal, joined)
			}
		}
		if _, err := helper.LoadISCSISessionState(env.stateDir, "volume-4"); !os.IsNotExist(err) {
			t.Fatalf("state survived successful cleanup: %v", err)
		}
	})
}

// --- Regression: staging/publish identity and stats mount guard ---

const (
	blockDeviceMode = os.ModeDevice | 0o660
	regularFileMode = os.FileMode(0o600)
)

// fakeDeviceInfo injects device metadata without real host device nodes.
type fakeDeviceInfo struct {
	mode os.FileMode
	rdev uint64
}

func (f fakeDeviceInfo) Name() string       { return "fake" }
func (f fakeDeviceInfo) Size() int64        { return 0 }
func (f fakeDeviceInfo) Mode() os.FileMode  { return f.mode }
func (f fakeDeviceInfo) ModTime() time.Time { return time.Time{} }
func (f fakeDeviceInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeDeviceInfo) Sys() interface{}   { return &syscall.Stat_t{Rdev: f.rdev} }

func swapStatFile(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	original := blockmount.StatFile
	blockmount.StatFile = fn
	t.Cleanup(func() { blockmount.StatFile = original })
}

// chainWWID gives the single-portal path a sysfs WWID so device convergence
// succeeds (the default chain env has none and stage fails at wait-for-device).
func (e *chainEnv) chainWWID(t *testing.T, portal string) {
	t.Helper()
	wwid, err := iscsi.MultipathWWID(chainSCSIID)
	if err != nil {
		t.Fatal(err)
	}
	base := "sd-" + strings.ReplaceAll(portal, ":", "-")
	dir := filepath.Join(e.sysfs, "class", "block", base, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wwid"), []byte(wwid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A repeat raw-block stage is idempotent ONLY when the staging bind
// exposes the same resolved device (rdev identity, never source strings).
func TestNodeStageBlockIdempotentSameDevice(t *testing.T) {
	host := &chainHost{}
	env := newChainEnv(t, host)
	env.chainDevice(t, chainPortalA)
	env.chainWWID(t, chainPortalA)
	staging := filepath.Join(t.TempDir(), "block-staging")
	req := env.stageRequest("volume-block-1", staging, chainPortalA)
	if _, err := env.driver.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first block stage: %v", err)
	}
	// The fake bind does not turn the staging file into a real device node;
	// inject the device identity a production bind mount would expose.
	swapStatFile(t, func(string) (os.FileInfo, error) {
		return fakeDeviceInfo{mode: blockDeviceMode, rdev: 42}, nil
	})
	fm := blockmount.Mounter.(*mount.FakeMounter)
	before := len(fm.GetLog())
	if _, err := env.driver.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("repeat block stage on the same device: %v", err)
	}
	if len(fm.GetLog()) != before {
		t.Fatalf("repeat stage mounted again: log %d -> %d", before, len(fm.GetLog()))
	}
}

// A staging bind from a DIFFERENT device (stale H:C:T:L reuse) must fail
// closed, never be reported as a successful idempotent stage.
func TestNodeStageBlockWrongDeviceFailsClosed(t *testing.T) {
	host := &chainHost{}
	env := newChainEnv(t, host)
	env.chainDevice(t, chainPortalA)
	env.chainWWID(t, chainPortalA)
	staging := filepath.Join(t.TempDir(), "block-staging")
	req := env.stageRequest("volume-block-2", staging, chainPortalA)
	if _, err := env.driver.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first block stage: %v", err)
	}
	swapStatFile(t, func(path string) (os.FileInfo, error) {
		if path == staging {
			return fakeDeviceInfo{mode: blockDeviceMode, rdev: 43}, nil
		}
		return fakeDeviceInfo{mode: blockDeviceMode, rdev: 42}, nil
	})
	_, err := env.driver.NodeStageVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("repeat stage on a different device: code=%s err=%v, want Internal fail-closed", status.Code(err), err)
	}
}

// A regular file at the staging path is not a device bind.
func TestNodeStageBlockRegularFileStagingFailsClosed(t *testing.T) {
	host := &chainHost{}
	env := newChainEnv(t, host)
	env.chainDevice(t, chainPortalA)
	env.chainWWID(t, chainPortalA)
	staging := filepath.Join(t.TempDir(), "block-staging")
	req := env.stageRequest("volume-block-3", staging, chainPortalA)
	if _, err := env.driver.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("first block stage: %v", err)
	}
	swapStatFile(t, func(string) (os.FileInfo, error) {
		return fakeDeviceInfo{mode: regularFileMode}, nil
	})
	if _, err := env.driver.NodeStageVolume(context.Background(), req); status.Code(err) != codes.Internal {
		t.Fatalf("repeat stage on a regular-file staging: code=%s err=%v, want Internal", status.Code(err), err)
	}
}

// An already-mounted target bound from a different volume is not an
// idempotent publish.
func TestNodePublishVolumeForeignTargetFailsClosed(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	for _, p := range []string{staging, target} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mounter := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/dm-1", Path: staging},
		{Device: "/dev/dm-2", Path: target, Opts: []string{"bind"}},
	})
	d := &Driver{mounter: mounter}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.pub-foreign", StagingTargetPath: staging, TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{}, PublishContext: map[string]string{},
	})
	if err == nil {
		t.Fatal("publish succeeded on a target bound from a different volume")
	}
}

// A writable existing mount does not satisfy a read-only publish.
func TestNodePublishVolumeReadOnlyMismatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	for _, p := range []string{staging, target} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mounter := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/dm-1", Path: staging, Opts: []string{"rw"}},
		{Device: "/dev/dm-1", Path: target, Opts: []string{"bind"}},
	})
	d := &Driver{mounter: mounter}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.pub-ro", StagingTargetPath: staging, TargetPath: target,
		Readonly: true, VolumeCapability: &csi.VolumeCapability{}, PublishContext: map[string]string{},
	})
	if err == nil {
		t.Fatal("read-only publish succeeded on a writable existing mount")
	}
}

// A first publish must never bind an unmounted staging directory (an
// empty directory would be exposed as the volume).
func TestNodePublishVolumeStagingNotMountedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	target := filepath.Join(dir, "target")
	for _, p := range []string{staging, target} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binds := 0
	d := &Driver{mounter: mount.NewFakeMounter(nil)}
	d.bindMount = func(source, target, fstype string, options []string) error {
		binds++
		return nil
	}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.pub-nostage", StagingTargetPath: staging, TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{}, PublishContext: map[string]string{},
	})
	if err == nil {
		t.Fatal("publish succeeded although staging is not mounted")
	}
	if binds != 0 {
		t.Fatalf("bind called %d times on unmounted staging, want 0", binds)
	}
}

// A block publish target bound from a different device rdev fails closed.
func TestNodePublishVolumeBlockForeignDeviceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "block-staging")
	target := filepath.Join(dir, "block-target")
	for _, p := range []string{staging, target} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mounter := mount.NewFakeMounter([]mount.MountPoint{{Device: "devtmpfs", Path: target, Opts: []string{"bind"}}})
	swapStatFile(t, func(path string) (os.FileInfo, error) {
		if path == target {
			return fakeDeviceInfo{mode: blockDeviceMode, rdev: 43}, nil
		}
		return fakeDeviceInfo{mode: blockDeviceMode, rdev: 42}, nil
	})
	d := &Driver{mounter: mounter}
	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.pub-block", StagingTargetPath: staging, TargetPath: target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		},
		PublishContext: map[string]string{},
	})
	if err == nil {
		t.Fatal("block publish succeeded on a target bound from a different device")
	}
}

// Stats require an active mount; an ordinary host directory is never
// reported as the volume's capacity.
func TestNodeGetVolumeStatsRequiresMountedPath(t *testing.T) {
	volumePath := t.TempDir()
	d := &Driver{mounter: mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/dm-1", Path: volumePath}})}
	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.stats", VolumePath: volumePath,
	})
	if err != nil {
		t.Fatalf("mounted volume stats: %v", err)
	}
	if len(resp.GetUsage()) != 1 || resp.GetUsage()[0].GetUnit() != csi.VolumeUsage_BYTES {
		t.Fatalf("usage = %#v, want one BYTES entry", resp.GetUsage())
	}
	d.mounter = mount.NewFakeMounter(nil)
	_, err = d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "iscsi.csi.ngxstorage.com.stats", VolumePath: volumePath,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unmounted path: code=%s err=%v, want NotFound", status.Code(err), err)
	}
}

// TestNodeStageConnectErrorMapping pins the cause-based error mapping for
// Connect/device-discovery failures on the full RPC chain: permanent
// login/auth rejection is FailedPrecondition, transient host/fabric failures
// are Unavailable, genuine wait expiry is DeadlineExceeded, and context end
// propagates. The session-state responsibility record is pre-persisted for
// every connector failure so NodeUnstage can always recover the volume.
func TestNodeStageConnectErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		host   *chainHost
		device bool
		want   codes.Code
	}{
		{
			name: "login authentication failure exit 19",
			host: &chainHost{loginExit: map[string]int{chainPortalA: 19}},
			want: codes.FailedPrecondition,
		},
		{
			name: "unauthorized initiator exit 24",
			host: &chainHost{loginExit: map[string]int{chainPortalA: 24}},
			want: codes.FailedPrecondition,
		},
		{
			name: "transient login failure exit 11",
			host: &chainHost{loginExit: map[string]int{chainPortalA: 11}},
			want: codes.Unavailable,
		},
		{
			name: "discovery failure is transient",
			host: &chainHost{discoverFail: map[string]bool{chainPortalA: true}},
			want: codes.Unavailable,
		},
		{
			name: "device wait expiry is a genuine timeout",
			host: &chainHost{},
			want: codes.DeadlineExceeded,
		},
		{
			name:   "mapped-device wait expiry is a genuine timeout",
			host:   &chainHost{},
			device: true,
			want:   codes.DeadlineExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newChainEnv(t, tc.host)
			if tc.device {
				env.chainDevice(t, chainPortalA)
			}
			staging := t.TempDir()
			_, err := env.driver.NodeStageVolume(context.Background(), env.stageRequest("volume-map", staging, chainPortalA))
			if status.Code(err) != tc.want {
				t.Fatalf("stage error code = %s, want %s (err=%v)", status.Code(err), tc.want, err)
			}
			// The responsibility record survives every connector failure.
			env.assertStateAssociation(t, "volume-map", chainPortalA)
		})
	}

	t.Run("canceled context during connect device wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &chainHost{}
		env := newChainEnv(t, host)
		// No device symlink: Connect's per-portal wait observes the canceled
		// context right after the login attempt.
		inner := newISCSIConnector
		newISCSIConnector = func() *iscsi.Connector {
			c := inner()
			baseExec := c.Exec
			c.Exec = func(name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), " -l") {
					cancel()
				}
				return baseExec(name, args...)
			}
			return c
		}
		staging := t.TempDir()
		_, err := env.driver.NodeStageVolume(ctx, env.stageRequest("volume-cancel-connect", staging, chainPortalA))
		if status.Code(err) != codes.Canceled {
			t.Fatalf("stage error code = %s, want Canceled (err=%v)", status.Code(err), err)
		}
		env.assertStateAssociation(t, "volume-cancel-connect", chainPortalA)
	})

	t.Run("canceled context during mapped-device verification", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &chainHost{}
		env := newChainEnv(t, host)
		env.chainDevice(t, chainPortalA)
		// The device exists but no sysfs WWID: WaitForMappedDevice observes
		// the canceled context during identity verification.
		inner := newISCSIConnector
		newISCSIConnector = func() *iscsi.Connector {
			c := inner()
			baseExec := c.Exec
			c.Exec = func(name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), " -l") {
					cancel()
				}
				return baseExec(name, args...)
			}
			return c
		}
		staging := t.TempDir()
		_, err := env.driver.NodeStageVolume(ctx, env.stageRequest("volume-cancel-mapped", staging, chainPortalA))
		if status.Code(err) != codes.Canceled {
			t.Fatalf("stage error code = %s, want Canceled (err=%v)", status.Code(err), err)
		}
		env.assertStateAssociation(t, "volume-cancel-mapped", chainPortalA)
	})
}

// TestDeviceStageErrorMapping pins the connector-failure to CSI-status table
// directly, including wrapped and rollback-joined error chains.
func TestDeviceStageErrorMapping(t *testing.T) {
	rejected := &iscsi.LoginRejectedError{Portal: chainPortalA, IQN: chainIQN, ExitCode: 19, Err: chainExitError(19)}
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"wrapped cancellation", fmt.Errorf("wait for portal: %w", context.Canceled), codes.Canceled},
		{"wrapped deadline", fmt.Errorf("wait for portal: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"permanent login rejection", rejected, codes.FailedPrecondition},
		{"rejection joined with rollback failure", errors.Join(fmt.Errorf("login portal %s: %w", chainPortalA, rejected), errors.New("rollback logout portal: refused")), codes.FailedPrecondition},
		{"genuine wait timeout", fmt.Errorf("wait for portal %s: %w", chainPortalA, iscsi.ErrDeviceWaitTimeout), codes.DeadlineExceeded},
		{"invalid connector input", fmt.Errorf("%w: empty iSCSI portal", iscsi.ErrInvalidDeviceRequest), codes.InvalidArgument},
		{"transient login failure", fmt.Errorf("iscsiadm login: %w", chainExitError(11)), codes.Unavailable},
		{"unknown host failure", errors.New("unexpected host condition"), codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := deviceStageError("connect iSCSI target", tc.err)
			if status.Code(err) != tc.want {
				t.Fatalf("deviceStageError(%v) code = %s, want %s", tc.err, status.Code(err), tc.want)
			}
		})
	}
}
