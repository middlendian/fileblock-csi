//go:build e2e

package e2e

import "testing"

// Pure, so it runs without a cluster: go test -tags=e2e -run TestPassiveSignal ./test/e2e/
func TestPassiveSignalKernel(t *testing.T) {
	for _, c := range []struct {
		release string
		want    bool
	}{
		{"6.11.0-29-generic", true},
		{"6.12.0", false},
		{"6.13.7-arch1-1", false},
		{"6.14.0-1015-generic", false},
		{"6.15.2", true},
		{"7.0.0-1020-generic", true},
		{"5.15.0-122-generic", true},
		{"6.14", false},
	} {
		got, ok := passiveSignalKernel(c.release)
		if !ok || got != c.want {
			t.Errorf("passiveSignalKernel(%q) = %v, %v; want %v, true", c.release, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "six.twelve", "6", "6.x"} {
		if _, ok := passiveSignalKernel(bad); ok {
			t.Errorf("passiveSignalKernel(%q) parsed", bad)
		}
	}
}
