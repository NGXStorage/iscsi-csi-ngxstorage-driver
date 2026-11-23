// csi-race is a test-only direct CSI concurrency helper. It deliberately sends
// multiple controller RPCs through a start barrier so Plan 11.1 race tests do
// not depend on sidecar scheduling to create overlap. It is never part of a
// production CSI image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const defaultEndpoint = "unix:///csi/csi.sock"

type callResult struct {
	Index         int    `json:"index"`
	Code          string `json:"code"`
	Error         string `json:"error,omitempty"`
	VolumeID      string `json:"volume_id,omitempty"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	CapacityBytes int64  `json:"capacity_bytes,omitempty"`
}

type summary struct {
	Operation          string       `json:"operation"`
	Results            []callResult `json:"results"`
	FinalCapacityBytes int64        `json:"final_capacity_bytes,omitempty"`
}

type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }
func (r *repeatedString) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var (
		out summary
		err error
	)
	switch os.Args[1] {
	case "create-volume":
		out, err = createVolumeCommand(ctx, os.Args[2:])
	case "delete-volume":
		out, err = deleteVolumeCommand(ctx, os.Args[2:])
	case "controller-unpublish":
		out, err = controllerUnpublishCommand(ctx, os.Args[2:])
	case "create-snapshot":
		out, err = createSnapshotCommand(ctx, os.Args[2:])
	case "delete-snapshot":
		out, err = deleteSnapshotCommand(ctx, os.Args[2:])
	case "controller-expand":
		out, err = controllerExpandCommand(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: csi-race <create-volume|delete-volume|create-snapshot|delete-snapshot|controller-expand|controller-unpublish> [flags]")
}

func createVolumeCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("create-volume", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	name := fs.String("name", "", "CSI volume name")
	capacity := fs.Int64("capacity-bytes", 1<<30, "required capacity in bytes")
	count := fs.Int("count", 2, "number of concurrent calls")
	fsType := fs.String("fs-type", "ext4", "filesystem type")
	modeText := fs.String("access-mode", "SINGLE_NODE_WRITER", "CSI access mode")
	snapshotID := fs.String("snapshot-id", "", "optional snapshot content source")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	var params repeatedString
	fs.Var(&params, "parameter", "volume parameter key=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "create-volume"}, err
	}
	if *name == "" || *count < 1 || *capacity < 1 {
		return summary{Operation: "create-volume"}, errors.New("name, positive count and positive capacity are required")
	}
	mode, err := parseAccessMode(*modeText)
	if err != nil {
		return summary{Operation: "create-volume"}, err
	}
	parameters, err := parseParameters(params)
	if err != nil {
		return summary{Operation: "create-volume"}, err
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "create-volume"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)

	results := runConcurrent(ctx, *count, func(callCtx context.Context, index int) callResult {
		req := &csi.CreateVolumeRequest{
			Name:          *name,
			CapacityRange: &csi.CapacityRange{RequiredBytes: *capacity},
			Parameters:    parameters,
			VolumeCapabilities: []*csi.VolumeCapability{{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: *fsType}},
			}},
		}
		if *snapshotID != "" {
			req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: *snapshotID},
			}}
		}
		resp, rpcErr := client.CreateVolume(callCtx, req)
		r := resultFromError(index, rpcErr)
		if resp != nil && resp.Volume != nil {
			r.VolumeID = resp.Volume.VolumeId
			r.CapacityBytes = resp.Volume.CapacityBytes
		}
		return r
	})
	out := summary{Operation: "create-volume", Results: results}
	if err := requireSuccessfulSameID(results, func(r callResult) string { return r.VolumeID }, "volume"); err != nil {
		return out, err
	}
	return out, nil
}

func deleteVolumeCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("delete-volume", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	volumeID := fs.String("volume-id", "", "CSI volume ID")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "delete-volume"}, err
	}
	if *volumeID == "" {
		return summary{Operation: "delete-volume"}, errors.New("volume-id is required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "delete-volume"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)
	_, rpcErr := client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: *volumeID})
	result := resultFromError(0, rpcErr)
	result.VolumeID = *volumeID
	out := summary{Operation: "delete-volume", Results: []callResult{result}}
	if rpcErr != nil {
		return out, fmt.Errorf("delete-volume failed: code=%s error=%s", result.Code, result.Error)
	}
	return out, nil
}

func controllerUnpublishCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("controller-unpublish", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	volumeID := fs.String("volume-id", "", "CSI volume ID")
	nodeID := fs.String("node-id", "", "CSI node ID")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "controller-unpublish"}, err
	}
	if *volumeID == "" || *nodeID == "" {
		return summary{Operation: "controller-unpublish"}, errors.New("volume-id and node-id are required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "controller-unpublish"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)
	_, rpcErr := client.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: *volumeID,
		NodeId:   *nodeID,
	})
	result := resultFromError(0, rpcErr)
	result.VolumeID = *volumeID
	out := summary{Operation: "controller-unpublish", Results: []callResult{result}}
	if rpcErr != nil {
		return out, fmt.Errorf("controller-unpublish failed: code=%s error=%s", result.Code, result.Error)
	}
	return out, nil
}

func createSnapshotCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("create-snapshot", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	name := fs.String("name", "", "CSI snapshot name")
	source := fs.String("source-volume-id", "", "source CSI volume ID")
	count := fs.Int("count", 2, "number of concurrent calls")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "create-snapshot"}, err
	}
	if *name == "" || *source == "" || *count < 1 {
		return summary{Operation: "create-snapshot"}, errors.New("name, source-volume-id and positive count are required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "create-snapshot"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)
	results := runConcurrent(ctx, *count, func(callCtx context.Context, index int) callResult {
		resp, rpcErr := client.CreateSnapshot(callCtx, &csi.CreateSnapshotRequest{Name: *name, SourceVolumeId: *source})
		r := resultFromError(index, rpcErr)
		if resp != nil && resp.Snapshot != nil {
			r.SnapshotID = resp.Snapshot.SnapshotId
			r.CapacityBytes = resp.Snapshot.SizeBytes
		}
		return r
	})
	out := summary{Operation: "create-snapshot", Results: results}
	if err := requireSuccessfulSameID(results, func(r callResult) string { return r.SnapshotID }, "snapshot"); err != nil {
		return out, err
	}
	return out, nil
}

func deleteSnapshotCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("delete-snapshot", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	snapshotID := fs.String("snapshot-id", "", "CSI snapshot ID")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "delete-snapshot"}, err
	}
	if *snapshotID == "" {
		return summary{Operation: "delete-snapshot"}, errors.New("snapshot-id is required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "delete-snapshot"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)
	_, rpcErr := client.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: *snapshotID})
	result := resultFromError(0, rpcErr)
	result.SnapshotID = *snapshotID
	out := summary{Operation: "delete-snapshot", Results: []callResult{result}}
	if rpcErr != nil {
		return out, fmt.Errorf("delete-snapshot failed: code=%s error=%s", result.Code, result.Error)
	}
	return out, nil
}

func controllerExpandCommand(parent context.Context, args []string) (summary, error) {
	fs := flag.NewFlagSet("controller-expand", flag.ContinueOnError)
	endpoint := fs.String("endpoint", defaultEndpoint, "CSI unix socket endpoint")
	volumeID := fs.String("volume-id", "", "CSI volume ID")
	sizesText := fs.String("sizes", "", "comma-separated requested capacities in bytes")
	fsType := fs.String("fs-type", "ext4", "filesystem type")
	modeText := fs.String("access-mode", "SINGLE_NODE_WRITER", "CSI access mode")
	timeout := fs.Duration("timeout", 60*time.Second, "RPC/dial timeout")
	if err := fs.Parse(args); err != nil {
		return summary{Operation: "controller-expand"}, err
	}
	if *volumeID == "" {
		return summary{Operation: "controller-expand"}, errors.New("volume-id is required")
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		return summary{Operation: "controller-expand"}, err
	}
	mode, err := parseAccessMode(*modeText)
	if err != nil {
		return summary{Operation: "controller-expand"}, err
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	conn, err := dialCSI(ctx, *endpoint)
	if err != nil {
		return summary{Operation: "controller-expand"}, err
	}
	defer conn.Close()
	client := csi.NewControllerClient(conn)
	results := runConcurrent(ctx, len(sizes), func(callCtx context.Context, index int) callResult {
		resp, rpcErr := client.ControllerExpandVolume(callCtx, &csi.ControllerExpandVolumeRequest{
			VolumeId:      *volumeID,
			CapacityRange: &csi.CapacityRange{RequiredBytes: sizes[index]},
			VolumeCapability: &csi.VolumeCapability{
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: *fsType}},
			},
		})
		r := resultFromError(index, rpcErr)
		if resp != nil {
			r.VolumeID = *volumeID
			r.CapacityBytes = resp.CapacityBytes
		}
		return r
	})
	out := summary{Operation: "controller-expand", Results: results}
	for _, result := range results {
		if result.Code != codes.OK.String() {
			return out, fmt.Errorf("controller-expand call %d failed: code=%s error=%s", result.Index, result.Code, result.Error)
		}
	}
	maxRequested := sizes[0]
	for _, size := range sizes[1:] {
		if size > maxRequested {
			maxRequested = size
		}
	}
	volume, getErr := client.ControllerGetVolume(ctx, &csi.ControllerGetVolumeRequest{VolumeId: *volumeID})
	if getErr != nil {
		return out, fmt.Errorf("controller-expand final readback failed: %w", getErr)
	}
	if volume.GetVolume() == nil {
		return out, errors.New("controller-expand final readback returned no volume")
	}
	out.FinalCapacityBytes = volume.GetVolume().GetCapacityBytes()
	if out.FinalCapacityBytes != maxRequested {
		return out, fmt.Errorf("controller-expand final capacity=%d, want exactly %d", out.FinalCapacityBytes, maxRequested)
	}
	return out, nil
}

func dialCSI(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
	path, err := unixSocketPath(endpoint)
	if err != nil {
		return nil, err
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	conn, err := grpc.DialContext(ctx, "passthrough:///csi-race",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial CSI endpoint %q: %w", endpoint, err)
	}
	return conn, nil
}

func unixSocketPath(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		endpoint = strings.TrimPrefix(endpoint, "unix://")
	case strings.HasPrefix(endpoint, "unix:"):
		endpoint = strings.TrimPrefix(endpoint, "unix:")
	}
	if endpoint == "" || endpoint[0] != '/' {
		return "", fmt.Errorf("CSI endpoint must resolve to an absolute unix socket path, got %q", endpoint)
	}
	return endpoint, nil
}

func runConcurrent(ctx context.Context, count int, fn func(context.Context, int) callResult) []callResult {
	results := make([]callResult, count)
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(count)
	done.Add(count)
	for i := 0; i < count; i++ {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			results[index] = fn(ctx, index)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results
}

func resultFromError(index int, err error) callResult {
	r := callResult{Index: index, Code: codes.OK.String()}
	if err == nil {
		return r
	}
	r.Code = status.Code(err).String()
	r.Error = err.Error()
	return r
}

func requireSuccessfulSameID(results []callResult, id func(callResult) string, label string) error {
	if len(results) == 0 {
		return errors.New("no RPC results")
	}
	want := ""
	for _, result := range results {
		if result.Code != codes.OK.String() {
			return fmt.Errorf("%s call %d failed: code=%s error=%s", label, result.Index, result.Code, result.Error)
		}
		got := id(result)
		if got == "" {
			return fmt.Errorf("%s call %d returned an empty id", label, result.Index)
		}
		if want == "" {
			want = got
		} else if got != want {
			return fmt.Errorf("%s IDs diverged: got=%q want=%q", label, got, want)
		}
	}
	return nil
}

func parseParameters(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("parameter must be key=value, got %q", value)
		}
		result[key] = val
	}
	return result, nil
}

func parseSizes(value string) ([]int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("sizes is required")
	}
	parts := strings.Split(value, ",")
	result := make([]int64, 0, len(parts))
	for _, part := range parts {
		size, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("invalid requested size %q", part)
		}
		result = append(result, size)
	}
	return result, nil
}

func parseAccessMode(value string) (csi.VolumeCapability_AccessMode_Mode, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if number, ok := csi.VolumeCapability_AccessMode_Mode_value[value]; ok {
		return csi.VolumeCapability_AccessMode_Mode(number), nil
	}
	return csi.VolumeCapability_AccessMode_UNKNOWN, fmt.Errorf("unsupported access mode %q", value)
}
