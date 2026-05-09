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

func TestCanonicalNFS(t *testing.T) {
	c := Config{
		Type:            TypeNFS,
		NFSServer:       "nfs.example.internal",
		NFSPath:         "/exports/fileblock",
		NFSMountOptions: "nfsvers=4.1,hard,timeo=600",
	}
	got := string(c.Canonical())
	want := "nfs|nfs.example.internal|/exports/fileblock|hard,nfsvers=4.1,timeo=600"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestCanonicalNFSReordersOptions(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "nfsvers=4.1,hard,timeo=600"}
	b := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: "hard,nfsvers=4.1,timeo=600"}
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Error("differently-ordered mountOptions must canonicalize to the same bytes")
	}
}

func TestCanonicalNFSDropsEmptyOptions(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p", NFSMountOptions: ",hard,,nfsvers=3,"}
	got := string(c.Canonical())
	want := "nfs|s|/p|hard,nfsvers=3"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestCanonicalLocal(t *testing.T) {
	c := Config{Type: TypeLocal, LocalPath: "/var/lib/fileblock-store"}
	got := string(c.Canonical())
	want := "local|/var/lib/fileblock-store"
	if got != want {
		t.Errorf("Canonical = %q\n  want %q", got, want)
	}
}

func TestIDIsDeterministicAndShort(t *testing.T) {
	c := Config{Type: TypeNFS, NFSServer: "s", NFSPath: "/p"}
	id1 := c.ID()
	id2 := c.ID()
	if id1 != id2 {
		t.Errorf("ID not deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != 12 {
		t.Errorf("ID length = %d, want 12", len(id1))
	}
}

func TestIDDiffersForDifferentConfigs(t *testing.T) {
	a := Config{Type: TypeNFS, NFSServer: "s1", NFSPath: "/p"}
	b := Config{Type: TypeNFS, NFSServer: "s2", NFSPath: "/p"}
	if a.ID() == b.ID() {
		t.Error("IDs collide for distinct configs")
	}
}
