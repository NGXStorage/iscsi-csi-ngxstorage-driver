package helper

import (
	"reflect"
	"testing"
)

func TestISCSISessionStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := ISCSISessionState{
		TargetIQN: "iqn.test",
		Portals:   []string{"10.0.0.1", "10.0.0.2"},
		SCSIID:    "589c005636424757595041fcbe15d9d9",
	}
	if err := SaveISCSISessionState(dir, "volume-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadISCSISessionState(dir, "volume-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	if err := DeleteISCSISessionState(dir, "volume-1"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteISCSISessionState(dir, "volume-1"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}
