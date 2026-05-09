package store

import (
	"strings"
	"testing"
)

func TestConfigFromParamsNFS(t *testing.T) {
	in := map[string]string{
		"backingStore.type":             "nfs",
		"backingStore.nfs.server":       "nfs.example.internal",
		"backingStore.nfs.path":         "/exports/fileblock",
		"backingStore.nfs.mountOptions": "nfsvers=4.1,hard,timeo=600",
	}
	c, err := ConfigFromParams(in)
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.Type != TypeNFS {
		t.Errorf("Type = %q, want %q", c.Type, TypeNFS)
	}
	if c.NFSServer != "nfs.example.internal" || c.NFSPath != "/exports/fileblock" {
		t.Errorf("server/path mismatch: %+v", c)
	}
	if c.NFSMountOptions != "nfsvers=4.1,hard,timeo=600" {
		t.Errorf("mountOptions = %q", c.NFSMountOptions)
	}
}

func TestConfigFromParamsNFSMissingServer(t *testing.T) {
	in := map[string]string{"backingStore.type": "nfs", "backingStore.nfs.path": "/p"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.server") {
		t.Fatalf("expected error mentioning backingStore.nfs.server, got %v", err)
	}
}

func TestConfigFromParamsNFSMissingPath(t *testing.T) {
	in := map[string]string{"backingStore.type": "nfs", "backingStore.nfs.server": "x"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.path") {
		t.Fatalf("expected error mentioning backingStore.nfs.path, got %v", err)
	}
}

func TestConfigFromParamsLocal(t *testing.T) {
	in := map[string]string{"backingStore.type": "local", "backingStore.local.path": "/var/lib/fileblock-store"}
	c, err := ConfigFromParams(in)
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.Type != TypeLocal || c.LocalPath != "/var/lib/fileblock-store" {
		t.Errorf("got %+v", c)
	}
}

func TestConfigFromParamsLocalMissingPath(t *testing.T) {
	in := map[string]string{"backingStore.type": "local"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "backingStore.local.path") {
		t.Fatalf("expected error mentioning backingStore.local.path, got %v", err)
	}
}

func TestConfigFromParamsMissingType(t *testing.T) {
	_, err := ConfigFromParams(map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "backingStore.type") {
		t.Fatalf("expected error mentioning backingStore.type, got %v", err)
	}
}

func TestConfigFromParamsUnknownType(t *testing.T) {
	in := map[string]string{"backingStore.type": "smb"}
	_, err := ConfigFromParams(in)
	if err == nil || !strings.Contains(err.Error(), "smb") {
		t.Fatalf("expected error mentioning unknown type smb, got %v", err)
	}
}
