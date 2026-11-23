package helper

import (
	"context"
	"errors"
	"testing"

	kubeutils "github.com/NGXStorage/csi-ngxstorage-utils/kubernetes"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

func TestVolumeIDRoundtrip(t *testing.T) {
	const prefix = "iscsi.csi.ngxstorage.com."
	id := VolumeID(prefix, "abc123")
	if id != prefix+"abc123" {
		t.Fatalf("VolumeID = %q", id)
	}
	got, err := LUNIDFromVolumeIDChecked(prefix, id)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("roundtrip = %q", got)
	}
}

// A bare backend LUN ID (or prefix-only handle) is not this driver's volume:
// NotFound (CSI semantics); an empty ID is InvalidArgument.
func TestLUNIDFromVolumeIDCheckedRejectsInvalidPrefix(t *testing.T) {
	const prefix = "iscsi.csi.ngxstorage.com."
	for _, id := range []string{"abc123", prefix} {
		if _, err := LUNIDFromVolumeIDChecked(prefix, id); status.Code(err) != codes.NotFound {
			t.Fatalf("id %q: code = %s, want NotFound (err=%v)", id, status.Code(err), err)
		}
	}
	if _, err := LUNIDFromVolumeIDChecked(prefix, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id: code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestIsAmbiguousPV(t *testing.T) {
	if IsAmbiguousPV(errors.New("other")) {
		t.Fatal("plain error is not ambiguous")
	}
	var ambiguous kubeutils.ErrAmbiguousVolumePV
	if !IsAmbiguousPV(ambiguous) {
		t.Fatal("ErrAmbiguousVolumePV must classify")
	}
}

type fakeNodeLookup struct {
	exists bool
	err    error
}

func (f *fakeNodeLookup) NodeExists(context.Context, string) (bool, error) { return f.exists, f.err }
func (f *fakeNodeLookup) RemainingNodes(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeNodeLookup) PersistentVolumes(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeNodeLookup) VolumeAttachments(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeNodeLookup) Node(context.Context, string) (string, error) { return "", nil }

func TestAssertNodeExistsPolicy(t *testing.T) {
	// Non-NodeLookup attachments surface: no node verification, no error.
	if err := AssertNodeExists(context.Background(), nil, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := AssertNodeExists(context.Background(), &fakeNodeLookup{exists: true}, "n1"); err != nil {
		t.Fatal(err)
	}
	err := AssertNodeExists(context.Background(), &fakeNodeLookup{exists: false}, "n1")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("absent node -> %v", err)
	}
	err = AssertNodeExists(context.Background(), &fakeNodeLookup{err: errors.New("api down")}, "n1")
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unreadable cluster -> %v (never NotFound)", err)
	}
}
