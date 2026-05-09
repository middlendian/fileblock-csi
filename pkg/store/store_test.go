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
