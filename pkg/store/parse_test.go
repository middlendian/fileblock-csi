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

func TestVolumeContextRoundTripNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	vc := c.ToVolumeContext()
	if vc[ParamType] != "nfs" {
		t.Errorf("vc[%s] = %q", ParamType, vc[ParamType])
	}
	if vc[VolumeContextStoreID] != c.StoreID() {
		t.Errorf("vc[storeID] = %q, want %q", vc[VolumeContextStoreID], c.StoreID())
	}
	got, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if got != c {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, c)
	}
}

func TestVolumeContextRoundTripLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	vc := c.ToVolumeContext()
	got, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if got != c {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, c)
	}
}

func TestVolumeContextOmitsEmptyMountOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	vc := c.ToVolumeContext()
	if _, present := vc[ParamNFSMountOptions]; present {
		t.Errorf("mountOptions key should be absent when empty, got %+v", vc)
	}
}

func nfsParams(extra map[string]string) map[string]string {
	p := map[string]string{
		"backingStore.type":       "nfs",
		"backingStore.nfs.server": "nfs.example.internal",
		"backingStore.nfs.path":   "/exports/k8s_ns",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestConfigFromParamsSubDirAbsentIsEmpty(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(nil))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "" {
		t.Errorf("NFSSubDir = %q, want empty", c.NFSSubDir)
	}
}

func TestConfigFromParamsSubDirLiteral(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "team-a/fileblock",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "team-a/fileblock" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "team-a/fileblock")
	}
}

func TestConfigFromParamsSubDirSubstitutesNamespace(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":          "${pvc.namespace}/fileblock",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "team-a/fileblock" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "team-a/fileblock")
	}
}

func TestConfigFromParamsSubDirSubstitutesPVCAndPVName(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":     "${pvc.name}/${pv.name}",
		"csi.storage.k8s.io/pvc/name": "my-claim",
		"csi.storage.k8s.io/pv/name":  "pv-123",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "my-claim/pv-123" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "my-claim/pv-123")
	}
}

// Without --extra-create-metadata=true the metadata keys are absent, the
// token survives, and every namespace would land in one shared directory
// whose name looks like a template. That is the exact isolation failure
// the parameter exists to prevent, so it is fatal.
func TestConfigFromParamsSubDirUnresolvedTokenIsFatal(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "${pvc.namespace}/fileblock",
	}))
	if err == nil {
		t.Fatal("expected an error for an unresolved token")
	}
	for _, want := range []string{
		"backingStore.nfs.subDir",
		"${pvc.namespace}",
		"--extra-create-metadata=true",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The v0.4.0 spelling is gone, not aliased. It must fail loudly and the
// message must show the operator the spelling that replaced it.
func TestConfigFromParamsSubDirOldSpellingIsFatal(t *testing.T) {
	for _, old := range []string{"${pvc.metadata.namespace}", "${pvc.metadata.name}", "${pv.metadata.name}"} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir":          old + "/fileblock",
			"csi.storage.k8s.io/pvc/namespace": "team-a",
			"csi.storage.k8s.io/pvc/name":      "my-claim",
			"csi.storage.k8s.io/pv/name":       "pv-123",
		}))
		if err == nil {
			t.Fatalf("%s: expected an error for the old spelling", old)
		}
		if !strings.Contains(err.Error(), old) {
			t.Errorf("%s: error %q should quote the unresolved token", old, err)
		}
		if !strings.Contains(err.Error(), "${pvc.namespace}") {
			t.Errorf("%s: error %q should list the supported tokens", old, err)
		}
		if !strings.Contains(err.Error(), "v0.5.0") {
			t.Errorf("%s: error %q should hint at the v0.5.0 rename", old, err)
		}
	}
}

func TestConfigFromParamsSubDirRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"../escape",
		"team-a/../../escape",
		"a/../b",
	} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": bad,
		}))
		if err == nil {
			t.Errorf("subDir %q was accepted; traversal defeats the isolation the feature provides", bad)
		}
	}
}

func TestConfigFromParamsSubDirRejectsAbsolute(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "/exports/elsewhere",
	}))
	if err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("expected a 'relative' error, got %v", err)
	}
}

func TestConfigFromParamsSubDirRejectsDot(t *testing.T) {
	for _, bad := range []string{".", "./"} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": bad,
		}))
		if err == nil {
			t.Errorf("subDir %q was accepted; it names the mount root under a distinct StoreID", bad)
		}
	}
}

func TestConfigFromParamsSubDirRejectsNUL(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "team-a\x00/fileblock",
	}))
	if err == nil {
		t.Fatal("expected an error for a NUL byte in subDir")
	}
}

// Trailing slashes and a leading ./ must not mint separate StoreIDs for
// one directory.
func TestConfigFromParamsSubDirNormalizes(t *testing.T) {
	var ids []string
	for _, in := range []string{"team-a/fileblock", "team-a/fileblock/", "./team-a/fileblock"} {
		c, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir": in,
		}))
		if err != nil {
			t.Fatalf("ConfigFromParams(%q): %v", in, err)
		}
		if c.NFSSubDir != "team-a/fileblock" {
			t.Errorf("subDir %q normalized to %q, want %q", in, c.NFSSubDir, "team-a/fileblock")
		}
		ids = append(ids, c.StoreID())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("normalization produced distinct StoreIDs: %v", ids)
			break
		}
	}
}

func TestConfigFromParamsSubDirRejectedForLocal(t *testing.T) {
	_, err := ConfigFromParams(map[string]string{
		"backingStore.type":       "local",
		"backingStore.local.path": "/var/lib/fileblock",
		"backingStore.nfs.subDir": "team-a",
	})
	if err == nil || !strings.Contains(err.Error(), "backingStore.nfs.subDir") {
		t.Fatalf("expected a subDir-not-supported error, got %v", err)
	}
}

// The node receives an already-resolved subDir in volume context and must
// re-parse it without metadata keys present.
func TestVolumeContextRoundTripsSubDir(t *testing.T) {
	c := Config{
		Type:      TypeNFS,
		NFSServer: "nfs.example.internal",
		NFSPath:   "/exports/k8s_ns",
		NFSSubDir: "team-a/fileblock",
	}
	vc := c.ToVolumeContext()
	if vc["backingStore.nfs.subDir"] != "team-a/fileblock" {
		t.Fatalf("volume context subDir = %q", vc["backingStore.nfs.subDir"])
	}
	back, err := ConfigFromVolumeContext(vc)
	if err != nil {
		t.Fatalf("ConfigFromVolumeContext: %v", err)
	}
	if back.NFSSubDir != c.NFSSubDir {
		t.Errorf("subDir round-trip: %q -> %q", c.NFSSubDir, back.NFSSubDir)
	}
	if back.StoreID() != c.StoreID() {
		t.Errorf("StoreID round-trip: %q -> %q", c.StoreID(), back.StoreID())
	}
}

func TestVolumeContextOmitsEmptySubDir(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	if _, ok := c.ToVolumeContext()["backingStore.nfs.subDir"]; ok {
		t.Error("volume context must omit subDir when it is empty")
	}
}
