package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

type concurrentController struct {
	csi.UnimplementedControllerServer
	entered       chan struct{}
	release       chan struct{}
	inFlight      atomic.Int32
	maxConcurrent atomic.Int32
}

type expandController struct {
	csi.UnimplementedControllerServer
	mu       sync.Mutex
	capacity int64
	additive bool
}

func (s *expandController) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := req.GetCapacityRange().GetRequiredBytes()
	if s.additive {
		// Deliberately model the historical caller race against an additive
		// backend: each stale 1Gi->2Gi request adds another 1Gi.
		s.capacity += 1 << 30
	} else if target > s.capacity {
		s.capacity = target
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: s.capacity}, nil
}

func (s *expandController) ControllerGetVolume(context.Context, *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &csi.ControllerGetVolumeResponse{Volume: &csi.Volume{VolumeId: "volume-1", CapacityBytes: s.capacity}}, nil
}

func serveController(t *testing.T, impl csi.ControllerServer) string {
	t.Helper()
	// Unix socket paths are limited to 107 bytes on Linux. t.TempDir embeds
	// the long test name, which exceeds that limit under the required cache
	// TMPDIR. Keep only this socket fixture in a short, unique directory.
	dir, err := os.MkdirTemp(os.TempDir(), "csi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "csi.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	csi.RegisterControllerServer(server, impl)
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	return socket
}

func (s *concurrentController) CreateVolume(context.Context, *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	current := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		max := s.maxConcurrent.Load()
		if current <= max || s.maxConcurrent.CompareAndSwap(max, current) {
			break
		}
	}
	s.entered <- struct{}{}
	<-s.release
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{VolumeId: "same-volume", CapacityBytes: 1 << 30}}, nil
}

func TestCreateVolumeCommandUsesConcurrentBarrier(t *testing.T) {
	impl := &concurrentController{entered: make(chan struct{}, 2), release: make(chan struct{})}
	socket := serveController(t, impl)

	done := make(chan error, 1)
	go func() {
		_, err := createVolumeCommand(context.Background(), []string{
			"--endpoint=unix://" + socket,
			"--name=race-volume",
			"--count=2",
			"--timeout=5s",
		})
		done <- err
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-impl.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("two CreateVolume RPCs did not overlap; barrier is not exercising concurrency")
		}
	}
	close(impl.release)
	if err := <-done; err != nil {
		t.Fatalf("createVolumeCommand: %v", err)
	}
	if got := impl.maxConcurrent.Load(); got != 2 {
		t.Fatalf("max concurrent RPCs = %d, want 2", got)
	}
}

func TestControllerExpandCommandRequiresExactFinalCapacity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		additive  bool
		wantError bool
	}{
		{name: "serialized-idempotent", additive: false, wantError: false},
		{name: "double-additive-regression", additive: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			impl := &expandController{capacity: 1 << 30, additive: tc.additive}
			socket := serveController(t, impl)
			out, err := controllerExpandCommand(context.Background(), []string{
				"--endpoint=unix://" + socket,
				"--volume-id=volume-1",
				"--sizes=2147483648,2147483648",
				"--timeout=5s",
			})
			if tc.wantError {
				if err == nil {
					t.Fatalf("double-additive final capacity was accepted: %+v", out)
				}
				if out.FinalCapacityBytes != 3<<30 {
					t.Fatalf("regression final capacity = %d, want 3GiB evidence", out.FinalCapacityBytes)
				}
				return
			}
			if err != nil {
				t.Fatalf("controllerExpandCommand: %v", err)
			}
			if out.FinalCapacityBytes != 2<<30 {
				t.Fatalf("final capacity = %d, want exactly 2GiB", out.FinalCapacityBytes)
			}
		})
	}
}

func TestParsers(t *testing.T) {
	if got, err := unixSocketPath("unix:///csi/csi.sock"); err != nil || got != "/csi/csi.sock" {
		t.Fatalf("unixSocketPath = %q, %v", got, err)
	}
	if _, err := unixSocketPath("tcp://example"); err == nil {
		t.Fatal("non-unix endpoint accepted")
	}
	if sizes, err := parseSizes("2147483648,3221225472"); err != nil || len(sizes) != 2 || sizes[1] != 3221225472 {
		t.Fatalf("parseSizes = %v, %v", sizes, err)
	}
	if _, err := parseSizes("1,bad"); err == nil {
		t.Fatal("invalid size accepted")
	}
	params, err := parseParameters([]string{"thin_provision=on", "io_type=random"})
	if err != nil || params["thin_provision"] != "on" || params["io_type"] != "random" {
		t.Fatalf("parseParameters = %v, %v", params, err)
	}
}
