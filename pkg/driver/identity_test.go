package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestGetPluginInfo(t *testing.T) {
	id := NewIdentityServer(true)
	resp, err := id.GetPluginInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.Name != DriverName {
		t.Fatalf("Name=%q want %q", resp.Name, DriverName)
	}
	if resp.VendorVersion == "" {
		t.Fatal("VendorVersion empty")
	}
}

func TestGetPluginCapabilities(t *testing.T) {
	id := NewIdentityServer(true)
	resp, err := id.GetPluginCapabilities(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	wantSvc := map[csi.PluginCapability_Service_Type]bool{
		csi.PluginCapability_Service_CONTROLLER_SERVICE:               false,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS: false,
	}
	gotExpand := false
	for _, c := range resp.Capabilities {
		if s := c.GetService(); s != nil {
			if _, ok := wantSvc[s.Type]; ok {
				wantSvc[s.Type] = true
			}
		}
		if e := c.GetVolumeExpansion(); e != nil && e.Type == csi.PluginCapability_VolumeExpansion_OFFLINE {
			gotExpand = true
		}
	}
	for k, ok := range wantSvc {
		if !ok {
			t.Errorf("missing service capability %s", k)
		}
	}
	if !gotExpand {
		t.Error("missing OFFLINE volume expansion capability")
	}
}

func TestProbe(t *testing.T) {
	id := NewIdentityServer(false)
	resp, err := id.Probe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if resp.GetReady() == nil || !resp.GetReady().GetValue() {
		t.Fatalf("Probe.Ready=%v, want true", resp.GetReady())
	}
}
