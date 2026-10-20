package mount

import (
	"strings"
	"testing"
)

const sampleMountInfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
100 22 7:3 / /var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/abc/globalmount rw,relatime shared:50 - ext4 /dev/loop3 ro,errors=remount-ro
101 22 7:3 / /var/lib/kubelet/pods/11111111-2222-3333-4444-555555555555/volumes/kubernetes.io~csi/pvc-1/mount rw,relatime shared:50 - ext4 /dev/loop3 ro,errors=remount-ro
102 22 7:3 / /var/lib/kubelet/pods/66666666-7777-8888-9999-000000000000/volumes/kubernetes.io~csi/pvc-1/mount ro,relatime shared:50 - ext4 /dev/loop3 ro,errors=remount-ro
103 22 7:4 / /var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/def/globalmount rw,relatime - ext4 /dev/loop4 rw
104 22 7:5 / /var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/ro-on-purpose/globalmount ro,relatime - ext4 /dev/loop5 ro
105 22 7:6 / /var/lib/kubelet/plugins/with\040space/globalmount rw - ext4 /dev/loop6 ro
`

func parseSample(t *testing.T) []MountInfo {
	t.Helper()
	ms, err := ParseMountInfo(strings.NewReader(sampleMountInfo))
	if err != nil {
		t.Fatalf("ParseMountInfo: %v", err)
	}
	return ms
}

func TestParseMountInfoFields(t *testing.T) {
	ms := parseSample(t)
	if len(ms) != 7 {
		t.Fatalf("got %d entries, want 7", len(ms))
	}
	m := ms[1]
	if m.MajorMinor != "7:3" || m.FSType != "ext4" || m.Source != "/dev/loop3" {
		t.Fatalf("unexpected entry: %+v", m)
	}
	if m.MountOptions[0] != "rw" || m.SuperOptions[0] != "ro" {
		t.Fatalf("options: %+v", m)
	}
}

func TestForcedReadOnly(t *testing.T) {
	ms := parseSample(t)
	cases := map[string]bool{
		"/var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/abc/globalmount":           true,
		"/var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/def/globalmount":           false,
		"/var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/ro-on-purpose/globalmount": false,
	}
	for mp, want := range cases {
		m, ok := LastAt(ms, mp)
		if !ok {
			t.Fatalf("%s not found", mp)
		}
		if got := m.ForcedReadOnly(); got != want {
			t.Errorf("%s: ForcedReadOnly=%v want %v", mp, got, want)
		}
	}
}

func TestOctalEscapedMountPoint(t *testing.T) {
	ms := parseSample(t)
	m, ok := LastAt(ms, "/var/lib/kubelet/plugins/with space/globalmount")
	if !ok || !m.ForcedReadOnly() {
		t.Fatalf("escaped mount point not matched: %+v ok=%v", m, ok)
	}
}

func TestLastAtUsesLastRow(t *testing.T) {
	stacked := `1 0 7:1 / /stage rw - ext4 /dev/loop1 ro
2 0 7:2 / /stage rw - ext4 /dev/loop2 rw
`
	ms, err := ParseMountInfo(strings.NewReader(stacked))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := LastAt(ms, "/stage")
	if m.Source != "/dev/loop2" || m.ForcedReadOnly() {
		t.Fatalf("want the visible (last) row, got %+v", m)
	}
}

func TestPublishTargets(t *testing.T) {
	ms := parseSample(t)
	got := PublishTargets(ms, "7:3")
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	if got[0].PodUID != "11111111-2222-3333-4444-555555555555" ||
		got[1].PodUID != "66666666-7777-8888-9999-000000000000" {
		t.Fatalf("pod UIDs: %+v", got)
	}
	if len(PublishTargets(ms, "7:4")) != 0 {
		t.Fatal("a staging mount is not a publish target")
	}
}

func TestParseMountInfoRejectsMalformed(t *testing.T) {
	if _, err := ParseMountInfo(strings.NewReader("1 2 3\n")); err == nil {
		t.Fatal("want error for a line without the separator")
	}
}
