package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MountInfo is one row of /proc/<pid>/mountinfo.
type MountInfo struct {
	MajorMinor   string
	MountPoint   string
	MountOptions []string
	FSType       string
	Source       string
	SuperOptions []string
}

// ParseMountInfo parses proc(5) mountinfo: fields 3 and 5-6, then the
// three fields after the "-" separator.
func ParseMountInfo(r io.Reader) ([]MountInfo, error) {
	var out []MountInfo
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		sep := slices.Index(f, "-")
		if sep < 6 || len(f) < sep+4 {
			return nil, fmt.Errorf("malformed mountinfo line: %q", sc.Text())
		}
		out = append(out, MountInfo{
			MajorMinor:   f[2],
			MountPoint:   unescapeOctal(f[4]),
			MountOptions: strings.Split(f[5], ","),
			FSType:       f[sep+1],
			Source:       unescapeOctal(f[sep+2]),
			SuperOptions: strings.Split(f[sep+3], ","),
		})
	}
	return out, sc.Err()
}

// ReadMountInfo parses the mountinfo file at path.
func ReadMountInfo(path string) ([]MountInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountInfo(f)
}

// ForcedReadOnly reports a filesystem that was mounted read-write and has
// since made its superblock read-only (ext4 after a journal abort).
func (m MountInfo) ForcedReadOnly() bool {
	return slices.Contains(m.MountOptions, "rw") && slices.Contains(m.SuperOptions, "ro")
}

// LastAt returns the last row mounted at mountPoint: the one visible there.
func LastAt(entries []MountInfo, mountPoint string) (MountInfo, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].MountPoint == mountPoint {
			return entries[i], true
		}
	}
	return MountInfo{}, false
}

// PublishTarget is a kubelet pod volume mount of a staged device.
type PublishTarget struct{ PodUID, Path string }

var podVolumePath = regexp.MustCompile(`/pods/([0-9a-f-]{36})/volumes/kubernetes\.io~csi/[^/]+/mount$`)

// PublishTargets lists the pod volume mounts on device majorMinor.
func PublishTargets(entries []MountInfo, majorMinor string) []PublishTarget {
	var out []PublishTarget
	for _, e := range entries {
		if e.MajorMinor != majorMinor {
			continue
		}
		if m := podVolumePath.FindStringSubmatch(e.MountPoint); m != nil {
			out = append(out, PublishTarget{PodUID: m[1], Path: e.MountPoint})
		}
	}
	return out
}

// unescapeOctal undoes the kernel's \ooo escaping of space, tab, newline
// and backslash.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
