package driver

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

func TestGetPluginInfoReturnsConfiguredIdentity(t *testing.T) {
	d := &Driver{Name: DriverName, Version: DriverVersion}
	resp, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.GetName() != "iscsi.csi.ngxstorage.com" {
		t.Fatalf("name = %q, want canonical driver name", resp.GetName())
	}
	if resp.GetVendorVersion() != DriverVersion {
		t.Fatalf("vendor version = %q, want %q", resp.GetVendorVersion(), DriverVersion)
	}
	if len(resp.GetManifest()) != 0 {
		t.Fatalf("manifest must be empty, got %v", resp.GetManifest())
	}
}

func TestGetPluginInfoContextEnded(t *testing.T) {
	d := &Driver{Name: DriverName, Version: DriverVersion}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("error = %v, want Canceled", err)
	}
}

func TestGetPluginCapabilitiesAdvertisesControllerAndOnlineExpansion(t *testing.T) {
	d := &Driver{}
	resp, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	caps := resp.GetCapabilities()
	if len(caps) != 2 {
		t.Fatalf("capability count = %d, want exactly 2 (controller service + online expansion)", len(caps))
	}
	var controller, online bool
	for _, cap := range caps {
		switch svc := cap.GetType().(type) {
		case *csi.PluginCapability_Service_:
			if svc.Service.GetType() == csi.PluginCapability_Service_CONTROLLER_SERVICE {
				controller = true
			}
		case *csi.PluginCapability_VolumeExpansion_:
			if svc.VolumeExpansion.GetType() == csi.PluginCapability_VolumeExpansion_ONLINE {
				online = true
			}
		}
	}
	if !controller || !online {
		t.Fatalf("capabilities = %v, want CONTROLLER_SERVICE and ONLINE expansion", caps)
	}
}

func TestGetPluginCapabilitiesContextEnded(t *testing.T) {
	d := &Driver{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.GetPluginCapabilities(ctx, &csi.GetPluginCapabilitiesRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("error = %v, want Canceled", err)
	}
}

func TestProbeReflectsProcessReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		d := &Driver{Ready: ready}
		resp, err := d.Probe(context.Background(), &csi.ProbeRequest{})
		if err != nil {
			t.Fatalf("Probe(ready=%v): %v", ready, err)
		}
		if resp.GetReady().GetValue() != ready {
			t.Fatalf("Probe ready = %v, want %v", resp.GetReady().GetValue(), ready)
		}
	}
}

func TestProbeContextEnded(t *testing.T) {
	d := &Driver{Ready: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Probe(ctx, &csi.ProbeRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("error = %v, want Canceled", err)
	}
}

func TestResolveDriverName(t *testing.T) {
	if ResolveDriverName("") != "iscsi.csi.ngxstorage.com" {
		t.Fatal("empty prefix should preserve canonical name")
	}
	if ResolveDriverName("201") != "201.iscsi.csi.ngxstorage.com" {
		t.Fatal("prefix should prepend canonical name")
	}
	for _, prefix := range []string{`"201"`, "201.", " 201 ", "PREFIX"} {
		want := "201.iscsi.csi.ngxstorage.com"
		if prefix == "PREFIX" {
			want = "prefix.iscsi.csi.ngxstorage.com"
		}
		if got := ResolveDriverName(prefix); got != want {
			t.Fatalf("ResolveDriverName(%q) = %q, want %q", prefix, got, want)
		}
	}
}
