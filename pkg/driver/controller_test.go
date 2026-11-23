package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	helper "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/helper"
	utils "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/utils"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	kubeutils "github.com/NGXStorage/csi-ngxstorage-utils/kubernetes"
	ngxnames "github.com/NGXStorage/csi-ngxstorage-utils/names"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

type fakeAttachmentLookup struct {
	remaining []string
	err       error
	calls     int
}

func (f *fakeAttachmentLookup) RemainingNodes(context.Context, string, string, string) ([]string, error) {
	f.calls++
	return f.remaining, f.err
}

// newTestDriver builds a Driver whose SDK client points at a mock NGX API.
// The mock routes cluster status and pool list (for RefreshController) plus
// the operation-specific responses supplied by handler.
func newTestDriver(t *testing.T, mockHandler http.HandlerFunc) *Driver {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default routing for controller initialization: cluster status,
		// pool list, pool detail (owner) and portal group list. The pool and
		// the portal group both belong to owner "o" so the startup ownership
		// check passes. Operation-specific paths delegate to the caller.
		switch {
		case strings.Contains(r.URL.Path, "/status/cluster"):
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool"):
			w.Write([]byte(`[{"id":"pool1","name":"pool1","owner":"o"}]`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool/pool1"):
			w.Write([]byte(`{"id":"pool1","name":"pool1","owner":"o"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/portal_group"):
			w.Write([]byte(`[{"id":"pg1","name":"pg1","owner":"o"}]`))
		default:
			mockHandler(w, r)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(req)
	})

	d, err := NewDriverWithHTTPClient(InputParameters{
		Ip:       "10.0.0.1",
		PoolName: "pool1",
		ApiKey:   "test-key",
		Endpoint: "http://127.0.0.1",
		NodeId:   "node-1",
		Role:     "controller",
	}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewDriverWithHTTPClient: %v", err)
	}
	d.coordination = newFakeCoordinator()
	d.attachmentLookup = &fakeAttachmentLookup{}
	return d
}

// roundTripperFunc adapts a func to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func createVolumeRequestForTest(name string, capacityBytes int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          name,
		CapacityRange: &csi.CapacityRange{RequiredBytes: capacityBytes},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		}},
	}
}

func TestCreateVolume(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/list"):
			// Idempotency check: no existing LUN with this name.
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			w.Write([]byte(`{"luns":[{"id":"lun-1","name":"pvcvol1"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		}
	})

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "vol1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// The volume ID embeds the backend LUN ID (SHA1), not the name.
	if resp.Volume.VolumeId != helper.VolumeID(volumeIDPrefix, "lun-1") {
		t.Fatalf("volume ID = %s, want %s", resp.Volume.VolumeId, helper.VolumeID(volumeIDPrefix, "lun-1"))
	}
	if resp.Volume.VolumeContext["lun_id"] != "lun-1" {
		t.Fatalf("lun_id = %s, want lun-1", resp.Volume.VolumeContext["lun_id"])
	}
}

func TestCreateVolumeConcurrentSameNameExactlyOneBackendCreate(t *testing.T) {
	var mu sync.Mutex
	exists := false
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			if exists {
				w.Write([]byte(`[{"id":"lun-1","name":"pvcvol1","size":1073741824}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			createCalls++
			exists = true
			w.Write([]byte(`{"luns":[{"id":"lun-1","name":"pvcvol1"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	start := make(chan struct{})
	responses := make(chan *csi.CreateVolumeResponse, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			resp, err := d.CreateVolume(context.Background(), createVolumeRequestForTest("vol1", 1<<30))
			responses <- resp
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent CreateVolume: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if got := (<-responses).GetVolume().GetVolumeId(); got != helper.VolumeID(volumeIDPrefix, "lun-1") {
			t.Fatalf("volume ID = %q, want %q", got, helper.VolumeID(volumeIDPrefix, "lun-1"))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if createCalls != 1 {
		t.Fatalf("backend create calls = %d, want exactly 1", createCalls)
	}
}

func TestCreateVolumeSameNameIncompatibleReturnsAlreadyExists(t *testing.T) {
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[{"id":"lun-1","name":"pvcvol1","size":1073741824}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			createCalls++
			w.Write([]byte(`{"luns":[{"id":"lun-2","name":"pvcvol1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := d.CreateVolume(context.Background(), createVolumeRequestForTest("vol1", 2<<30))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", status.Code(err), err)
	}
	if createCalls != 0 {
		t.Fatalf("backend create calls = %d, want 0", createCalls)
	}
}

func TestCreateVolumeRoundsRequiredCapacityUpToGiB(t *testing.T) {
	var requestedGB int64
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			requestedGB = int64(body["size"].(float64))
			w.Write([]byte(`{"luns":[{"id":"lun-1","name":"pvcvol1"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":2147483648}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	resp, err := d.CreateVolume(context.Background(), createVolumeRequestForTest("vol1", (1<<30)+(512<<20)))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if requestedGB != 2 {
		t.Fatalf("backend create size = %d GiB, want 2", requestedGB)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != 2<<30 {
		t.Fatalf("response capacity = %d, want 2GiB", got)
	}
}

func TestCreateVolumeRejectsUnsatisfiableCapacityLimitBeforeMutation(t *testing.T) {
	backendCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	req := createVolumeRequestForTest("vol1", (1<<30)+(512<<20))
	req.CapacityRange.LimitBytes = (1 << 30) + (768 << 20)

	_, err := d.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("code = %v, want OutOfRange (err=%v)", status.Code(err), err)
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d, want 0 for unsatisfiable limit", backendCalls)
	}
}

func TestCreateVolumeIdempotencyLookupFailureFailsClosed(t *testing.T) {
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			createCalls++
			w.Write([]byte(`{"luns":[{"id":"lun-1","name":"pvcvol1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := d.CreateVolume(context.Background(), createVolumeRequestForTest("vol1", 1<<30))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if createCalls != 0 {
		t.Fatalf("backend create calls = %d, want 0 after failed idempotency lookup", createCalls)
	}
}

func TestGetSnapshotAndControllerGetVolume(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			w.Write([]byte(`{"id":"snap-1","volume_id":"lun-1","created":"1785499878","size":"1073741824","ready":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":1073741824}`))
		default:
			http.NotFound(w, r)
		}
	})
	snapshot, err := d.GetSnapshot(context.Background(), &csi.GetSnapshotRequest{SnapshotId: "snap-1"})
	if err != nil || snapshot.GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Fatalf("GetSnapshot() = %#v, err=%v", snapshot, err)
	}
	volume, err := d.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")})
	if err != nil || volume.GetVolume().GetVolumeId() != helper.VolumeID(volumeIDPrefix, "lun-1") || volume.GetVolume().GetCapacityBytes() != 1<<30 {
		t.Fatalf("ControllerGetVolume() = %#v, err=%v", volume, err)
	}
}

func TestCreateVolumeAcceptsReadOnlyMany(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			w.Write([]byte(`{"luns":[{"id":"lun-rox","name":"pvcvolrox"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-rox"):
			w.Write([]byte(`{"id":"lun-rox","name":"pvcvolrox","size":1073741824}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		}
	})

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "vol-rox",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume ReadOnlyMany: %v", err)
	}
	if resp.GetVolume().GetVolumeId() != helper.VolumeID(volumeIDPrefix, "lun-rox") {
		t.Fatalf("volume ID = %s, want %s", resp.GetVolume().GetVolumeId(), helper.VolumeID(volumeIDPrefix, "lun-rox"))
	}
}

func TestDeleteVolumeIdempotent(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/vol1"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":5011,"error":"not found"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "vol1"),
	}); err != nil {
		t.Fatalf("DeleteVolume should be idempotent, got: %v", err)
	}
}

func TestDeleteVolumeFailsClosedWithActiveAttachment(t *testing.T) {
	deleteCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/lun/") {
			deleteCalls++
		}
		w.WriteHeader(http.StatusNotFound)
	})
	d.attachmentLookup = &fakeAttachmentLookup{remaining: []string{"node-2"}}

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if deleteCalls != 0 {
		t.Fatalf("backend delete calls = %d, want 0 with active attachment", deleteCalls)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	want := "volume:" + helper.VolumeID(volumeIDPrefix, "lun-1")
	if len(coord.acquireKeys) == 0 || coord.acquireKeys[len(coord.acquireKeys)-1] != want {
		t.Fatalf("coordination keys = %v, want volume lock %q", coord.acquireKeys, want)
	}
}

func TestDeleteVolumeFailsClosedWhenAttachmentLookupUnavailable(t *testing.T) {
	backendCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	d.attachmentLookup = nil

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d, want 0 when attachment lookup is unavailable", backendCalls)
	}
}

func TestDeleteVolumeRefusesISCSIMappedLUN(t *testing.T) {
	deleteCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[{"id":"t1","name":"legacy-target"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"legacy-target","luns":[{"id":"lun-1","number":"0"}]}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/lun/"):
			deleteCalls++
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if deleteCalls != 0 {
		t.Fatalf("backend delete calls = %d, want 0 for mapped LUN", deleteCalls)
	}
}

func TestDeleteVolumeHandlesStaleTargetListEntry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		detailHTTP int
		wantCode   codes.Code
		wantDelete int
	}{
		{name: "detail not found", detailHTTP: http.StatusNotFound, wantCode: codes.OK, wantDelete: 1},
		{name: "detail server error", detailHTTP: http.StatusInternalServerError, wantCode: codes.Unavailable, wantDelete: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleteCalls int
			lunDeleted := false
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
					if lunDeleted {
						w.WriteHeader(http.StatusNotFound)
						w.Write([]byte(`{"code":5011,"error":"not found"}`))
						return
					}
					w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
					w.Write([]byte(`[{"id":"stale-target","name":"old"}]`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/stale-target"):
					w.WriteHeader(tc.detailHTTP)
					if tc.detailHTTP == http.StatusNotFound {
						w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
					} else {
						w.Write([]byte(`{"code":5000,"error":"backend unavailable"}`))
					}
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list"):
					w.Write([]byte(`[]`))
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
					deleteCalls++
					lunDeleted = true
					w.Write([]byte(`{"success":true}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
					w.Write([]byte(`[]`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})

			_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")})
			if status.Code(err) != tc.wantCode || deleteCalls != tc.wantDelete {
				t.Fatalf("DeleteVolume: code=%v deleteCalls=%d err=%v, want code=%v deleteCalls=%d",
					status.Code(err), deleteCalls, err, tc.wantCode, tc.wantDelete)
			}
		})
	}
}

func TestCreateVolumeRequiresCapabilities(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"connected":1,"status":"Master"}`))
	})
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "vol1"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume without capabilities: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestControllerPublishVolume(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	targetCreated := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetCreated {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			// AuthGroups().Create → POST /api/v2/auth_group
			w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/target/iscsi"):
			// Create acknowledges success; the driver resolves its ID by name.
			targetCreated = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/portal_group":
			// PortalGroups().List → lightweight list (no listen_ips).
			w.Write([]byte(`[{"id":"pg1","name":"pg1"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			// PortalGroups().Get → detail with listen_ips.
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"1"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"` + targetName + `","target_iqn":"iqn.2000-01.example:tgt","luns":[{"id":"lun-1","name":"pvcvol1","number":"6"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"o"}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master"}`))
		}
	})

	resp, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeContext:    map[string]string{"fstype": "ext4"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	pc := resp.PublishContext
	if pc["target_iqn"] != "iqn.2000-01.example:tgt" {
		t.Fatalf("target_iqn = %q, want iqn.2000-01.example:tgt", pc["target_iqn"])
	}
	if pc["portals"] != "192.168.1.201" {
		t.Fatalf("portals = %q, want 192.168.1.201", pc["portals"])
	}
	if pc["lun_number"] != "6" {
		t.Fatalf("lun_number = %q, want 6", pc["lun_number"])
	}
	if pc["scsi_id"] != "3600" {
		t.Fatalf("scsi_id = %q, want 3600", pc["scsi_id"])
	}
}

func TestControllerPublishVolumeCreatesAuthGroupWithPoolOwner(t *testing.T) {
	var authGroupBody map[string]interface{}
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	targetCreated := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetCreated {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &authGroupBody)
			w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/target/iscsi"):
			targetCreated = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/portal_group":
			w.Write([]byte(`[{"id":"pg1","name":"pg1"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"o"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"target","target_iqn":"iqn.2000-01.example:tgt","luns":[{"id":"lun-1","name":"pvcvol1","number":"6"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"o"}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master"}`))
		}
	})

	_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeContext:    map[string]string{"fstype": "ext4"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	// Regression: the backend rejects a target whose auth group belongs to a
	// different controller (code 661). The auth group must therefore be
	// created with the pool owner, not a hardcoded controller token.
	if got := authGroupBody["owner"]; got != "o" {
		t.Fatalf("auth group create owner = %v, want pool owner %q", got, "o")
	}
}

func TestControllerPublishVolumeReusesExistingTargetAuthGroup(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var authCreateCalls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"` + targetName + `","owner":"o","target_iqn":"iqn.2000-01.example:tgt","auth_group_id":"ag-existing","luns":[{"id":"lun-1","number":"6"}]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			authCreateCalls++
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"ip_address":"192.168.1.201"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","scsi_id":"3600"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	resp, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-2",
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	if authCreateCalls != 0 {
		t.Fatalf("auth group create calls = %d, want 0 for existing target", authCreateCalls)
	}
	if got := resp.GetPublishContext()["auth_group"]; got != "ag-existing" {
		t.Fatalf("publish auth_group = %q, want existing group ag-existing", got)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	wantKey := "volume:" + helper.VolumeID(volumeIDPrefix, "lun-1")
	if len(coord.acquireKeys) != 1 || coord.acquireKeys[0] != wantKey {
		t.Fatalf("coordination keys = %v, want [%q]", coord.acquireKeys, wantKey)
	}
}

func TestControllerPublishVolumeRejectsExistingTargetOwnerMismatch(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var addLUNCalls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"` + targetName + `","owner":"other-controller","auth_group_id":"ag-existing"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			addLUNCalls++
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"ip_address":"192.168.1.201"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"o"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	if addLUNCalls != 0 {
		t.Fatalf("AddLUN calls = %d, want 0 for mismatched existing target owner", addLUNCalls)
	}
}

func TestControllerPublishVolumeConcurrentSameVolumeCreatesTargetAndAuthGroupOnce(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var mu sync.Mutex
	groupExists := false
	targetExists := false
	authCreateCalls := 0
	targetCreateCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetExists {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			if groupExists {
				w.Write([]byte(`[{"id":"ag-1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			authCreateCalls++
			groupExists = true
			w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			targetCreateCalls++
			targetExists = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"` + targetName + `","owner":"o","target_iqn":"iqn.2000-01.example:tgt","auth_group_id":"ag-1","luns":[{"id":"lun-1","number":"6"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"ip_address":"192.168.1.201"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","scsi_id":"3600"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	}
	start := make(chan struct{})
	type result struct {
		resp *csi.ControllerPublishVolumeResponse
		err  error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			resp, err := d.ControllerPublishVolume(context.Background(), req)
			results <- result{resp: resp, err: err}
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent publish %d: %v", i, got.err)
		}
		if got.resp.GetPublishContext()["auth_group"] != "ag-1" {
			t.Fatalf("concurrent publish %d auth_group = %q, want ag-1", i, got.resp.GetPublishContext()["auth_group"])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if authCreateCalls != 1 || targetCreateCalls != 1 {
		t.Fatalf("create calls: auth_group=%d target=%d, want exactly 1 each", authCreateCalls, targetCreateCalls)
	}
}

func TestRegisterInitiatorIQNUsesVolumeLease(t *testing.T) {
	addCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/auth_group/iqn/auth-1") {
			addCalls++
			w.Write([]byte(`{"id":"iqn-1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := d.initiatorRegistrar().Register(context.Background(), helper.VolumeID(volumeIDPrefix, "lun-1"), "auth-1", "iqn.test:node1", "node1"); err != nil {
		t.Fatalf("registerInitiatorIQN: %v", err)
	}
	if addCalls != 1 {
		t.Fatalf("AddIQN calls = %d, want 1", addCalls)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	want := "volume:" + helper.VolumeID(volumeIDPrefix, "lun-1")
	if len(coord.acquireKeys) == 0 || coord.acquireKeys[len(coord.acquireKeys)-1] != want {
		t.Fatalf("coordination keys = %v, want final key %q", coord.acquireKeys, want)
	}
}

func TestControllerExpandVolumeSendsIncrement(t *testing.T) {
	var extendSize int64
	var sizeBytes int64 = 1 << 30
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(fmt.Sprintf(`{"id":"lun-1","name":"pvcvol1","size":%d}`, sizeBytes)))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode expand request: %v", err)
			}
			extendSize = int64(body["size"].(float64))
			sizeBytes += extendSize << 30
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	resp, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if extendSize != 1 {
		t.Fatalf("backend extend size = %d GiB, want 1 GiB increment", extendSize)
	}
	if resp.GetCapacityBytes() != 2<<30 || !resp.GetNodeExpansionRequired() {
		t.Fatalf("response = %+v", resp)
	}
}

func TestControllerExpandVolumeConcurrentSameTargetDoesNotDoubleGrow(t *testing.T) {
	var mu sync.Mutex
	sizeBytes := int64(1 << 30)
	patchCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			mu.Lock()
			current := sizeBytes
			mu.Unlock()
			w.Write([]byte(fmt.Sprintf(`{"id":"lun-1","name":"pvcvol1","size":%d}`, current)))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode expand request: %v", err)
				return
			}
			increment := int64(body["size"].(float64))
			mu.Lock()
			patchCalls++
			sizeBytes += increment << 30
			mu.Unlock()
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	req := &csi.ControllerExpandVolumeRequest{
		VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := d.ControllerExpandVolume(context.Background(), req)
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if sizeBytes != 2<<30 {
		t.Fatalf("final size = %d, want exactly 2GiB", sizeBytes)
	}
	if patchCalls != 1 {
		t.Fatalf("expand PATCH calls = %d, want exactly 1", patchCalls)
	}
}

func TestControllerUnpublishFetchesTargetDetail(t *testing.T) {
	var unmapped, targetDeleted, authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"target"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"target","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"target","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:   "node-1",
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if !unmapped || !targetDeleted || authGroupDeleted {
		t.Fatalf("legacy cleanup calls: unmap=%v target_delete=%v auth_group_delete=%v (legacy auth group must be retained)",
			unmapped, targetDeleted, authGroupDeleted)
	}
}

func TestControllerUnpublishDeletesOwnedVolumeScopedAuthGroup(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var authGroupDeleteCalls int
	var unmapped, targetDeleted, authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			if authGroupDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"auth group no longer exists"}`))
				return
			}
			w.Write([]byte(`{"id":"auth-1","name":"` + targetName + `"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			authGroupDeleteCalls++
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if authGroupDeleteCalls != 1 {
		t.Fatalf("auth group delete calls = %d, want exactly 1", authGroupDeleteCalls)
	}
}

func TestControllerUnpublishSkipsStaleTargetListEntries(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var staleTargetGets, targetDeleted, authGroupDeleted int
	var unmapped, authDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			// The lightweight list keeps the stale entry even after the real
			// target is deleted; detail reads are authoritative.
			w.Write([]byte(`[{"id":"stale-target"},{"id":"target-1"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/stale-target"):
			staleTargetGets++
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted > 0 {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted++
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group/auth-1"):
			if authDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"auth group no longer exists"}`))
				return
			}
			w.Write([]byte(`{"id":"auth-1","name":"` + targetName + `"}`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted++
			authDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume with stale target list entry: %v", err)
	}
	if staleTargetGets != 2 {
		t.Fatalf("stale target detail reads = %d, want one during target lookup and one during auth-group reference check", staleTargetGets)
	}
	if targetDeleted != 1 || authGroupDeleted != 1 {
		t.Fatalf("cleanup calls: target delete=%d auth group delete=%d, want exactly one each", targetDeleted, authGroupDeleted)
	}
}

func TestDeleteVolumeOrphanCleanupSkipsDeleteAfterOwnershipLoss(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":5011,"error":"not found"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// The initial acquisition assertion succeeds; the fresh assertion taken
	// immediately before the destructive auth-group delete must fail.
	d.coordination.(*fakeCoordinator).failAssertAt.Store(2)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if authGroupDeleted {
		t.Fatal("auth group delete must not run after ownership loss")
	}
}

func TestControllerUnpublishOrphanCleanupSkipsDeleteAfterOwnershipLoss(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	d.coordination.(*fakeCoordinator).failAssertAt.Store(2)

	_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volume, NodeId: "node-1",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if authGroupDeleted {
		t.Fatal("auth group delete must not run after ownership loss")
	}
}

func TestControllerUnpublishNormalCleanupSkipsDeleteAfterOwnershipLoss(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	var targetDeleted, authGroupDeleted bool
	var unmapped bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			w.Write([]byte(`{"id":"auth-1","name":"` + targetName + `"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// Assertions: 1) initial, 2) before the unmount/target delete, 3) fresh
	// assertion added immediately before the owned auth-group delete.
	d.coordination.(*fakeCoordinator).failAssertAt.Store(3)

	_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if !targetDeleted {
		t.Fatal("target delete must have happened before the auth group cleanup")
	}
	if authGroupDeleted {
		t.Fatal("auth group delete must not run after ownership loss")
	}
}

func TestDeleteVolumeCleansOrphanedVolumeScopedAuthGroup(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var lunDeleted, authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if lunDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
				return
			}
			w.Write([]byte(`{"id":"lun-1","name":"vol1","protocols":{}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			lunDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if !lunDeleted || !authGroupDeleted {
		t.Fatalf("cleanup calls: lun_delete=%v auth_group_delete=%v", lunDeleted, authGroupDeleted)
	}
}

func TestDeleteVolumeCleansOrphanedAuthGroupWhenLUNAlreadyAbsent(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":5011,"error":"not found"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if !authGroupDeleted {
		t.Fatal("orphaned volume-scoped auth group was not deleted when the LUN was already absent")
	}
}

func TestDeleteVolumeRetainsAuthGroupReferencedByOtherTarget(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var lunDeleted, authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if lunDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
				return
			}
			w.Write([]byte(`{"id":"lun-1","name":"vol1","protocols":{}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[{"id":"t1","name":"other"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"other","auth_group_id":"auth-1"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			lunDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volume}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if authGroupDeleted {
		t.Fatal("auth group still referenced by another target must be retained")
	}
}

func TestControllerUnpublishCleansOrphanedAuthGroupWhenTargetAlreadyGone(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	groupName := ngxnames.ModifyVolName(volume)
	var authGroupDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[{"id":"auth-1","name":"` + groupName + `"}]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authGroupDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volume, NodeId: "node-1",
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if !authGroupDeleted {
		t.Fatal("orphaned volume-scoped auth group was not cleaned after the target was already gone")
	}
}

func TestControllerUnpublishFailsClosedOnAttachmentLookupError(t *testing.T) {
	var targetDeleteCalls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/target/iscsi/") {
			targetDeleteCalls++
		}
		w.WriteHeader(http.StatusNotFound)
	})
	d.attachmentLookup = &fakeAttachmentLookup{err: errors.New("apiserver unavailable")}

	_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("status = %s, want Unavailable; err=%v", status.Code(err), err)
	}
	if targetDeleteCalls != 0 {
		t.Fatalf("target delete calls = %d, want 0 on Kubernetes API uncertainty", targetDeleteCalls)
	}
}

func TestControllerUnpublishFailsClosedOnAmbiguousPV(t *testing.T) {
	var targetDeleteCalls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/target/iscsi/") {
			targetDeleteCalls++
		}
		w.WriteHeader(http.StatusNotFound)
	})
	d.attachmentLookup = &fakeAttachmentLookup{err: kubeutils.ErrAmbiguousVolumePV{Count: 2}}

	_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	if targetDeleteCalls != 0 {
		t.Fatalf("target delete calls = %d, want 0 for ambiguous PV identity", targetDeleteCalls)
	}
}

func TestInitiatorRegistrationSerializesWithLastUnpublish(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	addStarted := make(chan struct{})
	allowAdd := make(chan struct{})
	var addOnce sync.Once
	var stateMu sync.Mutex
	targetDeleted := false
	unmapped := false

	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/auth_group/iqn/auth-1"):
			addOnce.Do(func() { close(addStarted) })
			<-allowAdd
			w.Write([]byte(`{"id":"iqn-1"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			stateMu.Lock()
			gone := targetDeleted
			stateMu.Unlock()
			if gone {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			stateMu.Lock()
			gone, unmap := targetDeleted, unmapped
			stateMu.Unlock()
			if gone {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmap {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			stateMu.Lock()
			unmap := unmapped
			stateMu.Unlock()
			if unmap {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			stateMu.Lock()
			unmapped = true
			stateMu.Unlock()
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			stateMu.Lock()
			targetDeleted = true
			stateMu.Unlock()
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			// Legacy name intentionally prevents automatic auth-group deletion.
			w.Write([]byte(`{"id":"auth-1","name":"legacy-node-group"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	d.attachmentLookup = &fakeAttachmentLookup{}

	registerErr := make(chan error, 1)
	go func() {
		registerErr <- d.initiatorRegistrar().Register(context.Background(), helper.VolumeID(volumeIDPrefix, "lun-1"), "auth-1", "iqn.test:node2", "node2")
	}()
	select {
	case <-addStarted:
	case <-time.After(time.Second):
		t.Fatal("initiator registration did not enter backend mutation")
	}

	unpublishErr := make(chan error, 1)
	go func() {
		_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
		})
		unpublishErr <- err
	}()

	coord := d.coordination.(*fakeCoordinator)
	deadline := time.Now().Add(time.Second)
	for {
		coord.recordMu.Lock()
		attempts := len(coord.acquireKeys)
		coord.recordMu.Unlock()
		if attempts >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unpublish did not attempt the shared volume lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stateMu.Lock()
	deletedWhileAddActive := targetDeleted
	stateMu.Unlock()
	if deletedWhileAddActive {
		t.Fatal("target deleted while AddIQN still held the volume Lease")
	}

	close(allowAdd)
	if err := <-registerErr; err != nil {
		t.Fatalf("registerInitiatorIQN: %v", err)
	}
	if err := <-unpublishErr; err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if !targetDeleted {
		t.Fatal("last unpublish did not delete target after AddIQN released the volume Lease")
	}
}

func TestCreateVolumeFromSnapshotReturnsContentSource(t *testing.T) {
	cloneName := ngxnames.ModifyVolName("restore")
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			w.Write([]byte(`{"id":"snap-1","name":"snap-source","volume_id":"lun-source","volume_name":"source-volume"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api/v2/snapshot/clone/snap-1"):
			w.Write([]byte(`{"volume_id":"lun-clone"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-clone"):
			w.Write([]byte(`{"id":"lun-clone","name":"` + cloneName + `","size":1073741824,"clone":"snap-1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	source := &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
		},
	}
	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "restore",
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeContentSource: source,
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.GetVolume().GetContentSource().GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Fatalf("content source = %+v, want snapshot snap-1", resp.GetVolume().GetContentSource())
	}
}

func TestCreateSnapshotReturnsMetadata(t *testing.T) {
	snapshotName := ngxnames.ModifyVolName("snapshot-1")
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":"1073741824"}`))
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/api/v2/snapshot") || strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list")):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api/v2/snapshot"):
			w.Write([]byte(`{"id":"snap-1","name":"` + snapshotName + `","volume_id":"lun-1","created":"1700000000"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			w.Write([]byte(`{"id":"snap-1","name":"` + snapshotName + `","volume_id":"lun-1","created":"1700000000","size":"1073741824","ready":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	resp, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snapshot-1",
		SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap := resp.GetSnapshot()
	if snap.GetSizeBytes() != 1<<30 || snap.GetCreationTime().GetSeconds() != 1700000000 {
		t.Fatalf("snapshot metadata = %+v", snap)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	if len(coord.multiKeys) != 1 {
		t.Fatalf("AcquireMany calls = %d, want 1", len(coord.multiKeys))
	}
	gotLocks := map[string]bool{}
	for _, key := range coord.multiKeys[0] {
		gotLocks[key] = true
	}
	for _, want := range []string{"snapshot-name:" + snapshotName, "volume:" + helper.VolumeID(volumeIDPrefix, "lun-1")} {
		if !gotLocks[want] {
			t.Fatalf("snapshot lock set = %v, missing %q", coord.multiKeys[0], want)
		}
	}
}

func TestCreateSnapshotConcurrentSameNameSourceExactlyOneBackendCreate(t *testing.T) {
	const canonicalName = "pvcsnapshot1"
	var mu sync.Mutex
	exists := false
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":1073741824}`))
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v2/snapshot" || r.URL.Path == "/api/v2/snapshot/list"):
			if exists {
				w.Write([]byte(`[{"id":"snap-1","name":"` + canonicalName + `","volume_id":"lun-1","reserved":"1073741824","created":"1700000000","ready":true}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/snapshot":
			createCalls++
			exists = true
			w.Write([]byte(`{"id":"snap-1","name":"` + canonicalName + `","volume_id":"lun-1"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			w.Write([]byte(`{"id":"snap-1","name":"` + canonicalName + `","volume_id":"lun-1","reserved":"1073741824","created":"1700000000","ready":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	req := &csi.CreateSnapshotRequest{Name: "snapshot-1", SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1")}
	start := make(chan struct{})
	responses := make(chan *csi.CreateSnapshotResponse, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			resp, err := d.CreateSnapshot(context.Background(), req)
			responses <- resp
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent CreateSnapshot: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if got := (<-responses).GetSnapshot().GetSnapshotId(); got != "snap-1" {
			t.Fatalf("snapshot ID = %q, want snap-1", got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if createCalls != 1 {
		t.Fatalf("backend snapshot create calls = %d, want exactly 1", createCalls)
	}
}

func TestCreateSnapshotSameNameDifferentSourceReturnsAlreadyExists(t *testing.T) {
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-2"):
			w.Write([]byte(`{"id":"lun-2","size":1073741824}`))
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v2/snapshot" || r.URL.Path == "/api/v2/snapshot/list"):
			w.Write([]byte(`[{"id":"snap-1","name":"pvcsnapshot1","volume_id":"lun-1"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/snapshot":
			createCalls++
			w.Write([]byte(`{"id":"snap-2"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snapshot-1", SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-2"),
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err=%v)", status.Code(err), err)
	}
	if createCalls != 0 {
		t.Fatalf("backend snapshot create calls = %d, want 0", createCalls)
	}
}

func TestDeleteSnapshotIdempotentAndLocked(t *testing.T) {
	deleteCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-missing"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":5011,"error":"not found"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/snapshot/"):
			deleteCalls++
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-missing"}); err != nil {
		t.Fatalf("DeleteSnapshot idempotent missing: %v", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("backend delete calls = %d, want 0 for missing snapshot", deleteCalls)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	found := false
	for _, key := range coord.acquireKeys {
		if key == "snapshot:snap-missing" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DeleteSnapshot did not acquire snapshot:snap-missing, keys=%v", coord.acquireKeys)
	}
}

func TestCloneHoldsSnapshotLockAgainstDeleteSnapshot(t *testing.T) {
	cloneEntered := make(chan struct{})
	allowClone := make(chan struct{})
	deleteBackendEntered := make(chan struct{}, 1)
	snapshotDeleted := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			select {
			case deleteBackendEntered <- struct{}{}:
			default:
			}
			if snapshotDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
				return
			}
			w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","volume_name":"src"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api/v2/snapshot/clone/snap-1"):
			close(cloneEntered)
			<-allowClone
			w.Write([]byte(`{"volume_id":"lun-clone"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-clone"):
			w.Write([]byte(`{"id":"lun-clone","name":"pvcrestore","size":1073741824,"clone":"src@snap1"}`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			select {
			case deleteBackendEntered <- struct{}{}:
			default:
			}
			snapshotDeleted = true
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	coord := d.coordination.(*fakeCoordinator)

	source := &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
	}}
	cloneReq := createVolumeRequestForTest("restore", 1<<30)
	cloneReq.VolumeContentSource = source
	cloneErr := make(chan error, 1)
	go func() {
		_, err := d.CreateVolume(context.Background(), cloneReq)
		cloneErr <- err
	}()

	select {
	case <-cloneEntered:
	case <-time.After(time.Second):
		t.Fatal("clone did not reach backend")
	}
	// Drain the clone's source-snapshot GET observation; any subsequent backend
	// entry before allowClone is released would belong to DeleteSnapshot.
	select {
	case <-deleteBackendEntered:
	default:
	}

	deleteErr := make(chan error, 1)
	go func() {
		_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		deleteErr <- err
	}()

	for {
		select {
		case key := <-coord.attempts:
			if key == "snapshot:snap-1" {
				goto deleteWaiting
			}
		case <-time.After(time.Second):
			t.Fatal("DeleteSnapshot did not attempt snapshot:snap-1 lock")
		}
	}

deleteWaiting:
	select {
	case <-deleteBackendEntered:
		t.Fatal("DeleteSnapshot reached backend while clone still held snapshot lock")
	default:
	}
	close(allowClone)
	if err := <-cloneErr; err != nil {
		t.Fatalf("CreateVolume clone: %v", err)
	}
	if err := <-deleteErr; err != nil {
		t.Fatalf("DeleteSnapshot after clone: %v", err)
	}
}

func TestListSnapshotsFiltersSourceAndPaginates(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/snapshot") {
			w.Write([]byte(`[{"id":"s1","volume_id":"lun-1","reserved":"1073741824","created":"1700000000","ready":true},{"id":"s2","volume_id":"lun-2"},{"id":"s3","volume_id":"lun-1"}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), MaxEntries: 1,
	})
	if err != nil || len(resp.GetEntries()) != 1 || resp.GetEntries()[0].GetSnapshot().GetSnapshotId() != "s1" || resp.GetEntries()[0].GetSnapshot().GetSourceVolumeId() != helper.VolumeID(volumeIDPrefix, "lun-1") || resp.GetEntries()[0].GetSnapshot().GetSizeBytes() != 1<<30 || resp.GetNextToken() != "s1:s1" {
		t.Fatalf("first page = %+v, err=%v", resp, err)
	}
	resp, err = d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), StartingToken: "s1:s1", MaxEntries: 1,
	})
	if err != nil || len(resp.GetEntries()) != 1 || resp.GetEntries()[0].GetSnapshot().GetSnapshotId() != "s3" || resp.GetNextToken() != "" {
		t.Fatalf("second page = %+v, err=%v", resp, err)
	}
}

func TestListSnapshotsRejectsNegativeMaxEntries(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	_, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{MaxEntries: -1})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code=%v, want InvalidArgument", status.Code(err))
	}
}

func TestListSnapshotsRejectsPastEndToken(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"s1","volume_id":"lun-1"}]`))
	})
	_, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: "2"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("code=%v, want Aborted", status.Code(err))
	}
}

func TestListVolumesPaginates(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list") {
			w.Write([]byte(`[{"id":"l1","size":2147483648},{"id":"l2","size":4294967296}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	resp, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 1})
	if err != nil || len(resp.GetEntries()) != 1 || resp.GetNextToken() != "v1:"+helper.VolumeID(volumeIDPrefix, "l1") {
		t.Fatalf("page=%+v err=%v", resp, err)
	}
	if got := resp.GetEntries()[0].GetVolume().GetCapacityBytes(); got != 2147483648 {
		t.Fatalf("capacity=%d, want 2147483648", got)
	}
}

func TestControllerPublishVolumeContextOwnerTakesPrecedence(t *testing.T) {
	var createOwner, addLUNOwner string
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	targetCreated := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetCreated {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/portal_group":
			w.Write([]byte(`[{"id":"pg1","name":"pg1","owner":"SGZ1RVC2_A"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"SGZ1RVC2_A"}]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/target/iscsi"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode target create: %v", err)
			}
			createOwner, _ = body["owner"].(string)
			targetCreated = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode AddLUN: %v", err)
			}
			addLUNOwner, _ = body["owner"].(string)
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"target","target_iqn":"iqn.2000-01.example:tgt","luns":[{"id":"lun-1","name":"pvcvol1","number":"6"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"SGZ1RVC2_A"}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master"}`))
		}
	})

	_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeContext:    map[string]string{"owner": "SGZ1RVC2_A", "fstype": "ext4"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	if createOwner != "SGZ1RVC2_A" {
		t.Fatalf("target create owner = %q, want SGZ1RVC2_A (context takes precedence over pool owner)", createOwner)
	}
	if addLUNOwner != "SGZ1RVC2_A" {
		t.Fatalf("AddLUN owner = %q, want SGZ1RVC2_A", addLUNOwner)
	}
}

func TestControllerPublishVolumeUsesPoolOwnerWhenContextMissing(t *testing.T) {
	var createOwner, addLUNOwner string
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	targetCreated := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetCreated {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/portal_group":
			w.Write([]byte(`[{"id":"pg1","name":"pg1","owner":"SGZ1RVC2_A"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/portal_group/"):
			w.Write([]byte(`{"id":"pg1","name":"pg1","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"SGZ1RVC2_A"}]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/target/iscsi"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode target create: %v", err)
			}
			createOwner, _ = body["owner"].(string)
			targetCreated = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode AddLUN: %v", err)
			}
			addLUNOwner, _ = body["owner"].(string)
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"target","target_iqn":"iqn.2000-01.example:tgt","luns":[{"id":"lun-1","name":"pvcvol1","number":"6"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"o"}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master"}`))
		}
	})

	_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeContext:    map[string]string{"fstype": "ext4"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	if createOwner != "o" {
		t.Fatalf("target create owner = %q, want %q (pool owner)", createOwner, "o")
	}
	if addLUNOwner != "o" {
		t.Fatalf("AddLUN owner = %q, want %q (pool owner)", addLUNOwner, "o")
	}
}

func TestControllerUnpublishVolumeUsesPoolOwner(t *testing.T) {
	var removeOwner string
	var unmapped, targetDeleted bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"target"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"target","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"target","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode RemoveLUN: %v", err)
			}
			removeOwner, _ = body["owner"].(string)
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:   "node-1",
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if removeOwner != "o" {
		t.Fatalf("RemoveLUN owner = %q, want %q (pool owner)", removeOwner, "o")
	}
}

func TestControllerUnpublishRetainsTargetForOtherNodes(t *testing.T) {
	var removeCalls int
	var unmapped, targetDeleted bool
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","target_iqn":"iqn.test:vol1","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","target_iqn":"iqn.test:vol1","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			removeCalls++
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	d.attachmentLookup = &fakeAttachmentLookup{remaining: []string{"node-2"}}

	unpublish := func() {
		if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
			NodeId:   "node-1",
		}); err != nil {
			t.Fatalf("ControllerUnpublishVolume: %v", err)
		}
	}

	// node-2 still has a Kubernetes VolumeAttachment (MULTI_NODE_READER_ONLY):
	// the target must be retained regardless of backend session timing.
	unpublish()
	if removeCalls != 0 {
		t.Fatalf("RemoveLUN calls = %d, want 0 while node-2 still holds the target", removeCalls)
	}

	// Last attachment is gone: the final unpublish may delete.
	d.attachmentLookup = &fakeAttachmentLookup{remaining: nil}
	unpublish()
	if removeCalls != 1 {
		t.Fatalf("RemoveLUN calls = %d, want 1 after the last node unpublishes", removeCalls)
	}
}

func TestControllerModifyVolumeUsesVolumeLockAndVerifiesReadback(t *testing.T) {
	dedup := "off"
	patchCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(fmt.Sprintf(`{"id":"lun-1","deduplication":%q}`, dedup)))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode modify request: %v", err)
			}
			if got := fmt.Sprint(body["deduplication"]); got != "on" {
				t.Fatalf("deduplication patch = %q, want on", got)
			}
			patchCalls++
			dedup = "on"
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := d.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		MutableParameters: map[string]string{
			"deduplication": "true",
		},
	}); err != nil {
		t.Fatalf("ControllerModifyVolume: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("modify PATCH calls = %d, want 1", patchCalls)
	}
	coord := d.coordination.(*fakeCoordinator)
	coord.recordMu.Lock()
	defer coord.recordMu.Unlock()
	found := false
	for _, key := range coord.acquireKeys {
		if key == "volume:"+helper.VolumeID(volumeIDPrefix, "lun-1") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ControllerModifyVolume did not acquire volume lock, keys=%v", coord.acquireKeys)
	}
}

func TestControllerModifyVolumeFailsClosedWithoutCoordinator(t *testing.T) {
	backendCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	d.coordination = nil

	_, err := d.ControllerModifyVolume(context.Background(), &csi.ControllerModifyVolumeRequest{
		VolumeId:          helper.VolumeID(volumeIDPrefix, "lun-1"),
		MutableParameters: map[string]string{"deduplication": "true"},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d, want 0 without coordinator", backendCalls)
	}
}

func TestDeleteSnapshotFailsClosedWithoutCoordinator(t *testing.T) {
	backendCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	d.coordination = nil

	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d, want 0 without coordinator", backendCalls)
	}
}

// LUN/target reconciliation tests (relocated from reconcile_test.go).
// reconcileMockServer serves the controller-initialization and refresh
// endpoints and delegates everything else to the test handler.
func reconcileMockServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/status/cluster"):
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool"):
			w.Write([]byte(`[{"id":"pool1","name":"pool1","owner":"o"}]`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool/pool1"):
			w.Write([]byte(`{"id":"pool1","name":"pool1","owner":"o"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/portal_group"):
			w.Write([]byte(`[{"id":"pg1","name":"pg1","owner":"o"}]`))
		default:
			handler(w, r)
		}
	})
	server := httptest.NewServer(wrap)
	t.Cleanup(server.Close)
	return server
}

// reconcileTransport fails the first matching request with a transport error
// and forwards everything else to the mock server.
func reconcileTransport(server *httptest.Server, fail func(*http.Request) bool) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if fail != nil && fail(req) {
			return nil, fmt.Errorf("read tcp: EOF")
		}
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(req)
	})
}

func newReconcileDriver(t *testing.T, transport http.RoundTripper) *Driver {
	t.Helper()
	d, err := NewDriverWithHTTPClient(InputParameters{
		Ip:       "10.0.0.1",
		PoolName: "pool1",
		ApiKey:   "test-key",
		Endpoint: "http://127.0.0.1",
		NodeId:   "node-1",
		Role:     "controller",
	}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewDriverWithHTTPClient: %v", err)
	}
	d.coordination = newFakeCoordinator()
	d.attachmentLookup = &fakeAttachmentLookup{}
	return d
}

// A dropped create response may still have created the name-keyed LUN; the
// helper must resolve it by name instead of failing or creating a duplicate.
func TestCreateLUNReconciledAfterTransportFailure(t *testing.T) {
	posts := 0
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[{"id":"lun-1","name":"vol","size":1073741824}]`))
		default:
			http.NotFound(w, r)
		}
	})
	fail := true
	transport := reconcileTransport(server, func(req *http.Request) bool {
		if fail && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/api/v2/lun/") {
			fail = false
			posts++
			return true
		}
		return false
	})
	d := newReconcileDriver(t, transport)

	lun, err := d.reconciler().CreateLUNReconciled(context.Background(), "vol", 1, ngxstorage.LUNCreateOptions{})
	if err != nil {
		t.Fatalf("createLUNReconciled: %v", err)
	}
	if lun == nil || lun.ID != "lun-1" {
		t.Fatalf("reconciled LUN = %+v, want id lun-1", lun)
	}
	if posts != 1 {
		t.Fatalf("failed create POST count = %d, want 1 (no blind duplicate create)", posts)
	}
}

// A dropped unmap PATCH may already have applied; an absent mapping on the
// re-read target is success.
func TestRemoveLUNReconciledWhenMappingApplied(t *testing.T) {
	patches := 0
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/tgt-1"):
			w.Write([]byte(`{"id":"tgt-1","name":"target","luns":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
	fail := true
	transport := reconcileTransport(server, func(req *http.Request) bool {
		if fail && req.Method == http.MethodPatch && strings.Contains(req.URL.Path, "/api/v2/target/iscsi/lun/") {
			fail = false
			patches++
			return true
		}
		return false
	})
	d := newReconcileDriver(t, transport)

	if err := d.reconciler().RemoveLUNReconciled(context.Background(), "tgt-1", "lun-1"); err != nil {
		t.Fatalf("removeLUNReconciled: %v", err)
	}
	if patches != 1 {
		t.Fatalf("failed unmap PATCH count = %d, want 1", patches)
	}
}

// A dropped registration PATCH may already have applied; a present IQN on
// the re-read auth group is success.
func TestRegisterIQNReconciledWhenIQNApplied(t *testing.T) {
	patches := 0
	const iqn = "iqn.2026-01.com.ngxstorage:node1"
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group/ag-1"):
			w.Write([]byte(`{"id":"ag-1","name":"group","initiator":[{"initiator":"` + iqn + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	fail := true
	transport := reconcileTransport(server, func(req *http.Request) bool {
		if fail && req.Method == http.MethodPatch && strings.Contains(req.URL.Path, "/api/v2/auth_group/iqn/") {
			fail = false
			patches++
			return true
		}
		return false
	})
	d := newReconcileDriver(t, transport)

	if err := d.reconciler().RegisterIQNReconciled(context.Background(), "ag-1", iqn, "node1"); err != nil {
		t.Fatalf("registerIQNReconciled: %v", err)
	}
	if patches != 1 {
		t.Fatalf("failed register PATCH count = %d, want 1", patches)
	}
}

// TestRegisterIQNReconciledPassesPoolOwner pins the owner contract: the IQN
// registration must send d.poolOwner (the real pool owner), not a hardcoded
// controller token that fails the backend's "Length must be between 2 and 20" rule.
func TestRegisterIQNReconciledPassesPoolOwner(t *testing.T) {
	var capturedBody []byte
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/auth_group/iqn/") {
			body, _ := io.ReadAll(r.Body)
			capturedBody = body
		}
		w.Write([]byte(`{"success":true}`))
	})
	transport := reconcileTransport(server, func(*http.Request) bool { return false })
	d := newReconcileDriver(t, transport)
	d.poolOwner = "SGZ1RVC2_A"
	if err := d.reconciler().RegisterIQNReconciled(context.Background(), "ag-1", "iqn-test", "alias"); err != nil {
		t.Fatalf("registerIQNReconciled: %v", err)
	}
	if len(capturedBody) == 0 {
		t.Fatal("no PATCH body captured")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("unmarshal PATCH body: %v", err)
	}
	if got := payload["owner"]; got != "SGZ1RVC2_A" {
		t.Fatalf("owner = %v, want SGZ1RVC2_A", got)
	}
}

func TestTargetHasLUN(t *testing.T) {
	var target ngxstorage.ISCSITarget
	if err := json.Unmarshal([]byte(`{"id":"tgt","luns":[{"id":"lun-1"}]}`), &target); err != nil {
		t.Fatal(err)
	}
	if !helper.TargetHasLUN(&target, "lun-1") {
		t.Fatal("targetHasLUN = false, want true for a listed LUN")
	}
	if helper.TargetHasLUN(&target, "lun-2") {
		t.Fatal("targetHasLUN = true for an absent LUN")
	}
	if helper.TargetHasLUN(nil, "lun-1") {
		t.Fatal("helper.TargetHasLUN(nil) = true")
	}
}

// A dropped map-to-target PUT may already have applied; a present mapping on
// the re-read target is success.
func TestAddLUNReconciledWhenMappingApplied(t *testing.T) {
	puts := 0
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi/tgt-1"):
			w.Write([]byte(`{"id":"tgt-1","name":"target","luns":[{"id":"lun-1"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	fail := true
	transport := reconcileTransport(server, func(req *http.Request) bool {
		if fail && req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/api/v2/target/iscsi/lun/") {
			fail = false
			puts++
			return true
		}
		return false
	})
	d := newReconcileDriver(t, transport)

	if err := d.reconciler().AddLUNToTargetReconciled(context.Background(), "tgt-1", "lun-1", "owner"); err != nil {
		t.Fatalf("addLUNToTargetReconciled: %v", err)
	}
	if puts != 1 {
		t.Fatalf("failed map PUT count = %d, want 1", puts)
	}
}

// installFakeSleeper replaces the between-attempt backoff and counts calls.
func installFakeSleeper(t *testing.T, fn func(context.Context, int) error) *int {
	t.Helper()
	calls := new(int)
	original := utils.ReconcileSleeper
	utils.ReconcileSleeper = func(ctx context.Context, attempt int) error {
		*calls++
		if fn != nil {
			return fn(ctx, attempt)
		}
		return nil
	}
	t.Cleanup(func() { utils.ReconcileSleeper = original })
	return calls
}

func alwaysFailMapPUT(attempts *int) func(*http.Request) bool {
	return func(req *http.Request) bool {
		if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/api/v2/target/iscsi/lun/") {
			*attempts++
			return true
		}
		return false
	}
}

// Three attempts, exactly two sleeps: the final error returns immediately.
func TestReconcileNoSleepAfterFinalAttempt(t *testing.T) {
	sleeps := installFakeSleeper(t, nil)
	attempts := 0
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	transport := reconcileTransport(server, alwaysFailMapPUT(&attempts))
	d := newReconcileDriver(t, transport)

	if err := d.reconciler().AddLUNToTargetReconciled(context.Background(), "tgt-1", "lun-1", "owner"); err == nil {
		t.Fatal("addLUNToTargetReconciled succeeded, want transport failure")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if *sleeps != 2 {
		t.Fatalf("sleeps = %d, want 2 (no sleep after the final attempt)", *sleeps)
	}
}

func TestReconcileContextCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := installFakeSleeper(t, func(ctx context.Context, _ int) error {
		cancel()
		return ctx.Err()
	})
	attempts := 0
	server := reconcileMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	transport := reconcileTransport(server, alwaysFailMapPUT(&attempts))
	d := newReconcileDriver(t, transport)

	err := d.reconciler().AddLUNToTargetReconciled(ctx, "tgt-1", "lun-1", "owner")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (cancelled before a second attempt)", attempts)
	}
	if *sleeps != 1 {
		t.Fatalf("sleeps = %d, want 1", *sleeps)
	}
}

func TestLocalSanityLookupNodeExists(t *testing.T) {
	lookup := localSanityLookup{nodeID: "local-sanity"}
	for _, nodeID := range []string{"local-sanity", "unknown-node", ""} {
		t.Run(nodeID, func(t *testing.T) {
			exists, err := lookup.NodeExists(context.Background(), nodeID)
			want := nodeID == "local-sanity"
			if err != nil || exists != want {
				t.Fatalf("NodeExists(%q) = %v, %v; want %v, nil", nodeID, exists, err, want)
			}
		})
	}
}

type localSanityLookup struct{ nodeID string }

func (l localSanityLookup) NodeExists(_ context.Context, nodeID string) (bool, error) {
	return l.nodeID != "" && nodeID == l.nodeID, nil
}
func (localSanityLookup) RemainingNodes(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}

func localSanityErrorLogger(t *testing.T) *zap.Logger {
	t.Helper()
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
	logger, err := config.Build()
	if err != nil {
		t.Fatalf("create local sanity logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Sync() })
	return logger
}

// TestLocalSanityBackendPreflight verifies the live backend is ready before
// the shell runner asks for root privileges to access iSCSI devices.
func TestLocalSanityBackendPreflight(t *testing.T) {
	if os.Getenv("ISCSI_LIVE_MOCK_PREFLIGHT") != "1" {
		t.Skip("explicit backend preflight opt-in required")
	}
	_, err := NewDriver(InputParameters{
		Endpoint: "unix:///sanity-preflight.sock", Role: "all", NodeId: "local-sanity",
		Ip: os.Getenv("STORAGE_IPS"), PoolName: os.Getenv("STORAGE_POOL_NAME"),
		ApiKey: os.Getenv("STORAGE_API_KEY"), PortalGroup: os.Getenv("STORAGE_PORTAL_GROUP"),
	})
	if err != nil {
		t.Fatalf("live iSCSI backend preflight: %v", err)
	}
}

// TestLocalSanityWithMockCoordination runs csi-sanity against the live iSCSI
// backend with test-process-only coordination. It does not validate Lease
// behavior or protect against another driver process using the same backend.
func TestLocalSanityWithMockCoordination(t *testing.T) {
	if os.Getenv("ISCSI_LIVE_MOCK_SANITY") != "1" {
		t.Skip("explicit live mock sanity opt-in required")
	}
	focus := os.Getenv("SANITY_FOCUS_FILE")
	fullSuite := os.Getenv("SANITY_FULL") == "1"
	if focus == "" && !fullSuite {
		t.Fatal("SANITY_FOCUS_FILE must select one sanity spec")
	}
	if focus != "" && fullSuite {
		t.Fatal("set either SANITY_FOCUS_FILE or SANITY_FULL=1, not both")
	}
	sanityBin := os.Getenv("ISCSI_SANITY_BIN")
	configDir := os.Getenv("ISCSI_SANITY_CONFIG_DIR")
	if sanityBin == "" || configDir == "" {
		t.Fatal("ISCSI_SANITY_BIN and ISCSI_SANITY_CONFIG_DIR are required")
	}
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "iscsi-sanity-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "csi.sock")
	d, err := NewDriver(InputParameters{
		Endpoint: "unix://" + socket, Role: "all", NodeId: "local-sanity",
		Ip: os.Getenv("STORAGE_IPS"), PoolName: os.Getenv("STORAGE_POOL_NAME"),
		ApiKey: os.Getenv("STORAGE_API_KEY"), PortalGroup: os.Getenv("STORAGE_PORTAL_GROUP"),
		Logger: localSanityErrorLogger(t),
	})
	if err != nil {
		t.Fatalf("initialize live test driver: %v", err)
	}
	d.coordination = newFakeCoordinator()
	d.attachmentLookup = localSanityLookup{nodeID: d.NodeId}
	d.sessionStateDir = filepath.Join(dir, "iscsi-state")
	if err := d.prepareCoordination(context.Background()); err != nil {
		t.Fatalf("test coordination preflight: %v", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("test CSI socket: %v", err)
	}
	server := grpc.NewServer()
	d.Server = server
	csi.RegisterIdentityServer(server, d)
	csi.RegisterControllerServer(server, d)
	csi.RegisterNodeServer(server, d)
	d.Ready = true
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer server.Stop()
	waitForLocalSanitySocket(t, socket, serverErr)

	timeout := 60 * time.Second
	if fullSuite {
		timeout = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	skip := "GroupController"
	if extra := os.Getenv("SANITY_SKIP_TESTS"); extra != "" {
		skip += "|" + extra
	}
	args := []string{
		"--ginkgo.no-color", "--ginkgo.v", "--ginkgo.fail-on-empty",
		"--ginkgo.skip=" + skip,
		"--csi.endpoint=unix://" + socket, "--csi.controllerendpoint=unix://" + socket,
		"--csi.testvolumesize=1073741824", "--csi.testvolumeexpandsize=2147483648",
		"--csi.testvolumeaccesstype=block", "--csi.mountdir=" + filepath.Join(dir, "mnt"),
		"--csi.stagingdir=" + filepath.Join(dir, "staging"),
		"--csi.secrets=" + filepath.Join(configDir, "secrets.yaml"),
		"--csi.testvolumeparameters=" + filepath.Join(configDir, "params.yaml"),
		"--csi.testvolumemutableparameters=" + filepath.Join(configDir, "mutableparams.yaml"),
	}
	if focus != "" {
		args = append(args, "--ginkgo.focus-file="+focus)
	}
	// Opt-in shared-array inventory checks; unset => dedicated mode.
	if os.Getenv("SANITY_SHARED_ARRAY") != "" {
		args = append(args, "--shared-array")
	}
	cmd := exec.CommandContext(ctx, sanityBin, args...)
	cmd.Dir = dir
	var output bytes.Buffer
	combined := io.MultiWriter(os.Stdout, &output)
	cmd.Stdout, cmd.Stderr = combined, combined
	if err := cmd.Run(); err != nil {
		t.Fatalf("live sanity run failed: %v", err)
	}
	result := output.String()
	if fullSuite && (!strings.Contains(result, "Will run ") || !strings.Contains(result, "SUCCESS! -- ") || !strings.Contains(result, " | 0 Failed | ")) {
		t.Fatal("full live sanity suite did not complete with zero failures")
	}
	if !fullSuite && (!strings.Contains(result, "Will run 1 of ") || !strings.Contains(result, "1 Passed | 0 Failed")) {
		t.Fatalf("live sanity focus %s must select exactly one passing spec and have zero failures", focus)
	}
}

func waitForLocalSanitySocket(t *testing.T, socket string, serverErr <-chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case serveErr := <-serverErr:
			t.Fatalf("test driver startup failed: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("test driver socket did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestCreateVolumeRestoresRequestedCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		existing, wrongSource, mapped, refuse, stale bool
		initial                                      int64
		want                                         codes.Code
		patches                                      int
	}{
		{name: "new larger restore", initial: 1 << 30, want: codes.OK, patches: 1},
		{name: "resume partial restore", existing: true, initial: 1 << 30, want: codes.OK, patches: 1},
		{name: "already expanded", existing: true, initial: 2 << 30, want: codes.OK},
		{name: "different source untouched", existing: true, wrongSource: true, initial: 1 << 30, want: codes.AlreadyExists},
		{name: "mapped restore untouched", existing: true, mapped: true, initial: 1 << 30, want: codes.FailedPrecondition},
		{name: "resize rejection", existing: true, refuse: true, initial: 1 << 30, want: codes.InvalidArgument, patches: 1},
		{name: "resize readback mismatch", existing: true, stale: true, initial: 1 << 30, want: codes.Internal, patches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, exists, patches := tc.initial, tc.existing, 0
			clone := "src@snap1"
			if tc.wrongSource {
				clone = "other@snapshot"
			}
			lun := func() map[string]interface{} {
				protocols := map[string]string{}
				if tc.mapped {
					protocols["iscsi"] = "target"
				}
				return map[string]interface{}{"id": "lun-clone", "name": "pvcrestore", "size": size, "clone": clone, "protocols": protocols}
			}
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
					w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","volume_name":"src"}`))
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/list"):
					if exists {
						json.NewEncoder(w).Encode([]interface{}{lun()})
					} else {
						w.Write([]byte(`[]`))
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/clone/snap-1"):
					exists = true
					w.Write([]byte(`{"volume_id":"lun-clone"}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-clone"):
					json.NewEncoder(w).Encode(lun())
				case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-clone"):
					patches++
					var body map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["size"] != float64(1) || body["sing"] != "G" {
						t.Errorf("resize must send a 1 GiB increment: %v", body)
					}
					if tc.refuse {
						w.WriteHeader(400)
						w.Write([]byte(`{"code":999,"error":"resize rejected"}`))
						return
					}
					if !tc.stale {
						size += 1 << 30
					}
					w.Write([]byte(`{"success":true}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			req := createVolumeRequestForTest("restore", 2<<30)
			req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"}}}
			response, err := d.CreateVolume(context.Background(), req)
			if status.Code(err) != tc.want {
				t.Fatalf("code=%v want=%v err=%v", status.Code(err), tc.want, err)
			}
			if patches != tc.patches {
				t.Fatalf("resize requests=%d want=%d", patches, tc.patches)
			}
			if tc.want == codes.OK {
				if response.GetVolume().GetCapacityBytes() != 2<<30 {
					t.Fatalf("capacity=%d", response.GetVolume().GetCapacityBytes())
				}
				if _, err := d.CreateVolume(context.Background(), req); err != nil {
					t.Fatalf("repeat create: %v", err)
				}
				if patches != tc.patches {
					t.Fatal("repeat create expanded again")
				}
			}
		})
	}
}

func TestLocalSnapshotRestoreLargerFilesystem(t *testing.T) {
	if os.Getenv("ISCSI_LIVE_MOCK_SANITY") != "1" {
		t.Skip("explicit live backend opt-in required")
	}
	d, err := NewDriver(InputParameters{Endpoint: "unix:///restore-probe.sock", Role: "all", NodeId: "local-sanity", Ip: os.Getenv("STORAGE_IPS"), PoolName: os.Getenv("STORAGE_POOL_NAME"), ApiKey: os.Getenv("STORAGE_API_KEY"), PortalGroup: os.Getenv("STORAGE_PORTAL_GROUP")})
	if err != nil {
		t.Fatal(err)
	}
	d.coordination = newFakeCoordinator()
	d.attachmentLookup = localSanityLookup{nodeID: d.NodeId}
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "restore-capacity-")
	if err != nil {
		t.Fatal(err)
	}
	d.sessionStateDir = filepath.Join(dir, "state")
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	sourceName, restoredName := "s"+token, "d"+token
	var sourceID, restoredID, snapshotID string
	staged, published := map[string]bool{}, map[string]bool{}
	paths := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()
		if restoredID == "" && snapshotID != "" {
			lun, lookupErr := d.reconciler().LUNByNameStrict(cleanup, ngxnames.ModifyVolName(restoredName))
			if lookupErr != nil {
				t.Errorf("cleanup restore lookup: %v", lookupErr)
			}
			if lun != nil {
				snap, snapErr := d.client.Snapshots().Get(cleanup, snapshotID)
				check := createVolumeRequestForTest(restoredName, 1)
				if snapErr == nil && helper.ValidateExistingCreateVolume(lun, check, snap) == nil {
					restoredID = helper.VolumeID(volumeIDPrefix, lun.ID)
				} else {
					t.Errorf("cleanup cannot confirm orphan identity: %v", snapErr)
				}
			}
		}
		for _, id := range []string{restoredID, sourceID} {
			if id == "" {
				continue
			}
			if staged[id] {
				if _, e := d.NodeUnstageVolume(cleanup, &csi.NodeUnstageVolumeRequest{VolumeId: id, StagingTargetPath: paths[id]}); e != nil {
					t.Errorf("cleanup unstage: %v", e)
				}
			}
			if published[id] {
				if _, e := d.ControllerUnpublishVolume(cleanup, &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: d.NodeId}); e != nil {
					t.Errorf("cleanup unpublish: %v", e)
				}
			}
		}
		if restoredID != "" {
			if _, e := d.DeleteVolume(cleanup, &csi.DeleteVolumeRequest{VolumeId: restoredID}); e != nil {
				t.Errorf("cleanup restore: %v", e)
			}
		}
		if snapshotID != "" {
			if _, e := d.DeleteSnapshot(cleanup, &csi.DeleteSnapshotRequest{SnapshotId: snapshotID}); e != nil {
				t.Errorf("cleanup snapshot: %v", e)
			}
		}
		if sourceID != "" {
			if _, e := d.DeleteVolume(cleanup, &csi.DeleteVolumeRequest{VolumeId: sourceID}); e != nil {
				t.Errorf("cleanup source: %v", e)
			}
		}
		if !t.Failed() {
			os.RemoveAll(dir)
		}
	}()
	capability := createVolumeRequestForTest("", 1<<30).GetVolumeCapabilities()[0]
	stage := func(volume *csi.Volume) string {
		id := volume.GetVolumeId()
		published[id] = true
		pub, e := d.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: d.NodeId, VolumeCapability: capability})
		if e != nil {
			t.Fatalf("publish %s: %v", id, e)
		}
		path := filepath.Join(dir, fmt.Sprintf("stage-%d", len(paths)))
		paths[id], staged[id] = path, true
		_, e = d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: id, StagingTargetPath: path, PublishContext: pub.GetPublishContext(), VolumeContext: volume.GetVolumeContext(), VolumeCapability: capability})
		if e != nil {
			t.Fatalf("stage %s: %v", id, e)
		}
		return path
	}
	filesystemBytes := func(path string) uint64 {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			t.Fatal(err)
		}
		return stat.Blocks * uint64(stat.Bsize)
	}
	source, err := d.CreateVolume(ctx, createVolumeRequestForTest(sourceName, 1<<30))
	if err != nil {
		t.Fatal(err)
	}
	sourceID = source.GetVolume().GetVolumeId()
	sourcePath := stage(source.GetVolume())
	before := filesystemBytes(sourcePath)
	marker := []byte("restore-marker-" + token)
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: sourceID, StagingTargetPath: sourcePath}); err != nil {
		t.Fatal(err)
	}
	staged[sourceID] = false
	if _, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: sourceID, NodeId: d.NodeId}); err != nil {
		t.Fatal(err)
	}
	published[sourceID] = false
	snapshot, err := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "p" + token, SourceVolumeId: sourceID})
	if err != nil {
		t.Fatal(err)
	}
	snapshotID = snapshot.GetSnapshot().GetSnapshotId()
	req := createVolumeRequestForTest(restoredName, 2<<30)
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapshotID}}}
	restored, err := d.CreateVolume(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	restoredID = restored.GetVolume().GetVolumeId()
	if restored.GetVolume().GetCapacityBytes() < 2<<30 {
		t.Fatal("restore response is smaller than requested")
	}
	if _, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snapshotID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("snapshot with a dependent restore must return FailedPrecondition: %v", err)
	}
	restoredPath := stage(restored.GetVolume())
	after := filesystemBytes(restoredPath)
	if after <= before {
		t.Fatalf("restored filesystem did not grow: %d -> %d bytes", before, after)
	}
	data, err := os.ReadFile(filepath.Join(restoredPath, "marker"))
	if err != nil || !bytes.Equal(data, marker) {
		t.Fatalf("restored data mismatch: %q, %v", data, err)
	}
	t.Logf("restore PASS: 1 GiB -> 2 GiB, filesystem %d -> %d bytes, marker preserved", before, after)
}

func TestGetSnapshotRPCContract(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1") {
			w.Write([]byte(`{"id":"snap-1","volume_id":"lun-1","created":"1785499878","size":"1073741824","ready":true}`))
			return
		}
		http.NotFound(w, r)
	})
	// The typed CSI surface must reach the real implementation, never the
	// embedded Unimplemented stub.
	var srv csi.ControllerServer = d
	if _, err := srv.GetSnapshot(context.Background(), &csi.GetSnapshotRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id code = %s, want InvalidArgument", status.Code(err))
	}
	if _, err := srv.GetSnapshot(context.Background(), &csi.GetSnapshotRequest{SnapshotId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing id code = %s, want NotFound", status.Code(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.GetSnapshot(ctx, &csi.GetSnapshotRequest{SnapshotId: "snap-1"}); status.Code(err) != codes.Canceled {
		t.Fatalf("canceled ctx code = %s, want Canceled", status.Code(err))
	}
	resp, err := srv.GetSnapshot(context.Background(), &csi.GetSnapshotRequest{SnapshotId: "snap-1"})
	if err != nil || resp.GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Fatalf("canonical lookup = %#v, err=%v", resp, err)
	}
}

// --- Regressions: expand semantics, snapshot idempotency, ID prefix ---

// A smaller request is a shrink and must be rejected, not silently
// accepted by the whole-GiB idempotency rounding.
func TestControllerExpandVolumeShrinkRejected(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1") {
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":2147483648}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("shrink: code=%s err=%v, want InvalidArgument", status.Code(err), err)
	}
}

// A missing LUN is NotFound, not Internal.
func TestControllerExpandVolumeNotFoundMapped(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("absent LUN: code=%s err=%v, want NotFound", status.Code(err), err)
	}
}

// An empty/mismatched backend record is never expanded against.
func TestControllerExpandVolumeInvalidRecordFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":"lun-other","size":1073741824}`, `{"id":"lun-1","size":0}`} {
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1") {
				w.Write([]byte(body))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
			VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
			CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
		})
		if err == nil {
			t.Fatalf("record %s: expand succeeded against an invalid LUN record", body)
		}
	}
}

// An equal-capacity request stays an idempotent success with no mutation.
func TestControllerExpandVolumeEqualCapacityIdempotent(t *testing.T) {
	patches := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824}`))
		case r.Method == http.MethodPatch:
			patches++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	resp, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      helper.VolumeID(volumeIDPrefix, "lun-1"),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("equal capacity: %v", err)
	}
	if resp.GetCapacityBytes() != 1<<30 || patches != 0 {
		t.Fatalf("resp=%+v patches=%d, want idempotent success without mutation", resp, patches)
	}
}

// A same-name list record with an empty backend id is never returned as
// a successful idempotent snapshot.
func TestCreateSnapshotIdempotentEmptyIDFailsClosed(t *testing.T) {
	snapshotName := ngxnames.ModifyVolName("snapshot-empty")
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":"1073741824"}`))
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/api/v2/snapshot") || strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list")):
			w.Write([]byte(`[{"id":"","name":"` + snapshotName + `","volume_id":"lun-1"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snapshot-empty", SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("empty-id list record: code=%s err=%v, want Unavailable", status.Code(err), err)
	}
}

// The idempotent branch must read the snapshot back and verify identity.
func TestCreateSnapshotIdempotentReadbackMismatchFailsClosed(t *testing.T) {
	snapshotName := ngxnames.ModifyVolName("snapshot-mismatch")
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":"1073741824"}`))
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/api/v2/snapshot") || strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list")):
			w.Write([]byte(`[{"id":"snap-1","name":"` + snapshotName + `","volume_id":"lun-1"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			w.Write([]byte(`{"id":"snap-1","name":"other-name","volume_id":"lun-1","created":"1700000000","size":"1073741824"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snapshot-mismatch", SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("readback mismatch: code=%s err=%v, want Unavailable", status.Code(err), err)
	}
}

func TestCreateSnapshotIdempotentReadbackVerified(t *testing.T) {
	snapshotName := ngxnames.ModifyVolName("snapshot-ok")
	readbacks := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","size":"1073741824"}`))
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/api/v2/snapshot") || strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list")):
			w.Write([]byte(`[{"id":"snap-1","name":"` + snapshotName + `","volume_id":"lun-1"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
			readbacks++
			w.Write([]byte(`{"id":"snap-1","name":"` + snapshotName + `","volume_id":"lun-1","created":"1700000000","size":"1073741824","ready":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	resp, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snapshot-ok", SourceVolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
	})
	if err != nil {
		t.Fatalf("idempotent create with readback: %v", err)
	}
	if readbacks != 1 || resp.GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Fatalf("readbacks=%d resp=%+v, want one verified readback of snap-1", readbacks, resp)
	}
}

// A bare backend ID is not this driver's volume. DeleteVolume is idempotent
// -> OK, and the bare ID must NEVER reach the backend.
func TestDeleteVolumeBareBackendIDIdempotentOK(t *testing.T) {
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	resp, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "lun-1"})
	if err != nil {
		t.Fatalf("bare ID delete: err=%v, want idempotent OK", err)
	}
	if resp == nil || calls != 0 {
		t.Fatalf("resp=%v backend calls=%d, want OK without touching the backend", resp, calls)
	}
}

func TestControllerExpandVolumeBareBackendIDNotFound(t *testing.T) {
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "lun-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("bare ID expand: code=%s err=%v, want NotFound", status.Code(err), err)
	}
	if calls != 0 {
		t.Fatalf("backend touched %d times for a bare volume ID, want 0", calls)
	}
}

func TestControllerUnpublishVolumeBareBackendIDIdempotentOK(t *testing.T) {
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	resp, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "lun-1", NodeId: "worker0",
	})
	if err != nil || resp == nil {
		t.Fatalf("bare ID unpublish: resp=%v err=%v, want idempotent OK", resp, err)
	}
	if calls != 0 {
		t.Fatalf("backend touched %d times for a bare volume ID, want 0", calls)
	}
}

func TestControllerPublishVolumeBareBackendIDNotFound(t *testing.T) {
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "lun-1", NodeId: "worker0",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("bare ID publish: code=%s err=%v, want NotFound", status.Code(err), err)
	}
	if calls != 0 {
		t.Fatalf("backend touched %d times for a bare volume ID, want 0", calls)
	}
}

func TestListSnapshotsBareSourceReturnsEmpty(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/api/v2/snapshot") || strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/list")) {
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: "lun-1"})
	if err != nil {
		t.Fatalf("bare source ListSnapshots: err=%v, want empty OK", err)
	}
	if len(resp.GetEntries()) != 0 {
		t.Fatalf("entries=%d, want 0 for a non-driver source", len(resp.GetEntries()))
	}
}

// Adverse-backend regressions for the delete/unpublish/publish contracts.
// Each test drives the real RPC against a fake NGX API that violates the
// backend contract (acknowledges a mutation without applying it, drifts a
// configured resource, mutates the list between pages) and pins the
// fail-closed behavior the driver must keep.

// charEventRecorder records backend call order race-safely.
type charEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (c *charEventRecorder) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *charEventRecorder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func charCount(events []string, event string) int {
	n := 0
	for _, e := range events {
		if e == event {
			n++
		}
	}
	return n
}

func charLastIndex(events []string, event string) int {
	last := -1
	for i, e := range events {
		if e == event {
			last = i
		}
	}
	return last
}

// TestCharacterizationR1DeleteSnapshotTrusts200NoAbsenceReadback pins the
// read-after-delete contract for DeleteSnapshot: the DELETE 200 ack is not
// proof of deletion. Adverse condition: the backend acks but keeps the
// snapshot. The RPC must fail closed after an absence readback instead of
// attesting a deletion that never happened. The second subtest pins the
// mitigation for the known real trigger (clone dependency -> SDK
// ErrSnapshotInUse -> FailedPrecondition).
func TestCharacterizationR1DeleteSnapshotTrusts200NoAbsenceReadback(t *testing.T) {
	t.Run("200 ack without absence proof", func(t *testing.T) {
		rec := &charEventRecorder{}
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				rec.record("get")
				// The snapshot stays present even after the delete ack.
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				rec.record("delete")
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		// The short context bounds the verification poll loop for unit-test
		// runtime; the production budget is ISCSIDeleteVerifyTimeout.
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		if err == nil || resp != nil {
			t.Fatalf("DeleteSnapshot succeeded while the backend snapshot still exists: resp=%v err=%v", resp, err)
		}
		if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
			t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
		}
		events := rec.snapshot()
		if charCount(events, "delete") != 1 {
			t.Fatalf("backend call pattern = %v, want exactly one DELETE", events)
		}
		if charLastIndex(events, "get") <= charLastIndex(events, "delete") {
			t.Fatalf("expected an absence readback GET after the delete ack: %v", events)
		}
		// The object is still there; the driver must not have reported success.
		still, err := d.client.Snapshots().Get(context.Background(), "snap-1")
		if err != nil || still == nil || still.ID != "snap-1" {
			t.Fatalf("snapshot should still be present in the adverse backend: err=%v snap=%+v", err, still)
		}
	})

	t.Run("clone dependency mitigation maps to FailedPrecondition", func(t *testing.T) {
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"code":999,"error":"Snapshot could not be deleted. Because there are clone volumes created from this snapshot."}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v, want FailedPrecondition via SDK ErrSnapshotInUse (err=%v)", status.Code(err), err)
		}
	})
}

// TestDeleteVolumeAbsenceVerifiedAfterDelete pins the read-after-delete
// contract: a 200-success LUN DELETE response is not proof of backend
// absence. DeleteVolume must observe an authoritative NotFound before
// reporting CSI success. A still-present LUN or an ambiguous absence GET
// fails closed, so the CO never completes a deletion that did not happen
// (a false success would let a later same-name create adopt the stale LUN
// with the previous tenant's data as a "new" volume).
func TestDeleteVolumeAbsenceVerifiedAfterDelete(t *testing.T) {
	t.Run("delete ack while LUN persists fails closed", func(t *testing.T) {
		rec := &charEventRecorder{}
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				rec.record("lun-get")
				// All preconditions pass; the LUN survives the delete ack.
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				rec.record("lun-delete")
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		// The short context bounds the verification poll loop for unit-test
		// runtime; the production budget is ISCSIDeleteVerifyTimeout.
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		})
		if err == nil || resp != nil {
			t.Fatalf("DeleteVolume succeeded while the backend LUN still exists: resp=%v err=%v", resp, err)
		}
		if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
			t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
		}
		events := rec.snapshot()
		if charCount(events, "lun-delete") != 1 {
			t.Fatalf("backend call pattern = %v, want exactly one LUN DELETE", events)
		}
		if charLastIndex(events, "lun-get") <= charLastIndex(events, "lun-delete") {
			t.Fatalf("expected an absence readback GET after the delete ack: %v", events)
		}
		still, err := d.client.LUNs().Get(context.Background(), "lun-1")
		if err != nil || still == nil || still.ID != "lun-1" {
			t.Fatalf("LUN should still be present in the adverse backend: err=%v lun=%+v", err, still)
		}
	})

	t.Run("authoritative absence after delete succeeds", func(t *testing.T) {
		rec := &charEventRecorder{}
		deleted := false
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				rec.record("lun-get")
				if deleted {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
					return
				}
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				rec.record("lun-delete")
				deleted = true
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		resp, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		})
		if err != nil || resp == nil {
			t.Fatalf("DeleteVolume with authoritative absence: resp=%v err=%v", resp, err)
		}
		events := rec.snapshot()
		if charCount(events, "lun-delete") != 1 {
			t.Fatalf("backend call pattern = %v, want exactly one LUN DELETE", events)
		}
		if charLastIndex(events, "lun-get") <= charLastIndex(events, "lun-delete") {
			t.Fatalf("expected an absence readback GET after the delete ack: %v", events)
		}
		if _, err := d.client.LUNs().Get(context.Background(), "lun-1"); !ngxstorage.IsNotFound(err) {
			t.Fatalf("LUN should be absent after the honest delete: %v", err)
		}
	})

	t.Run("transient transport error then absence succeeds", func(t *testing.T) {
		var stateMu sync.Mutex
		deleted := false
		// Fail the first post-delete GET(s) at the transport layer; the SDK
		// may replay a safe GET once after a controller refresh, so allow
		// two wire failures before the authoritative 404.
		transportFailures := 2
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				gone := deleted
				fail := false
				if gone && transportFailures > 0 {
					transportFailures--
					fail = true
				}
				stateMu.Unlock()
				if gone {
					if fail {
						hj, ok := w.(http.Hijacker)
						if !ok {
							t.Error("response writer does not support hijack")
							return
						}
						conn, _, err := hj.Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						conn.Close()
						return
					}
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
					return
				}
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				deleted = true
				stateMu.Unlock()
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		resp, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		})
		if err != nil || resp == nil {
			t.Fatalf("DeleteVolume should retry an ambiguous GET and converge on absence: resp=%v err=%v", resp, err)
		}
	})

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		var stateMu sync.Mutex
		deleted := false
		deleteCalls := 0
		verifyGets := 0
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				gone := deleted
				if gone {
					verifyGets++
				}
				stateMu.Unlock()
				if gone {
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Error("response writer does not support hijack")
						return
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					conn.Close()
					return
				}
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				deleteCalls++
				deleted = true
				stateMu.Unlock()
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		})
		if err == nil || resp != nil {
			t.Fatalf("DeleteVolume treated an uncertain absence GET as success: resp=%v err=%v", resp, err)
		}
		stateMu.Lock()
		gotDeletes, gotGets := deleteCalls, verifyGets
		stateMu.Unlock()
		if gotDeletes != 1 || gotGets == 0 {
			t.Fatalf("delete calls = %d, verification GETs = %d, want 1 delete and at least one readback", gotDeletes, gotGets)
		}
	})

	t.Run("persistent server error fails closed", func(t *testing.T) {
		var stateMu sync.Mutex
		deleted := false
		deleteCalls := 0
		verifyGets := 0
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				gone := deleted
				if gone {
					verifyGets++
				}
				stateMu.Unlock()
				if gone {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"code":5000,"error":"backend unavailable"}`))
					return
				}
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				stateMu.Lock()
				deleteCalls++
				deleted = true
				stateMu.Unlock()
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		})
		if err == nil || resp != nil {
			t.Fatalf("DeleteVolume treated a 5xx absence GET as success: resp=%v err=%v", resp, err)
		}
		stateMu.Lock()
		gotDeletes, gotGets := deleteCalls, verifyGets
		stateMu.Unlock()
		if gotDeletes != 1 || gotGets == 0 {
			t.Fatalf("delete calls = %d, verification GETs = %d, want 1 delete and at least one readback", gotDeletes, gotGets)
		}
	})
}

// TestSameNameCreateAfterFailedDeleteRecoversExistingLUN pins the downstream
// half of the delete-verification contract: when the backend acknowledges a
// delete but keeps the LUN, DeleteVolume fails, so no deletion attestation
// reaches the CO and the PV is not removed. A same-name CreateVolume while
// the LUN persists is therefore idempotent recovery of the SAME volume (same
// backend ID, no new create) — never adoption of an orphaned stale LUN as a
// "new" volume with the previous tenant's data.
func TestSameNameCreateAfterFailedDeleteRecoversExistingLUN(t *testing.T) {
	deleteCalls := 0
	createCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list"):
			w.Write([]byte(`[{"id":"lun-1","name":"pvcvol1","size":1073741824}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			// Adverse backend: the LUN survives every delete ack.
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			deleteCalls++
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2/lun/"):
			createCalls++
			w.Write([]byte(`{"luns":[{"id":"unexpected","name":"pvcvol1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
	}); err == nil {
		t.Fatal("DeleteVolume must fail while the backend keeps the LUN; a false success would attest a deletion that never happened")
	}
	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}

	resp, err := d.CreateVolume(context.Background(), createVolumeRequestForTest("vol1", 1<<30))
	if err != nil {
		t.Fatalf("same-name CreateVolume after failed delete: %v", err)
	}
	if got, want := resp.GetVolume().GetVolumeId(), helper.VolumeID(volumeIDPrefix, "lun-1"); got != want {
		t.Fatalf("volume ID = %q, want the existing LUN ID %q (idempotent recovery, not a new volume)", got, want)
	}
	if createCalls != 0 {
		t.Fatalf("backend LUN create calls = %d, want 0 while the same-name LUN exists", createCalls)
	}
}

// TestCharacterizationR3UnpublishDeletesTargetWithoutUnmapConvergenceWait
// pins the unmap-convergence contract: ControllerUnpublishVolume must not
// delete the target on the strength of the unmap 200 ack alone. Adverse
// condition: the unmap is a silent no-op and the mapping is still live on
// every backend readback. The bounded convergence wait must fail closed
// BEFORE the destructive target delete; a live mapping is never stranded
// under a deleted target.
func TestCharacterizationR3UnpublishDeletesTargetWithoutUnmapConvergenceWait(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	rec := &charEventRecorder{}
	targetDeleted := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			rec.record("target-list")
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			rec.record("target-detail")
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			// Silent no-op unmap: the LUN stays mapped on every read.
			w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			rec.record("lun-detail")
			// The LUN keeps reporting the iSCSI mapping as well.
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			rec.record("unmap")
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			rec.record("target-delete")
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			// Legacy node-scoped group name: retained, not volume-owned.
			w.Write([]byte(`{"id":"auth-1","name":"legacy-node-group"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The short context bounds the convergence wait for unit-test runtime;
	// the production budget is iscsiUnmapVerifyAttempts x iscsiUnmapVerifyInterval.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:   "node-1",
	})
	if err == nil {
		t.Fatal("ControllerUnpublishVolume succeeded although the unmap never converged")
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("code = %v, want DeadlineExceeded from the bounded convergence wait (err=%v)", status.Code(err), err)
	}
	events := rec.snapshot()
	unmapAt := charLastIndex(events, "unmap")
	if unmapAt < 0 || charCount(events, "unmap") != 1 {
		t.Fatalf("call order = %v, want exactly one unmap attempt", events)
	}
	// The convergence readback happens after the unmap ack...
	if charLastIndex(events, "target-detail") <= unmapAt {
		t.Fatalf("expected a convergence readback after the unmap ack: %v", events)
	}
	// ...and the destructive target delete is never issued without proof.
	if charCount(events, "target-delete") != 0 {
		t.Fatalf("target delete issued without unmap convergence: %v", events)
	}
	if targetDeleted {
		t.Fatal("target must still exist in the adverse backend")
	}
}

// TestCharacterizationR4ListVolumesIndexTokenDuplicatesAfterInsertion pins
// the stable-cursor contract: the pagination token is a canonical-ID cursor,
// not a list index. A concurrent create that inserts at the head between
// pages must not shift the walk into repeating an already-returned volume.
func TestCharacterizationR4ListVolumesIndexTokenDuplicatesAfterInsertion(t *testing.T) {
	listCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list") {
			listCalls++
			if listCalls == 1 {
				w.Write([]byte(`[{"id":"l1","size":1073741824},{"id":"l2","size":1073741824},{"id":"l3","size":1073741824}]`))
			} else {
				// Concurrent create inserted l0 at the head between pages.
				w.Write([]byte(`[{"id":"l0","size":1073741824},{"id":"l1","size":1073741824},{"id":"l2","size":1073741824},{"id":"l3","size":1073741824}]`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	page1, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 2})
	if err != nil || len(page1.GetEntries()) != 2 || page1.GetNextToken() != "v1:"+helper.VolumeID(volumeIDPrefix, "l2") {
		t.Fatalf("page1 = %+v err=%v, want [l1 l2] with a canonical-ID cursor", page1, err)
	}
	page2, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{
		StartingToken: page1.GetNextToken(), MaxEntries: 2,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	seen := map[string]int{}
	for _, entry := range append(page1.GetEntries(), page2.GetEntries()...) {
		seen[entry.GetVolume().GetVolumeId()]++
	}
	// The head insertion must not duplicate l2, and the trailing l3 must
	// still be returned exactly once.
	for _, id := range []string{"l1", "l2", "l3"} {
		if seen[helper.VolumeID(volumeIDPrefix, id)] != 1 {
			t.Fatalf("%s occurrences = %d, want 1 (stable cursor); seen=%v", id, seen[helper.VolumeID(volumeIDPrefix, id)], seen)
		}
	}
	if len(page2.GetEntries()) != 1 || page2.GetEntries()[0].GetVolume().GetVolumeId() != helper.VolumeID(volumeIDPrefix, "l3") || page2.GetNextToken() != "" {
		t.Fatalf("page2 = %+v, want the single trailing entry l3 and no next token", page2)
	}
}

// TestCharacterizationR4ListVolumesIndexTokenOmitsAfterDeletion pins the
// omission side of the stable-cursor contract: a concurrent delete between
// pages must not shift the walk past a trailing volume that was never
// returned to the CO.
func TestCharacterizationR4ListVolumesIndexTokenOmitsAfterDeletion(t *testing.T) {
	listCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list") {
			listCalls++
			if listCalls == 1 {
				w.Write([]byte(`[{"id":"l1","size":1073741824},{"id":"l2","size":1073741824},{"id":"l3","size":1073741824}]`))
			} else {
				// Concurrent delete removed l1 between pages.
				w.Write([]byte(`[{"id":"l2","size":1073741824},{"id":"l3","size":1073741824}]`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	page1, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 2})
	if err != nil || len(page1.GetEntries()) != 2 || page1.GetNextToken() != "v1:"+helper.VolumeID(volumeIDPrefix, "l2") {
		t.Fatalf("page1 = %+v err=%v, want [l1 l2] with a canonical-ID cursor", page1, err)
	}
	page2, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{
		StartingToken: page1.GetNextToken(), MaxEntries: 2,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	// The cursor resumes after l2 by identity, so l3 is returned even though
	// the list shrank under the walk.
	if len(page2.GetEntries()) != 1 || page2.GetEntries()[0].GetVolume().GetVolumeId() != helper.VolumeID(volumeIDPrefix, "l3") || page2.GetNextToken() != "" {
		t.Fatalf("page2 = %+v, want [l3] and no next token (no omission)", page2)
	}
	seen := map[string]int{}
	for _, entry := range append(page1.GetEntries(), page2.GetEntries()...) {
		seen[entry.GetVolume().GetVolumeId()]++
	}
	for _, id := range []string{"l1", "l2", "l3"} {
		if seen[helper.VolumeID(volumeIDPrefix, id)] != 1 {
			t.Fatalf("%s occurrences = %d, want 1; seen=%v", id, seen[helper.VolumeID(volumeIDPrefix, id)], seen)
		}
	}
}

// TestCharacterizationR4ListSnapshotsIndexTokenDuplicatesAfterInsertion is
// the ListSnapshots counterpart of the stable-cursor contract: a head
// insertion between pages must not duplicate an already-returned snapshot.
func TestCharacterizationR4ListSnapshotsIndexTokenDuplicatesAfterInsertion(t *testing.T) {
	listCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list") {
			listCalls++
			if listCalls == 1 {
				w.Write([]byte(`[{"id":"s1","volume_id":"lun-1"},{"id":"s2","volume_id":"lun-1"},{"id":"s3","volume_id":"lun-1"}]`))
			} else {
				// Concurrent snapshot create inserted s0 at the head.
				w.Write([]byte(`[{"id":"s0","volume_id":"lun-1"},{"id":"s1","volume_id":"lun-1"},{"id":"s2","volume_id":"lun-1"},{"id":"s3","volume_id":"lun-1"}]`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	page1, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{MaxEntries: 2})
	if err != nil || len(page1.GetEntries()) != 2 || page1.GetNextToken() != "s1:s2" {
		t.Fatalf("page1 = %+v err=%v, want [s1 s2] with a canonical-ID cursor", page1, err)
	}
	page2, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		StartingToken: page1.GetNextToken(), MaxEntries: 2,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	seen := map[string]int{}
	for _, entry := range append(page1.GetEntries(), page2.GetEntries()...) {
		seen[entry.GetSnapshot().GetSnapshotId()]++
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if seen[id] != 1 {
			t.Fatalf("%s occurrences = %d, want 1 (stable cursor); seen=%v", id, seen[id], seen)
		}
	}
	if len(page2.GetEntries()) != 1 || page2.GetEntries()[0].GetSnapshot().GetSnapshotId() != "s3" || page2.GetNextToken() != "" {
		t.Fatalf("page2 = %+v, want the single trailing entry s3 and no next token", page2)
	}
}

// newTestDriverWithPortalGroup builds a Driver with a configured portal-group
// name. Unlike newTestDriver, portal-group list/detail responses are owned by
// mockHandler so a test can simulate post-startup configuration drift.
func newTestDriverWithPortalGroup(t *testing.T, mockHandler http.HandlerFunc, portalGroup string) *Driver {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/status/cluster"):
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool"):
			w.Write([]byte(`[{"id":"pool1","name":"pool1","owner":"o"}]`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool/pool1"):
			w.Write([]byte(`{"id":"pool1","name":"pool1","owner":"o"}`))
		default:
			mockHandler(w, r)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(req)
	})

	d, err := NewDriverWithHTTPClient(InputParameters{
		Ip:          "10.0.0.1",
		PoolName:    "pool1",
		ApiKey:      "test-key",
		Endpoint:    "http://127.0.0.1",
		PortalGroup: portalGroup,
		NodeId:      "node-1",
		Role:        "controller",
	}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewDriverWithHTTPClient: %v", err)
	}
	d.coordination = newFakeCoordinator()
	d.attachmentLookup = &fakeAttachmentLookup{}
	return d
}

// TestCharacterizationR5SelectPortalGroupSilentFallbackAfterRename pins the
// configured-portal-group contract: the configured name is validated at
// startup AND at publish time. If an operator renames/removes it afterwards
// (config drift), publish must fail closed with NotFound instead of silently
// selecting an unrelated group whose portals would be handed to the CO.
func TestCharacterizationR5SelectPortalGroupSilentFallbackAfterRename(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	// Closing the channel flips the backend into the drifted state; channel
	// reads are safe from the HTTP server goroutines.
	drift := make(chan struct{})
	isDrifted := func() bool {
		select {
		case <-drift:
			return true
		default:
			return false
		}
	}
	targetCreated := false
	var targetCreateBody map[string]interface{}
	d := newTestDriverWithPortalGroup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/portal_group":
			if !isDrifted() {
				// Startup: the configured name exists and passes validation.
				w.Write([]byte(`[{"id":"pg1","name":"pg-configured","owner":"o"}]`))
			} else {
				// Drift: pg-configured was renamed to pg-renamed and an
				// unrelated group now sorts first.
				w.Write([]byte(`[{"id":"pg-other","name":"unrelated-group","owner":"o"},{"id":"pg1","name":"pg-renamed","owner":"o"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/portal_group/pg-other"):
			w.Write([]byte(`{"id":"pg-other","name":"unrelated-group","owner":"o","listen_ips":[{"id":"lip9","interface_name":"igb9","ip_address":"10.99.99.1","owner":"o"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/portal_group/pg1"):
			w.Write([]byte(`{"id":"pg1","name":"pg-renamed","owner":"o","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"o"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","scsi_id":"3600","owner":"o"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetCreated {
				w.Write([]byte(`[{"id":"t1","name":"` + targetName + `"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/auth_group"):
			w.Write([]byte(`{"id":"auth-new"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/target/iscsi"):
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &targetCreateBody)
			targetCreated = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/target/iscsi/lun/"):
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/t1"):
			w.Write([]byte(`{"id":"t1","name":"` + targetName + `","auth_group_id":"auth-new","target_iqn":"iqn.2000-01.example:tgt","luns":[{"id":"lun-1","name":"pvcvol1","number":"6"}]}`))
		default:
			w.Write([]byte(`{"connected":1,"status":"Master"}`))
		}
	}, "pg-configured")

	// Startup validation passed; now the operator renames the group.
	close(drift)

	resp, err := d.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId:         helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:           "node-1",
		VolumeContext:    map[string]string{"fstype": "ext4"},
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
	})
	if err == nil || resp != nil {
		t.Fatalf("ControllerPublishVolume fell back to an unrelated portal group: resp=%v err=%v", resp, err)
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound for the absent configured portal group (err=%v)", status.Code(err), err)
	}
	// Fail closed before any backend mutation: no target was created against
	// the fallback group and no unrelated portals were handed to the CO.
	if targetCreated || targetCreateBody != nil {
		t.Fatalf("target create issued despite absent configured portal group: body=%v", targetCreateBody)
	}
}

// TestCharacterizationR6UnpublishTrustsTargetDelete200 pins the
// read-after-delete contract for the per-volume iSCSI target: the DELETE 200
// ack is not proof of absence. Adverse condition: the backend acks but keeps
// the target. The RPC must fail closed after an absence readback instead of
// reporting unpublish success and leaving an orphan target behind.
func TestCharacterizationR6UnpublishTrustsTargetDelete200(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	rec := &charEventRecorder{}
	unmapped := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			rec.record("target-list")
			// The target survives its own delete ack.
			w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			rec.record("target-detail")
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			rec.record("unmap")
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			rec.record("target-delete")
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The short context bounds the verification poll loop for unit-test
	// runtime; the production budget is ISCSIDeleteVerifyTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"),
		NodeId:   "node-1",
	})
	if err == nil {
		t.Fatal("ControllerUnpublishVolume succeeded while the backend target still exists")
	}
	if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
		t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
	}
	events := rec.snapshot()
	deleteAt := charLastIndex(events, "target-delete")
	if deleteAt < 0 || charCount(events, "target-delete") != 1 {
		t.Fatalf("target delete never issued or repeated: %v", events)
	}
	// The absence readback happens after the delete ack.
	if charLastIndex(events, "target-detail") <= deleteAt {
		t.Fatalf("expected an absence readback GET after the target delete ack: %v", events)
	}
	still, err := d.client.ISCSITargets().Get(context.Background(), "target-1")
	if err != nil || still == nil || still.ID != "target-1" {
		t.Fatalf("target should still be present in the adverse backend: err=%v target=%+v", err, still)
	}
}

// TestCharacterizationR7UnpublishTrustsAuthGroupDelete200 pins the
// read-after-delete contract for the volume-scoped auth group, both in
// ControllerUnpublishVolume's owned-group cleanup and in
// CleanupOrphanedVolumeScopedAuthGroup (same verification). Adverse
// condition: the backend acks but keeps the group; the orphan auth group
// would retain stale CHAP credentials and IQNs. The RPC must fail closed
// after an absence readback instead of reporting success.
func TestCharacterizationR7UnpublishTrustsAuthGroupDelete200(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	targetName := ngxnames.ModifyVolName(volume)
	rec := &charEventRecorder{}
	unmapped := false
	targetDeleted := false
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			rec.record("target-list")
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
			rec.record("auth-list")
			// The group survives its own delete ack.
			w.Write([]byte(`[{"id":"auth-1","name":"` + targetName + `"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			rec.record("auth-get")
			w.Write([]byte(`{"id":"auth-1","name":"` + targetName + `"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			rec.record("auth-delete")
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// The short context bounds the verification poll loop for unit-test
	// runtime; the production budget is ISCSIDeleteVerifyTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volume,
		NodeId:   "node-1",
	})
	if err == nil {
		t.Fatal("ControllerUnpublishVolume succeeded while the backend auth group still exists")
	}
	if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
		t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
	}
	events := rec.snapshot()
	deleteAt := charLastIndex(events, "auth-delete")
	if deleteAt < 0 || charCount(events, "auth-delete") != 1 {
		t.Fatalf("auth group delete never issued or repeated: %v", events)
	}
	// The absence readback happens after the delete ack.
	if charLastIndex(events, "auth-get") <= deleteAt {
		t.Fatalf("expected an absence readback GET after the auth group delete ack: %v", events)
	}
	still, err := d.client.AuthGroups().Get(context.Background(), "auth-1")
	if err != nil || still == nil || still.ID != "auth-1" {
		t.Fatalf("auth group should still be present in the adverse backend: err=%v group=%+v", err, still)
	}
}

// TestDeleteSnapshotAbsenceVerifiedAfterDelete pins the DeleteSnapshot
// read-after-delete contract on the real RPC path: success requires an
// authoritative NotFound readback; contradictory or ambiguous absence GETs
// fail closed so the CO never completes a deletion that did not happen.
func TestDeleteSnapshotAbsenceVerifiedAfterDelete(t *testing.T) {
	t.Run("authoritative absence after delete succeeds", func(t *testing.T) {
		rec := &charEventRecorder{}
		deleted := false
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				rec.record("get")
				if deleted {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
					return
				}
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				rec.record("delete")
				deleted = true
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		resp, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		if err != nil || resp == nil {
			t.Fatalf("DeleteSnapshot with authoritative absence: resp=%v err=%v", resp, err)
		}
		events := rec.snapshot()
		if charCount(events, "delete") != 1 {
			t.Fatalf("backend call pattern = %v, want exactly one DELETE", events)
		}
		if charLastIndex(events, "get") <= charLastIndex(events, "delete") {
			t.Fatalf("expected an absence readback GET after the delete ack: %v", events)
		}
	})

	t.Run("transient transport error then absence succeeds", func(t *testing.T) {
		var stateMu sync.Mutex
		deleted := false
		// Fail the first post-delete GET(s) at the transport layer; the SDK
		// may replay a safe GET once after a controller refresh, so allow
		// two wire failures before the authoritative 404.
		transportFailures := 2
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				stateMu.Lock()
				gone := deleted
				fail := false
				if gone && transportFailures > 0 {
					transportFailures--
					fail = true
				}
				stateMu.Unlock()
				if gone {
					if fail {
						hj, ok := w.(http.Hijacker)
						if !ok {
							t.Error("response writer does not support hijack")
							return
						}
						conn, _, err := hj.Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						conn.Close()
						return
					}
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
					return
				}
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				stateMu.Lock()
				deleted = true
				stateMu.Unlock()
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		resp, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		if err != nil || resp == nil {
			t.Fatalf("DeleteSnapshot should retry an ambiguous GET and converge on absence: resp=%v err=%v", resp, err)
		}
	})

	ambiguous := []struct {
		name   string
		status int
		body   string
	}{
		{"404 with other code fails closed", 404, `{"code":710,"error":"exists"}`},
		{"403 carrying NotFound code fails closed", 403, `{"code":5011,"error":"forbidden"}`},
		{"5xx carrying NotFound code fails closed", 500, `{"code":5011,"error":"internal"}`},
		{"503 fails closed", 503, `{"error":"unavailable"}`},
	}
	for _, tc := range ambiguous {
		t.Run(tc.name, func(t *testing.T) {
			deleted := false
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
					if deleted {
						w.WriteHeader(tc.status)
						w.Write([]byte(tc.body))
						return
					}
					w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
					deleted = true
					w.Write([]byte(`{"success":true}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			resp, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
			if err == nil || resp != nil {
				t.Fatalf("DeleteSnapshot treated HTTP %d %s as absence: resp=%v err=%v", tc.status, tc.body, resp, err)
			}
			if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
				t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
			}
		})
	}

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		var stateMu sync.Mutex
		deleted := false
		deleteCalls := 0
		verifyGets := 0
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				stateMu.Lock()
				gone := deleted
				if gone {
					verifyGets++
				}
				stateMu.Unlock()
				if gone {
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Error("response writer does not support hijack")
						return
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					conn.Close()
					return
				}
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-src","ready":true}`))
			case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/v2/snapshot/snap-1"):
				stateMu.Lock()
				deleteCalls++
				deleted = true
				stateMu.Unlock()
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"})
		if err == nil || resp != nil {
			t.Fatalf("DeleteSnapshot treated an uncertain absence GET as success: resp=%v err=%v", resp, err)
		}
		stateMu.Lock()
		gotDeletes, gotGets := deleteCalls, verifyGets
		stateMu.Unlock()
		if gotDeletes != 1 || gotGets == 0 {
			t.Fatalf("delete calls = %d, verification GETs = %d, want 1 delete and at least one readback", gotDeletes, gotGets)
		}
	})
}

// unpublishConvergedMock serves an honest unmap/target-delete backend: the
// unmap removes the mapping, the target delete removes the target. The
// post-delete target GET response is injectable for absence-verification
// regressions; authGroupDeleteCalls counts owned-group cleanup deletes.
func unpublishConvergedMock(t *testing.T, targetName string, targetGetAfterDelete func(w http.ResponseWriter), authGroupBody string, authGetAfterDelete func(w http.ResponseWriter), authGroupDeleteCalls *int) http.HandlerFunc {
	t.Helper()
	var unmapped, targetDeleted, authDeleted bool
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
			if targetDeleted {
				w.Write([]byte(`[]`))
			} else {
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			if targetDeleted {
				if targetGetAfterDelete != nil {
					targetGetAfterDelete(w)
					return
				}
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
				return
			}
			if unmapped {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
			} else {
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
			if unmapped {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
			} else {
				w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
			unmapped = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
			targetDeleted = true
			w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			if authDeleted {
				if authGetAfterDelete != nil {
					authGetAfterDelete(w)
					return
				}
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"auth group no longer exists"}`))
				return
			}
			w.Write([]byte(authGroupBody))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
			authDeleted = true
			if authGroupDeleteCalls != nil {
				*authGroupDeleteCalls++
			}
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestControllerUnpublishTargetAbsenceVerifiedAfterDelete pins the target
// read-after-delete contract on the real unpublish path: an ambiguous or
// contradictory absence GET after the target DELETE ack fails closed before
// auth-group cleanup or success bookkeeping.
func TestControllerUnpublishTargetAbsenceVerifiedAfterDelete(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))
	legacyGroup := `{"id":"auth-1","name":"legacy-node-group"}`

	t.Run("authoritative absence after delete succeeds", func(t *testing.T) {
		rec := &charEventRecorder{}
		handler := unpublishConvergedMock(t, targetName, nil, legacyGroup, nil, nil)
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1") {
				rec.record("target-detail")
			}
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1") {
				rec.record("target-delete")
			}
			handler(w, r)
		})
		if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
		}); err != nil {
			t.Fatalf("ControllerUnpublishVolume with authoritative target absence: %v", err)
		}
		events := rec.snapshot()
		if charLastIndex(events, "target-detail") <= charLastIndex(events, "target-delete") {
			t.Fatalf("expected an absence readback GET after the target delete ack: %v", events)
		}
	})

	ambiguous := []struct {
		name   string
		status int
		body   string
	}{
		{"404 with other code fails closed", 404, `{"code":710,"error":"exists"}`},
		{"403 carrying NotFound code fails closed", 403, `{"code":5011,"error":"forbidden"}`},
		{"5xx carrying TargetDeleted code fails closed", 500, `{"code":611,"error":"internal"}`},
	}
	for _, tc := range ambiguous {
		t.Run(tc.name, func(t *testing.T) {
			authDeletes := 0
			d := newTestDriver(t, unpublishConvergedMock(t, targetName,
				func(w http.ResponseWriter) {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				}, legacyGroup, nil, &authDeletes))
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
				VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
			})
			if err == nil {
				t.Fatalf("ControllerUnpublishVolume treated HTTP %d %s as target absence", tc.status, tc.body)
			}
			if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
				t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
			}
			if authDeletes != 0 {
				t.Fatalf("auth group cleanup ran although target absence was not proven: deletes=%d", authDeletes)
			}
		})
	}
}

// TestControllerUnpublishAuthGroupAbsenceVerifiedAfterDelete pins the
// auth-group read-after-delete contract on both cleanup paths: the owned
// volume-scoped group delete in ControllerUnpublishVolume and the orphan
// cleanup in CleanupOrphanedVolumeScopedAuthGroup (reached via DeleteVolume
// when the LUN is already absent).
func TestControllerUnpublishAuthGroupAbsenceVerifiedAfterDelete(t *testing.T) {
	volume := helper.VolumeID(volumeIDPrefix, "lun-1")
	targetName := ngxnames.ModifyVolName(volume)
	ownedGroup := `{"id":"auth-1","name":"` + targetName + `"}`

	t.Run("authoritative absence after delete succeeds", func(t *testing.T) {
		authDeletes := 0
		rec := &charEventRecorder{}
		handler := unpublishConvergedMock(t, targetName, nil, ownedGroup, nil, &authDeletes)
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1") {
				rec.record("auth-get")
			}
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1") {
				rec.record("auth-delete")
			}
			handler(w, r)
		})
		if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: volume, NodeId: "node-1",
		}); err != nil {
			t.Fatalf("ControllerUnpublishVolume with authoritative auth group absence: %v", err)
		}
		if authDeletes != 1 {
			t.Fatalf("auth group delete calls = %d, want exactly 1", authDeletes)
		}
		events := rec.snapshot()
		if charLastIndex(events, "auth-get") <= charLastIndex(events, "auth-delete") {
			t.Fatalf("expected an absence readback GET after the auth group delete ack: %v", events)
		}
	})

	ambiguous := []struct {
		name   string
		status int
		body   string
	}{
		{"404 with other code fails closed", 404, `{"code":710,"error":"exists"}`},
		{"403 carrying NotFound code fails closed", 403, `{"code":5011,"error":"forbidden"}`},
		{"5xx carrying NotFound code fails closed", 500, `{"code":5011,"error":"internal"}`},
	}
	for _, tc := range ambiguous {
		t.Run(tc.name, func(t *testing.T) {
			authDeletes := 0
			d := newTestDriver(t, unpublishConvergedMock(t, targetName, nil, ownedGroup,
				func(w http.ResponseWriter) {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				}, &authDeletes))
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
				VolumeId: volume, NodeId: "node-1",
			})
			if err == nil {
				t.Fatalf("ControllerUnpublishVolume treated HTTP %d %s as auth group absence", tc.status, tc.body)
			}
			if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
				t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
			}
			if authDeletes != 1 {
				t.Fatalf("auth group delete calls = %d, want exactly 1 before the failed readback", authDeletes)
			}
		})
	}

	t.Run("orphan cleanup path verifies absence", func(t *testing.T) {
		// DeleteVolume with the LUN already absent reaches
		// CleanupOrphanedVolumeScopedAuthGroup; the group delete acks but the
		// group survives, so the RPC must fail closed on the readback.
		authDeletes := 0
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/auth_group"):
				w.Write([]byte(`[{"id":"auth-1","name":"` + targetName + `"}]`))
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[]`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
				// The group survives its own delete ack.
				w.Write([]byte(ownedGroup))
			case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
				authDeletes++
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volume})
		if err == nil {
			t.Fatal("DeleteVolume orphan cleanup succeeded while the auth group still exists")
		}
		if code := status.Code(err); code != codes.DeadlineExceeded && code != codes.Internal {
			t.Fatalf("code = %v, want DeadlineExceeded/Internal (err=%v)", code, err)
		}
		if authDeletes != 1 {
			t.Fatalf("auth group delete calls = %d, want exactly 1", authDeletes)
		}
	})
}

// TestControllerUnpublishWaitsForUnmapConvergence pins the convergence gate
// on the real unpublish path: a converged (possibly delayed) unmap proceeds
// to the verified target delete; an ambiguous convergence readback fails
// closed before any target delete.
func TestControllerUnpublishWaitsForUnmapConvergence(t *testing.T) {
	targetName := ngxnames.ModifyVolName(helper.VolumeID(volumeIDPrefix, "lun-1"))

	t.Run("converged unmap proceeds to verified target delete", func(t *testing.T) {
		rec := &charEventRecorder{}
		handler := unpublishConvergedMock(t, targetName, nil, `{"id":"auth-1","name":"legacy-node-group"}`, nil, nil)
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
				rec.record("unmap")
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				rec.record("target-detail")
			case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				rec.record("target-delete")
			}
			handler(w, r)
		})
		if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
		}); err != nil {
			t.Fatalf("ControllerUnpublishVolume with converged unmap: %v", err)
		}
		events := rec.snapshot()
		unmapAt := charLastIndex(events, "unmap")
		deleteAt := charLastIndex(events, "target-delete")
		if unmapAt < 0 || deleteAt < 0 || deleteAt < unmapAt {
			t.Fatalf("call order = %v, want unmap before target delete", events)
		}
		convergenceRead := false
		for i, e := range events {
			if e == "target-detail" && i > unmapAt && i < deleteAt {
				convergenceRead = true
			}
		}
		if !convergenceRead {
			t.Fatalf("expected a convergence readback between unmap ack and target delete: %v", events)
		}
		if charLastIndex(events, "target-detail") <= deleteAt {
			t.Fatalf("expected an absence readback after the target delete ack: %v", events)
		}
	})

	t.Run("delayed convergence proceeds", func(t *testing.T) {
		var mu sync.Mutex
		unmapped, targetDeleted := false, false
		detailReads := 0
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				mu.Lock()
				gone := targetDeleted
				mu.Unlock()
				if gone {
					w.Write([]byte(`[]`))
				} else {
					w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
				}
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				mu.Lock()
				gone := targetDeleted
				if !gone {
					detailReads++
				}
				reads := detailReads
				mu.Unlock()
				if gone {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"target no longer exists"}`))
					return
				}
				// The first post-unmap readback still shows the mapping;
				// the backend converges on the second read.
				if reads <= 2 {
					w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
				} else {
					w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[]}`))
				}
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/lun-1"):
				mu.Lock()
				reads := detailReads
				mu.Unlock()
				if reads <= 2 {
					w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`))
				} else {
					w.Write([]byte(`{"id":"lun-1","name":"pvcvol1","protocols":{}}`))
				}
			case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
				mu.Lock()
				unmapped = true
				mu.Unlock()
				w.Write([]byte(`{"success":true}`))
			case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				mu.Lock()
				targetDeleted = true
				mu.Unlock()
				w.Write([]byte(`{"success":true}`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/auth_group/auth-1"):
				w.Write([]byte(`{"id":"auth-1","name":"legacy-node-group"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
		}); err != nil {
			t.Fatalf("ControllerUnpublishVolume should wait for delayed unmap convergence: %v", err)
		}
		mu.Lock()
		reads, gone, unmap := detailReads, targetDeleted, unmapped
		mu.Unlock()
		if !unmap || !gone || reads < 3 {
			t.Fatalf("unmap=%v targetDeleted=%v detailReads=%d, want a re-read that converged before the delete", unmap, gone, reads)
		}
	})

	t.Run("ambiguous convergence readback fails closed before target delete", func(t *testing.T) {
		targetDeletes := 0
		unmapped := false
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/target/iscsi"):
				w.Write([]byte(`[{"id":"target-1","name":"` + targetName + `"}]`))
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				if unmapped {
					// The convergence readback is ambiguous, never absence.
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"code":5000,"error":"internal"}`))
					return
				}
				w.Write([]byte(`{"id":"target-1","name":"` + targetName + `","auth_group_id":"auth-1","luns":[{"id":"lun-1","number":"0"}]}`))
			case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/lun/target-1"):
				unmapped = true
				w.Write([]byte(`{"success":true}`))
			case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v2/target/iscsi/target-1"):
				targetDeletes++
				w.Write([]byte(`{"success":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		_, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
			VolumeId: helper.VolumeID(volumeIDPrefix, "lun-1"), NodeId: "node-1",
		})
		if err == nil {
			t.Fatal("ControllerUnpublishVolume deleted the target despite an ambiguous convergence readback")
		}
		if status.Code(err) != codes.Internal {
			t.Fatalf("code = %v, want Internal for the failed convergence verification (err=%v)", status.Code(err), err)
		}
		if targetDeletes != 0 {
			t.Fatalf("target delete calls = %d, want 0 without convergence proof", targetDeletes)
		}
	})
}

// TestListVolumesStableCursorAcrossMutations pins the canonical-ID cursor
// contract under concurrent creates AND deletes between pages: every volume
// at or after the cursor is returned exactly once, and unknown tokens are
// Aborted.
func TestListVolumesStableCursorAcrossMutations(t *testing.T) {
	vid := func(id string) string { return helper.VolumeID(volumeIDPrefix, id) }
	listCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2/lun/list") {
			listCalls++
			switch listCalls {
			case 1:
				w.Write([]byte(`[{"id":"l1","size":1073741824},{"id":"l2","size":1073741824},{"id":"l3","size":1073741824},{"id":"l4","size":1073741824}]`))
			case 2:
				// l1 deleted; l0 and l5 created between pages.
				w.Write([]byte(`[{"id":"l0","size":1073741824},{"id":"l2","size":1073741824},{"id":"l3","size":1073741824},{"id":"l4","size":1073741824},{"id":"l5","size":1073741824}]`))
			default:
				// l2 deleted; l6 created between pages.
				w.Write([]byte(`[{"id":"l0","size":1073741824},{"id":"l3","size":1073741824},{"id":"l4","size":1073741824},{"id":"l5","size":1073741824},{"id":"l6","size":1073741824}]`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	seen := map[string]int{}
	token := ""
	pages := 0
	for {
		resp, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: token, MaxEntries: 2})
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		for _, entry := range resp.GetEntries() {
			seen[entry.GetVolume().GetVolumeId()]++
		}
		pages++
		if resp.GetNextToken() == "" {
			break
		}
		token = resp.GetNextToken()
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	// Every volume that existed at or after the walk's cursor position is
	// returned exactly once; the head-inserted l0 sorts before the first
	// cursor and is outside the walk (stable-cursor semantics).
	for _, id := range []string{"l1", "l2", "l3", "l4", "l5", "l6"} {
		if seen[vid(id)] != 1 {
			t.Fatalf("%s occurrences = %d, want 1; seen=%v", id, seen[vid(id)], seen)
		}
	}
	if seen[vid("l0")] != 0 {
		t.Fatalf("l0 occurrences = %d, want 0 (inserted before the cursor); seen=%v", seen[vid("l0")], seen)
	}

	for _, token := range []string{"2", "v1:", "s1:l1", "garbage", "-1"} {
		_, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: token})
		if status.Code(err) != codes.Aborted {
			t.Fatalf("token %q: code = %v, want Aborted", token, status.Code(err))
		}
	}

	t.Run("cursor past all entries yields an empty page", func(t *testing.T) {
		resp, err := d.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: "v1:" + vid("zz")})
		if err != nil || len(resp.GetEntries()) != 0 || resp.GetNextToken() != "" {
			t.Fatalf("resp = %+v err=%v, want an empty final page", resp, err)
		}
	})
}

// TestListSnapshotsStableCursorAcrossMutations is the ListSnapshots
// counterpart: canonical-ID cursors survive concurrent creates and deletes
// between pages without duplicates or omissions; unknown tokens are Aborted.
func TestListSnapshotsStableCursorAcrossMutations(t *testing.T) {
	listCalls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/snapshot/list") {
			listCalls++
			switch listCalls {
			case 1:
				w.Write([]byte(`[{"id":"s1","volume_id":"lun-1"},{"id":"s2","volume_id":"lun-1"},{"id":"s3","volume_id":"lun-1"},{"id":"s4","volume_id":"lun-1"}]`))
			case 2:
				// s1 deleted; s0 and s5 created between pages.
				w.Write([]byte(`[{"id":"s0","volume_id":"lun-1"},{"id":"s2","volume_id":"lun-1"},{"id":"s3","volume_id":"lun-1"},{"id":"s4","volume_id":"lun-1"},{"id":"s5","volume_id":"lun-1"}]`))
			default:
				// s2 deleted; s6 created between pages.
				w.Write([]byte(`[{"id":"s0","volume_id":"lun-1"},{"id":"s3","volume_id":"lun-1"},{"id":"s4","volume_id":"lun-1"},{"id":"s5","volume_id":"lun-1"},{"id":"s6","volume_id":"lun-1"}]`))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	seen := map[string]int{}
	token := ""
	pages := 0
	for {
		resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: token, MaxEntries: 2})
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		for _, entry := range resp.GetEntries() {
			seen[entry.GetSnapshot().GetSnapshotId()]++
		}
		pages++
		if resp.GetNextToken() == "" {
			break
		}
		token = resp.GetNextToken()
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		if seen[id] != 1 {
			t.Fatalf("%s occurrences = %d, want 1; seen=%v", id, seen[id], seen)
		}
	}
	if seen["s0"] != 0 {
		t.Fatalf("s0 occurrences = %d, want 0 (inserted before the cursor); seen=%v", seen["s0"], seen)
	}

	for _, token := range []string{"2", "s1:", "v1:s1", "garbage", "-1"} {
		_, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: token})
		if status.Code(err) != codes.Aborted {
			t.Fatalf("token %q: code = %v, want Aborted", token, status.Code(err))
		}
	}

	t.Run("cursor past all entries yields an empty page", func(t *testing.T) {
		resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: "s1:zz"})
		if err != nil || len(resp.GetEntries()) != 0 || resp.GetNextToken() != "" {
			t.Fatalf("resp = %+v err=%v, want an empty final page", resp, err)
		}
	})
}
