package store

import "testing"

func TestTypeConstants(t *testing.T) {
	if TypeNFS != "nfs" {
		t.Errorf("TypeNFS = %q, want %q", TypeNFS, "nfs")
	}
	if TypeLocal != "local" {
		t.Errorf("TypeLocal = %q, want %q", TypeLocal, "local")
	}
}

func TestConfigZero(t *testing.T) {
	var c Config
	if c.Type != "" {
		t.Errorf("zero Config.Type = %q, want empty", c.Type)
	}
}

func TestMountKeyNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	got := c.mountKey()
	want := "nfs|nfs.example.internal|/exports/fileblock|hard,nfsvers=4.1,timeo=600"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestMountKeyNFSReordersOptions(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=4.1,hard,timeo=600"}
	b := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=4.1,timeo=600"}
	if a.mountKey() != b.mountKey() {
		t.Error("differently-ordered mountOptions must canonicalize identically")
	}
}

func TestMountKeyNFSDropsEmptyOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: ",hard,,nfsvers=3,"}
	got := c.mountKey()
	want := "nfs|s|/p|hard,nfsvers=3"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestMountKeyLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	got := c.mountKey()
	want := "local|/var/lib/fileblock-store"
	if got != want {
		t.Errorf("mountKey = %q\n  want %q", got, want)
	}
}

func TestStoreIDIsDeterministicAndShort(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	id1 := c.StoreID()
	id2 := c.StoreID()
	if id1 != id2 {
		t.Errorf("StoreID not deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != 12 {
		t.Errorf("StoreID length = %d, want 12", len(id1))
	}
	if len(c.MountID()) != 12 {
		t.Errorf("MountID length = %d, want 12", len(c.MountID()))
	}
}

func TestStoreIDDiffersForDifferentConfigs(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	if a.StoreID() == b.StoreID() {
		t.Error("StoreIDs collide for distinct configs")
	}
}

// TestStoreIDPinnedLegacyValue pins a storeID computed from the v0.3.8
// tag, before subDir existed:
//
//	printf 'nfs|nfs.example.internal|/exports/k8s_ns|' | sha256sum | cut -c1-12
//
// If this fails, the canonical form of a no-subDir config has changed and
// every volumeID issued by an earlier release now resolves to a store that
// does not exist. DeleteVolume would return OK per the CSI idempotency
// rule and leave the .img orphaned on the export forever. Do not
// regenerate this value from the working tree — that pins the bug.
func TestStoreIDPinnedLegacyValue(t *testing.T) {
	c := Config{
		Type:      TypeNFS,
		NFSServer: "nfs.example.internal",
		NFSPath:   "/exports/k8s_ns",
	}
	const want = "2a355b61d5f7"
	if got := c.StoreID(); got != want {
		t.Errorf("StoreID = %q, want %q (v0.3.8 value)", got, want)
	}
	if got := c.MountID(); got != want {
		t.Errorf("MountID = %q, want %q (v0.3.8 value)", got, want)
	}
}

// TestStoreIDEqualsMountIDWithoutSubDir is the compatibility invariant
// that the NFSSubDir == "" branch of storeKey exists to hold. Without
// this test the branch looks like dead code.
func TestStoreIDEqualsMountIDWithoutSubDir(t *testing.T) {
	for _, c := range []Config{
		{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"},
		{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=3"},
		{Type: TypeLocal, LocalPath: "/var/lib/fileblock"},
	} {
		if c.StoreID() != c.MountID() {
			t.Errorf("%+v: StoreID %q != MountID %q with no subDir", c, c.StoreID(), c.MountID())
		}
	}
}

func TestSubDirChangesStoreIDNotMountID(t *testing.T) {
	base := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	a := base
	a.NFSSubDir = "ns-a/fileblock"
	b := base
	b.NFSSubDir = "ns-b/fileblock"

	if a.StoreID() == b.StoreID() {
		t.Error("distinct subDirs must produce distinct StoreIDs")
	}
	if a.StoreID() == base.StoreID() {
		t.Error("a subDir must change the StoreID")
	}
	if a.MountID() != b.MountID() || a.MountID() != base.MountID() {
		t.Errorf("subDir must not change MountID: %q %q %q",
			base.MountID(), a.MountID(), b.MountID())
	}
}
