package driver

import "testing"

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		in         string
		wantScheme string
		wantAddr   string
		wantErr    bool
	}{
		{"unix:///csi/csi.sock", "unix", "/csi/csi.sock", false},
		{"/csi/csi.sock", "unix", "/csi/csi.sock", false},
		{"tcp://127.0.0.1:9000", "tcp", "127.0.0.1:9000", false},
		{"http://example.com", "", "", true},
	}
	for _, c := range cases {
		scheme, addr, err := parseEndpoint(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseEndpoint(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if scheme != c.wantScheme || addr != c.wantAddr {
			t.Errorf("parseEndpoint(%q) = (%q,%q), want (%q,%q)",
				c.in, scheme, addr, c.wantScheme, c.wantAddr)
		}
	}
}
