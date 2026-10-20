# Failed-volume remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `fileblock-node` detects staged volumes whose ext4 was forced read-only, reports them, and (by default) deletes the controller-owned pods using them on its own node, re-staging internally when the replacement lands on the same node.

**Architecture:**
- A passive monitor goroutine reads `/proc/self/mountinfo` every 10s. It records failed volumes in an in-memory tracker that `NodeGetVolumeStats` and `NodePublishVolume` also read.
- Deletion goes through a small hand-rolled Kubernetes REST client: in-cluster token and CA, no client-go. That matches the repo's small-dependency stance (`pkg/mount` shells out rather than import `k8s.io/mount-utils`).
- A ValidatingAdmissionPolicy in `deploy/kustomize/base` limits the plugin's deletes to its own node, controller-owned pods, and pods with a PVC.

**Tech Stack:** Go 1.25, CSI spec v1.12, `net/http` against the kube-apiserver REST API, `github.com/google/cel-go` (tests only), kustomize, kind e2e.

**Spec:** `docs/superpowers/specs/2026-10-19-failed-volume-remediation-design.md`

## Global Constraints

- Flag: `--auto-delete-pod-when-volume-failed`, bool, default `true`. It is the only new flag.
- Constants: check interval `10 * time.Second`; per-volume hold-off `5 * time.Minute`, which does not double.
- Trigger: the staging mount's per-mount options contain `rw` **and** its super options contain `ro`.
- Detection is passive. No read or stat of the loop device, the `.img`, or anything on the backing store.
- Deleting a pod requires all of the following:
  - a controlling owner reference;
  - no `deletionTimestamp`;
  - `status.startTime` set and before `failedAt`;
  - the delete carries `preconditions.uid`;
  - the pod's own grace period (no `gracePeriodSeconds` in the request).
- With the flag on, deletion happens only if the VAP `fileblock-node-pod-delete` and the binding `fileblock-node-pod-delete` both exist and the binding's `validationActions` contains `Deny`. Otherwise the plugin logs an error and only detects and reports.
- Event reason `FileblockVolumeFailed`, type `Warning`, on each publishing pod.
- `VolumeCondition{Abnormal: true}` from `NodeGetVolumeStats` for a failed volume; node capability `VOLUME_CONDITION`.
- `delete` mode needs Kubernetes 1.30+. Detection and reporting need nothing new.
- No new runtime Go module dependencies. `cel-go` is allowed only in `_test.go` files.
- Every shell-out goes through `pkg/exec.Runner`. No `panic` in handlers. Comments follow CLAUDE.md: one short comment per non-obvious block.
- **Pseudonymous project.** Code, comments, docs, commits and tests use generic examples only: no real hostnames, IPs, org or repo names other than `middlendian/fileblock-csi`.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never amend, rebase or force-push.
- Before each push, run `make fmt-check vet lint tidy-check test build` (`make check` needs root and loop devices).

## Review Focus

1. **Octal escapes in mountinfo mount points** (`\040` for a space, `\011`, `\012`, `\134`). A kubelet path containing a space must still match its staging path. Task 1 pins it.
2. **Several mountinfo rows for one staging path** (stacked mounts from older versions). The trigger must use the **last** row, the one visible at that path, as `mount.Source` does. Task 1 pins it.
3. **A plugin restart while a volume is already failed.** The monitor must rediscover the failure from mountinfo with a fresh `failedAt`. Pods started before restart + detection still qualify, because their `startTime` is older. Task 4 pins it.
4. **The API server is unreachable or returns 5xx during a tick.** The tick must log and continue: no crash, no hold-off consumed, a retry next tick. Task 4 pins it.
5. **NodePublishVolume for a failed volume whose other target is the very target being published** (a kubelet retry after a partial publish). The check must exclude `req.TargetPath` itself. Task 5 pins it.

---

### Task 1: mountinfo parsing (`pkg/mount/mountinfo.go`)

**Files:**
- Create: `pkg/mount/mountinfo.go`
- Test: `pkg/mount/mountinfo_test.go`

**Interfaces:**
- Produces:
  ```go
  type MountInfo struct {
      MajorMinor   string   // "7:3"
      MountPoint   string   // unescaped
      MountOptions []string // per-mount, e.g. ["rw","relatime"]
      FSType       string
      Source       string
      SuperOptions []string // e.g. ["ro","errors=remount-ro"]
  }
  func ParseMountInfo(r io.Reader) ([]MountInfo, error)
  func ReadMountInfo(path string) ([]MountInfo, error) // path is usually "/proc/self/mountinfo"
  func (m MountInfo) ForcedReadOnly() bool            // "rw" in MountOptions && "ro" in SuperOptions
  func LastAt(entries []MountInfo, mountPoint string) (MountInfo, bool)
  type PublishTarget struct{ PodUID, Path string }
  func PublishTargets(entries []MountInfo, majorMinor string) []PublishTarget
  ```

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./pkg/mount/ -run 'MountInfo|ForcedReadOnly|LastAt|PublishTargets|Octal' -v`
Expected: FAIL. The build fails with `undefined: ParseMountInfo`.

- [ ] **Step 3: Implement**

```go
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
```

- [ ] **Step 4: Run them and see them pass**

Run: `go test ./pkg/mount/ -v`
Expected: PASS, including the existing `mount_test.go`.

- [ ] **Step 5: Commit**

```bash
git add pkg/mount/mountinfo.go pkg/mount/mountinfo_test.go
git commit -m "mount: parse mountinfo; detect forced read-only and pod publish targets

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Failure tracker and VolumeCondition

**Files:**
- Create: `pkg/driver/failures.go`
- Test: `pkg/driver/failures_test.go`
- Modify: `pkg/driver/node.go`. Add the `failures` field to `NodeServer` and init it in `NewNodeServer`; add `VOLUME_CONDITION` to `NodeGetCapabilities`; set `VolumeCondition` in `NodeGetVolumeStats`; call `n.failures.Clear(volumeID)` at the end of a successful `NodeStageVolume` and `NodeUnstageVolume`.
- Test: `pkg/driver/node_test.go`. Extend `TestNodeGetCapabilities`'s `want` map with `csi.NodeServiceCapability_RPC_VOLUME_CONDITION: false`.

**Interfaces:**
- Produces:
  ```go
  type failures struct { /* mutex + map */ }
  func newFailures() *failures
  // Mark records volumeID as failed at t; it returns true only the first time.
  func (f *failures) Mark(volumeID string, t time.Time) bool
  func (f *failures) Get(volumeID string) (failedAt time.Time, ok bool)
  func (f *failures) Clear(volumeID string)
  // Hold-off, kept separately from the failed state so Clear doesn't reset it.
  func (f *failures) HeldUntil(volumeID string) time.Time
  func (f *failures) Hold(volumeID string, until time.Time)
  ```
  `NodeServer` gains the field `failures *failures`.

- [ ] **Step 1: Write the failing tests**

```go
package driver

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestFailuresMarkOnce(t *testing.T) {
	f := newFailures()
	t0 := time.Unix(1000, 0)
	if !f.Mark("v", t0) {
		t.Fatal("first Mark should report new")
	}
	if f.Mark("v", t0.Add(time.Minute)) {
		t.Fatal("second Mark should not report new")
	}
	if at, ok := f.Get("v"); !ok || !at.Equal(t0) {
		t.Fatalf("failedAt = %v ok=%v, want %v", at, ok, t0)
	}
}

func TestFailuresClearKeepsHold(t *testing.T) {
	f := newFailures()
	until := time.Unix(2000, 0)
	f.Mark("v", time.Unix(1000, 0))
	f.Hold("v", until)
	f.Clear("v")
	if _, ok := f.Get("v"); ok {
		t.Fatal("Clear should drop the failed state")
	}
	if !f.HeldUntil("v").Equal(until) {
		t.Fatal("Clear must not reset the hold-off")
	}
}

func TestNodeGetVolumeStatsReportsCondition(t *testing.T) {
	n := newStatsTestNode(t) // helper below
	n.failures.Mark("vol-1", time.Now())
	resp, err := n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "vol-1", VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	if c := resp.GetVolumeCondition(); c == nil || !c.Abnormal || c.Message == "" {
		t.Fatalf("want an abnormal condition with a message, got %+v", c)
	}
}

func TestNodeGetVolumeStatsHealthyCondition(t *testing.T) {
	n := newStatsTestNode(t)
	resp, err := n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "vol-1", VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := resp.GetVolumeCondition(); c == nil || c.Abnormal {
		t.Fatalf("want a normal condition, got %+v", c)
	}
}

// newStatsTestNode builds a NodeServer whose state file has vol-1 staged.
func newStatsTestNode(t *testing.T) *NodeServer {
	t.Helper()
	st, err := loop.LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop9", StagePath: "/stage"}); err != nil {
		t.Fatal(err)
	}
	return NewNodeServer("n", nil, nil, nil, st, discardLog(), newTestNodeRegistry(t))
}
```

(Add `"path/filepath"` and `"github.com/middlendian/fileblock-csi/pkg/loop"` to the test imports.)

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./pkg/driver/ -run 'Failures|VolumeStats|Capabilities' -v`
Expected: FAIL with `undefined: newFailures`.

- [ ] **Step 3: Implement `failures.go`**

```go
package driver

import (
	"sync"
	"time"
)

// failures holds volumes whose ext4 was forced read-only, and each
// volume's remediation hold-off. In memory only: a plugin restart
// rediscovers failures from mountinfo.
type failures struct {
	mu     sync.Mutex
	failed map[string]time.Time
	held   map[string]time.Time
}

func newFailures() *failures {
	return &failures{failed: map[string]time.Time{}, held: map[string]time.Time{}}
}

func (f *failures) Mark(volumeID string, t time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.failed[volumeID]; ok {
		return false
	}
	f.failed[volumeID] = t
	return true
}

func (f *failures) Get(volumeID string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.failed[volumeID]
	return t, ok
}

func (f *failures) Clear(volumeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failed, volumeID)
}

func (f *failures) HeldUntil(volumeID string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[volumeID]
}

func (f *failures) Hold(volumeID string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[volumeID] = until
}
```

- [ ] **Step 4: Wire it into `node.go`**

In `NodeServer`, add `failures *failures` and set `failures: newFailures()` in `NewNodeServer`. Add `rpc(csi.NodeServiceCapability_RPC_VOLUME_CONDITION)` to the capability list. Then, in `NodeGetVolumeStats`, replace the final `return` with:

```go
	cond := &csi.VolumeCondition{Abnormal: false, Message: "volume is healthy"}
	if at, failed := n.failures.Get(volumeID); failed {
		cond = &csi.VolumeCondition{
			Abnormal: true,
			Message: "ext4 forced the staged filesystem read-only at " + at.UTC().Format(time.RFC3339) +
				"; the backing file is unusable until the volume is re-staged",
		}
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: total, Available: free, Used: used},
			{Unit: csi.VolumeUsage_INODES, Total: int64(st.Files), Available: int64(st.Ffree), Used: int64(st.Files - st.Ffree)},
		},
		VolumeCondition: cond,
	}, nil
```

Add `"time"` to the imports. At the end of `NodeStageVolume`, after `state.Put` succeeds and before `return`, add `n.failures.Clear(volumeID)`. Do the same in `NodeUnstageVolume`, before its final return. Do the same at the end of `adoptStage` too: an adopted mount gets re-examined by the next tick.

- [ ] **Step 5: Run the tests and see them pass**

Run: `go test ./pkg/driver/ -v`
Expected: PASS, including every existing node test.

- [ ] **Step 6: Commit**

```bash
git add pkg/driver/failures.go pkg/driver/failures_test.go pkg/driver/node.go pkg/driver/node_test.go
git commit -m "driver: track failed volumes and report VolumeCondition

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Minimal Kubernetes client (`pkg/kube`)

**Files:**
- Create: `pkg/kube/client.go`, `pkg/kube/types.go`
- Test: `pkg/kube/client_test.go`

**Interfaces:**
- Produces:
  ```go
  type OwnerReference struct {
      APIVersion string `json:"apiVersion"`
      Kind       string `json:"kind"`
      Name       string `json:"name"`
      Controller *bool  `json:"controller,omitempty"`
  }
  type Pod struct {
      Metadata struct {
          Name              string           `json:"name"`
          Namespace         string           `json:"namespace"`
          UID               string           `json:"uid"`
          DeletionTimestamp *time.Time       `json:"deletionTimestamp,omitempty"`
          OwnerReferences   []OwnerReference `json:"ownerReferences,omitempty"`
      } `json:"metadata"`
      Spec struct {
          NodeName string `json:"nodeName"`
      } `json:"spec"`
      Status struct {
          StartTime *time.Time `json:"startTime,omitempty"`
      } `json:"status"`
  }
  func (p Pod) ControllerOwner() *OwnerReference // nil when none has Controller == true

  type Client interface {
      ListPodsOnNode(ctx context.Context, node string) ([]Pod, error)
      DeletePod(ctx context.Context, namespace, name, uid string) error // preconditions.uid; NotFound → nil
      PodEvent(ctx context.Context, pod Pod, reason, message string) error // type Warning, source component "fileblock-node"
      PolicyEnforced(ctx context.Context, name string) (bool, error) // VAP <name> and binding <name> exist and the binding has Deny
  }
  func InCluster() (*HTTPClient, error) // reads KUBERNETES_SERVICE_HOST/PORT, the SA token and CA
  type HTTPClient struct { /* base URL, http.Client, tokenPath */ }
  func NewHTTPClient(baseURL string, hc *http.Client, tokenPath string) *HTTPClient
  ```
  `*HTTPClient` implements `Client`. The token is **re-read from `tokenPath` on every request**, because projected tokens rotate.

- [ ] **Step 1: Write the failing tests (httptest server)**

```go
package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewHTTPClient(srv.URL, srv.Client(), tok)
}

func TestListPodsOnNode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" || r.URL.Query().Get("fieldSelector") != "spec.nodeName=node-a" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"items":[{"metadata":{"name":"p","namespace":"ns","uid":"u1",
		  "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"rs","controller":true}]},
		  "spec":{"nodeName":"node-a"},"status":{"startTime":"2026-01-01T00:00:00Z"}}]}`)
	})
	pods, err := c.ListPodsOnNode(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Metadata.UID != "u1" || pods[0].ControllerOwner() == nil || pods[0].Status.StartTime == nil {
		t.Fatalf("decoded %+v", pods)
	}
}

func TestDeletePodSendsUIDPrecondition(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/namespaces/ns/pods/p" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		pre, _ := body["preconditions"].(map[string]any)
		if pre["uid"] != "u1" {
			t.Errorf("preconditions: %v", body)
		}
		if _, ok := body["gracePeriodSeconds"]; ok {
			t.Errorf("must not override the pod's grace period: %v", body)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{}`)
	})
	if err := c.DeletePod(context.Background(), "ns", "p", "u1"); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePodNotFoundIsNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeletePod(context.Background(), "ns", "p", "u1"); err != nil {
		t.Fatalf("NotFound should be nil, got %v", err)
	}
}

func TestDeletePodForbiddenIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"denied by policy"}`)
	})
	err := c.DeletePod(context.Background(), "ns", "p", "u1")
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("want the API message in the error, got %v", err)
	}
}

func TestPodEvent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/ns/events" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev["reason"] != "FileblockVolumeFailed" || ev["type"] != "Warning" {
			t.Errorf("event: %v", ev)
		}
		io_ := ev["involvedObject"].(map[string]any)
		if io_["uid"] != "u1" || io_["kind"] != "Pod" {
			t.Errorf("involvedObject: %v", io_)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)
	})
	var p Pod
	p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID = "p", "ns", "u1"
	if err := c.PodEvent(context.Background(), p, "FileblockVolumeFailed", "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyEnforced(t *testing.T) {
	for name, tc := range map[string]struct {
		policy, binding int
		actions         string
		want            bool
	}{
		"both with Deny":   {200, 200, `["Deny"]`, true},
		"audit only":       {200, 200, `["Audit"]`, false},
		"missing policy":   {404, 200, `["Deny"]`, false},
		"missing binding":  {200, 404, `["Deny"]`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/fileblock-node-pod-delete":
					w.WriteHeader(tc.policy)
					io.WriteString(w, `{}`)
				case "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/fileblock-node-pod-delete":
					w.WriteHeader(tc.binding)
					io.WriteString(w, `{"spec":{"validationActions":`+tc.actions+`}}`)
				default:
					t.Errorf("unexpected %s", r.URL)
				}
			})
			got, err := c.PolicyEnforced(context.Background(), "fileblock-node-pod-delete")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("PolicyEnforced=%v want %v", got, tc.want)
			}
		})
	}
}

func TestTokenReReadEachRequest(t *testing.T) {
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		io.WriteString(w, `{"items":[]}`)
	})
	_, _ = c.ListPodsOnNode(context.Background(), "n")
	_ = os.WriteFile(c.tokenPath, []byte("tok-2"), 0o600)
	_, _ = c.ListPodsOnNode(context.Background(), "n")
	if len(seen) != 2 || seen[1] != "Bearer tok-2" {
		t.Fatalf("token not re-read: %v", seen)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./pkg/kube/ -v`
Expected: FAIL. The package does not exist.

- [ ] **Step 3: Implement**

`types.go` holds the `Pod` and `OwnerReference` types from Interfaces, plus:

```go
func (p Pod) ControllerOwner() *OwnerReference {
	for i := range p.Metadata.OwnerReferences {
		if c := p.Metadata.OwnerReferences[i].Controller; c != nil && *c {
			return &p.Metadata.OwnerReferences[i]
		}
	}
	return nil
}
```

`client.go`:

```go
// Package kube is the few Kubernetes API calls the node plugin makes, over
// net/http with the pod's service-account token. It avoids client-go to
// keep the dependency tree small.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	saDir     = "/var/run/secrets/kubernetes.io/serviceaccount"
	userAgent = "fileblock-node"
)

type Client interface {
	ListPodsOnNode(ctx context.Context, node string) ([]Pod, error)
	DeletePod(ctx context.Context, namespace, name, uid string) error
	PodEvent(ctx context.Context, pod Pod, reason, message string) error
	PolicyEnforced(ctx context.Context, name string) (bool, error)
}

type HTTPClient struct {
	base      string
	hc        *http.Client
	tokenPath string
}

func NewHTTPClient(baseURL string, hc *http.Client, tokenPath string) *HTTPClient {
	return &HTTPClient{base: strings.TrimRight(baseURL, "/"), hc: hc, tokenPath: tokenPath}
}

// InCluster builds a client from the pod's service-account mount and the
// KUBERNETES_SERVICE_* env the kubelet injects.
func InCluster() (*HTTPClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s/ca.crt", saDir)
	}
	hc := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	return NewHTTPClient("https://"+net.JoinHostPort(host, port), hc, saDir+"/token"), nil
}

type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("kube API %d: %s", e.Code, e.Message) }

func (c *HTTPClient) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return fmt.Errorf("read service-account token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var st struct{ Message string `json:"message"` }
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = http.StatusText(resp.StatusCode)
		}
		return &apiError{Code: resp.StatusCode, Message: st.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isNotFound(err error) bool {
	e, ok := err.(*apiError)
	return ok && e.Code == http.StatusNotFound
}

func (c *HTTPClient) ListPodsOnNode(ctx context.Context, node string) ([]Pod, error) {
	var list struct{ Items []Pod `json:"items"` }
	q := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	if err := c.do(ctx, http.MethodGet, "/api/v1/pods?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *HTTPClient) DeletePod(ctx context.Context, namespace, name, uid string) error {
	body := map[string]any{
		"apiVersion":    "v1",
		"kind":          "DeleteOptions",
		"preconditions": map[string]string{"uid": uid},
	}
	err := c.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/"+url.PathEscape(name), body, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (c *HTTPClient) PodEvent(ctx context.Context, pod Pod, reason, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	ev := map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]string{"generateName": pod.Metadata.Name + ".", "namespace": pod.Metadata.Namespace},
		"involvedObject": map[string]string{
			"apiVersion": "v1", "kind": "Pod",
			"name": pod.Metadata.Name, "namespace": pod.Metadata.Namespace, "uid": pod.Metadata.UID,
		},
		"reason":         reason,
		"message":        message,
		"type":           "Warning",
		"source":         map[string]string{"component": "fileblock-node", "host": pod.Spec.NodeName},
		"firstTimestamp": now,
		"lastTimestamp":  now,
		"count":          1,
	}
	return c.do(ctx, http.MethodPost, "/api/v1/namespaces/"+url.PathEscape(pod.Metadata.Namespace)+"/events", ev, nil)
}

func (c *HTTPClient) PolicyEnforced(ctx context.Context, name string) (bool, error) {
	const base = "/apis/admissionregistration.k8s.io/v1/"
	if err := c.do(ctx, http.MethodGet, base+"validatingadmissionpolicies/"+url.PathEscape(name), nil, nil); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var b struct {
		Spec struct {
			ValidationActions []string `json:"validationActions"`
		} `json:"spec"`
	}
	if err := c.do(ctx, http.MethodGet, base+"validatingadmissionpolicybindings/"+url.PathEscape(name), nil, &b); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return slices.Contains(b.Spec.ValidationActions, "Deny"), nil
}
```

Use `errors.As` in `isNotFound` instead of the type assertion if `lint` flags it (errorlint).

- [ ] **Step 4: Run them and see them pass**

Run: `go test ./pkg/kube/ -v && go vet ./pkg/kube/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/kube
git commit -m "kube: minimal API client for listing, deleting and eventing pods

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: The monitor (`pkg/driver/monitor.go`)

**Files:**
- Create: `pkg/driver/monitor.go`
- Test: `pkg/driver/monitor_test.go`

**Interfaces:**
- Consumes:
  - `mount.ReadMountInfo`, `mount.LastAt`, `mount.PublishTargets`, `MountInfo.ForcedReadOnly` (Task 1);
  - `failures` (Task 2);
  - `kube.Client` and `kube.Pod` (Task 3);
  - `loop.State.All()`.
- Produces:
  ```go
  const (
      MonitorInterval = 10 * time.Second
      RemediationHold = 5 * time.Minute
      EventReasonVolumeFailed = "FileblockVolumeFailed"
      PodDeletePolicyName = "fileblock-node-pod-delete"
  )
  type Monitor struct {
      Node       string
      State      *loop.State
      Failures   *failures
      Kube       kube.Client   // nil: detect and log only
      AutoDelete bool          // flag value AND policy enforced; main.go decides
      MountInfo  func() ([]mount.MountInfo, error) // default: ReadMountInfo("/proc/self/mountinfo")
      Now        func() time.Time
      Log        *slog.Logger
  }
  func (n *NodeServer) Failures() *failures // so main.go can share the tracker
  func (m *Monitor) Run(ctx context.Context) // ticks every MonitorInterval until ctx is done
  func (m *Monitor) Tick(ctx context.Context) // one pass; exported for tests
  ```

**Behaviour of `Tick`:**
1. Read mountinfo. On error, log at warn and return.
2. For each mapping in `State.All()`:
   - find `LastAt(entries, m.StagePath)`. If it's absent, or not `ForcedReadOnly()`, skip it. Don't clear anything here: stage and unstage own clearing.
   - Call `Failures.Mark(volID, Now())`. If it's new, log at `warn` with the volumeID, loop device, crypt device and staging path, and send an event to every publishing pod (step 3's lookup).
   - If `!AutoDelete`, log once per volume at `info` that automatic deletion is off, then continue.
   - If `Now()` is before `Failures.HeldUntil(volID)`, continue.
   - Otherwise resolve the targets with `PublishTargets(entries, entry.MajorMinor)`, call `Kube.ListPodsOnNode(Node)`, and match on UID.
   - For each matched pod, skip it (with a log line, and an event saying why) if `ControllerOwner()==nil`, if `DeletionTimestamp!=nil`, or if `StartTime==nil || !StartTime.Before(failedAt)`. Otherwise call `DeletePod(ns, name, uid)`.
   - Only once **at least one delete was attempted without a transport or 5xx error**: call `Failures.Hold(volID, Now().Add(RemediationHold))`. If the list fails, return without holding (Review Focus 4).
3. Events are best-effort: log errors from them, but never let them stop a delete.

- [ ] **Step 1: Write the failing tests**

```go
package driver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/middlendian/fileblock-csi/pkg/kube"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

type fakeKube struct {
	pods    []kube.Pod
	listErr error
	deleted []string
	events  []string
}

func (f *fakeKube) ListPodsOnNode(context.Context, string) ([]kube.Pod, error) { return f.pods, f.listErr }
func (f *fakeKube) DeletePod(_ context.Context, ns, name, uid string) error {
	f.deleted = append(f.deleted, ns+"/"+name+"@"+uid)
	return nil
}
func (f *fakeKube) PodEvent(_ context.Context, p kube.Pod, reason, _ string) error {
	f.events = append(f.events, p.Metadata.Name+":"+reason)
	return nil
}
func (f *fakeKube) PolicyEnforced(context.Context, string) (bool, error) { return true, nil }

const (
	stage  = "/var/lib/kubelet/plugins/kubernetes.io/csi/fileblock.csi/h/globalmount"
	uidOld = "11111111-2222-3333-4444-555555555555"
)

func failedMountInfo() []mount.MountInfo {
	return []mount.MountInfo{
		{MajorMinor: "7:3", MountPoint: stage, MountOptions: []string{"rw"}, SuperOptions: []string{"ro"}},
		{MajorMinor: "7:3", MountPoint: "/var/lib/kubelet/pods/" + uidOld + "/volumes/kubernetes.io~csi/pvc-1/mount",
			MountOptions: []string{"rw"}, SuperOptions: []string{"ro"}},
	}
}

func pod(name, uid string, started time.Time, owned bool) kube.Pod {
	var p kube.Pod
	p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID = name, "ns", uid
	p.Spec.NodeName = "node-a"
	p.Status.StartTime = &started
	if owned {
		t := true
		p.Metadata.OwnerReferences = []kube.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", Controller: &t}}
	}
	return p
}

func newMonitor(t *testing.T, k *fakeKube, now *time.Time, mi []mount.MountInfo) *Monitor {
	t.Helper()
	st, err := loop.LoadState(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop3", StagePath: stage})
	return &Monitor{
		Node: "node-a", State: st, Failures: newFailures(), Kube: k, AutoDelete: true,
		MountInfo: func() ([]mount.MountInfo, error) { return mi, nil },
		Now:       func() time.Time { return *now },
		Log:       discardLog(),
	}
}

func TestTickDeletesOwnedPodStartedBeforeFailure(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(-time.Hour), true)}}
	m := newMonitor(t, k, &now, failedMountInfo())
	m.Tick(context.Background())
	if len(k.deleted) != 1 || k.deleted[0] != "ns/app@"+uidOld {
		t.Fatalf("deleted %v", k.deleted)
	}
	if len(k.events) == 0 {
		t.Fatal("want a FileblockVolumeFailed event")
	}
}

func TestTickSkipsUnowned(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("bare", uidOld, now.Add(-time.Hour), false)}}
	newMonitor(t, k, &now, failedMountInfo()).Tick(context.Background())
	if len(k.deleted) != 0 {
		t.Fatalf("unowned pod deleted: %v", k.deleted)
	}
}

func TestTickSkipsTerminating(t *testing.T) {
	now := time.Unix(10_000, 0)
	p := pod("app", uidOld, now.Add(-time.Hour), true)
	p.Metadata.DeletionTimestamp = &now
	k := &fakeKube{pods: []kube.Pod{p}}
	newMonitor(t, k, &now, failedMountInfo()).Tick(context.Background())
	if len(k.deleted) != 0 {
		t.Fatalf("terminating pod deleted: %v", k.deleted)
	}
}

func TestTickSkipsPodStartedAfterFailure(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(time.Second), true)}}
	newMonitor(t, k, &now, failedMountInfo()).Tick(context.Background())
	if len(k.deleted) != 0 {
		t.Fatalf("replacement pod deleted: %v", k.deleted)
	}
}

func TestTickSkipsPodWithoutStartTime(t *testing.T) {
	now := time.Unix(10_000, 0)
	p := pod("app", uidOld, now, true)
	p.Status.StartTime = nil
	k := &fakeKube{pods: []kube.Pod{p}}
	newMonitor(t, k, &now, failedMountInfo()).Tick(context.Background())
	if len(k.deleted) != 0 {
		t.Fatalf("pod without startTime deleted: %v", k.deleted)
	}
}

func TestTickHoldOffSurvivesRefail(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(-time.Hour), true)}}
	m := newMonitor(t, k, &now, failedMountInfo())
	m.Tick(context.Background())
	m.Failures.Clear("vol-1") // a re-stage happened
	now = now.Add(time.Minute)
	m.Tick(context.Background()) // fails again inside the hold-off
	if len(k.deleted) != 1 {
		t.Fatalf("second delete inside hold-off: %v", k.deleted)
	}
	now = now.Add(RemediationHold)
	m.Tick(context.Background())
	if len(k.deleted) != 2 {
		t.Fatalf("want a delete after the hold-off, got %v", k.deleted)
	}
}

func TestTickAutoDeleteOff(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(-time.Hour), true)}}
	m := newMonitor(t, k, &now, failedMountInfo())
	m.AutoDelete = false
	m.Tick(context.Background())
	if len(k.deleted) != 0 {
		t.Fatalf("deleted with AutoDelete off: %v", k.deleted)
	}
	if _, failed := m.Failures.Get("vol-1"); !failed {
		t.Fatal("detection must still mark the volume failed")
	}
}

func TestTickListErrorDoesNotConsumeHold(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{listErr: errors.New("connection refused")}
	m := newMonitor(t, k, &now, failedMountInfo())
	m.Tick(context.Background())
	if !m.Failures.HeldUntil("vol-1").IsZero() {
		t.Fatal("a failed list must not start the hold-off")
	}
	k.listErr = nil
	k.pods = []kube.Pod{pod("app", uidOld, now.Add(-time.Hour), true)}
	now = now.Add(MonitorInterval)
	m.Tick(context.Background())
	if len(k.deleted) != 1 {
		t.Fatalf("want a retry next tick, got %v", k.deleted)
	}
}

func TestTickHealthyVolumeUntouched(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(-time.Hour), true)}}
	mi := failedMountInfo()
	mi[0].SuperOptions = []string{"rw"}
	m := newMonitor(t, k, &now, mi)
	m.Tick(context.Background())
	if _, failed := m.Failures.Get("vol-1"); failed || len(k.deleted) != 0 {
		t.Fatal("a healthy volume must not be touched")
	}
}

// Review Focus 3: after a plugin restart the failure is rediscovered with a
// fresh failedAt, and a pod that started earlier still qualifies.
func TestTickRediscoversAfterRestart(t *testing.T) {
	now := time.Unix(10_000, 0)
	k := &fakeKube{pods: []kube.Pod{pod("app", uidOld, now.Add(-24*time.Hour), true)}}
	m := newMonitor(t, k, &now, failedMountInfo())
	m.Failures = newFailures() // a fresh process
	m.Tick(context.Background())
	if len(k.deleted) != 1 {
		t.Fatalf("want delete after restart, got %v", k.deleted)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./pkg/driver/ -run Tick -v`
Expected: FAIL with `undefined: Monitor`.

- [ ] **Step 3: Implement `monitor.go`**

```go
package driver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/middlendian/fileblock-csi/pkg/kube"
	"github.com/middlendian/fileblock-csi/pkg/loop"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

const (
	MonitorInterval         = 10 * time.Second
	RemediationHold         = 5 * time.Minute
	EventReasonVolumeFailed = "FileblockVolumeFailed"
	PodDeletePolicyName     = "fileblock-node-pod-delete"
)

// Monitor finds staged volumes whose ext4 forced itself read-only and, when
// AutoDelete is set, deletes the pods using them so they are re-staged. It
// only reads mountinfo: any I/O to the backing file during an ESTALE window
// would poison an inode that would otherwise survive.
type Monitor struct {
	Node       string
	State      *loop.State
	Failures   *failures
	Kube       kube.Client
	AutoDelete bool
	MountInfo  func() ([]mount.MountInfo, error)
	Now        func() time.Time
	Log        *slog.Logger

	offLogged map[string]bool
}

// Failures returns the tracker the monitor must share with this server.
func (n *NodeServer) Failures() *failures { return n.failures }

func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(MonitorInterval)
	defer t.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Monitor) Tick(ctx context.Context) {
	entries, err := m.MountInfo()
	if err != nil {
		m.Log.Warn("read mountinfo", "err", err)
		return
	}
	for _, vol := range m.State.All() {
		e, ok := mount.LastAt(entries, vol.StagePath)
		if !ok || !e.ForcedReadOnly() {
			continue
		}
		m.handleFailed(ctx, vol, e, entries)
	}
}

func (m *Monitor) handleFailed(ctx context.Context, vol loop.Mapping, e mount.MountInfo, entries []mount.MountInfo) {
	now := m.Now()
	isNew := m.Failures.Mark(vol.VolumeID, now)
	failedAt, _ := m.Failures.Get(vol.VolumeID)
	if isNew {
		m.Log.Warn("staged filesystem was forced read-only; the volume must be re-staged",
			"volumeID", vol.VolumeID, "loopDev", vol.LoopDev, "cryptDev", vol.CryptDev, "stagePath", vol.StagePath)
	}
	if m.Kube == nil {
		return
	}
	needPods := isNew || m.AutoDelete && !now.Before(m.Failures.HeldUntil(vol.VolumeID))
	if !needPods {
		return
	}
	targets := mount.PublishTargets(entries, e.MajorMinor)
	pods, err := m.Kube.ListPodsOnNode(ctx, m.Node)
	if err != nil {
		m.Log.Warn("list pods on node; will retry", "volumeID", vol.VolumeID, "err", err)
		return
	}
	byUID := map[string]kube.Pod{}
	for _, p := range pods {
		byUID[p.Metadata.UID] = p
	}
	var users []kube.Pod
	for _, t := range targets {
		if p, ok := byUID[t.PodUID]; ok {
			users = append(users, p)
		}
	}
	if isNew {
		msg := fmt.Sprintf("fileblock volume %s was forced read-only by ext4 (backing-store I/O error); it must be re-staged", vol.VolumeID)
		for _, p := range users {
			m.event(ctx, p, msg)
		}
	}
	if !m.AutoDelete {
		if m.offLogged == nil {
			m.offLogged = map[string]bool{}
		}
		if !m.offLogged[vol.VolumeID] {
			m.offLogged[vol.VolumeID] = true
			m.Log.Info("automatic pod deletion is off; delete the volume's pods to recover", "volumeID", vol.VolumeID)
		}
		return
	}
	if now.Before(m.Failures.HeldUntil(vol.VolumeID)) {
		return
	}
	attempted := false
	for _, p := range users {
		if reason := skipReason(p, failedAt); reason != "" {
			m.Log.Info("not deleting pod", "volumeID", vol.VolumeID, "pod", p.Metadata.Namespace+"/"+p.Metadata.Name, "reason", reason)
			continue
		}
		attempted = true
		if err := m.Kube.DeletePod(ctx, p.Metadata.Namespace, p.Metadata.Name, p.Metadata.UID); err != nil {
			m.Log.Error("delete pod", "volumeID", vol.VolumeID, "pod", p.Metadata.Namespace+"/"+p.Metadata.Name, "err", err)
			continue
		}
		m.Log.Warn("deleted pod so its volume is re-staged", "volumeID", vol.VolumeID, "pod", p.Metadata.Namespace+"/"+p.Metadata.Name)
	}
	if attempted {
		m.Failures.Hold(vol.VolumeID, now.Add(RemediationHold))
	}
}

// skipReason mirrors the admission policy; a denial there means a bug here.
func skipReason(p kube.Pod, failedAt time.Time) string {
	switch {
	case p.ControllerOwner() == nil:
		return "no controller would recreate it"
	case p.Metadata.DeletionTimestamp != nil:
		return "already terminating"
	case p.Status.StartTime == nil || !p.Status.StartTime.Before(failedAt):
		return "started after the volume failed"
	}
	return ""
}

func (m *Monitor) event(ctx context.Context, p kube.Pod, msg string) {
	if err := m.Kube.PodEvent(ctx, p, EventReasonVolumeFailed, msg); err != nil {
		m.Log.Warn("emit event", "pod", p.Metadata.Namespace+"/"+p.Metadata.Name, "err", err)
	}
}
```

A delete that the policy rejects with a 4xx still counts as attempted. That is deliberate: it stops a tight retry loop against a policy that denies. A 5xx or a transport error is no different in this code. If the reviewer insists on separating them, sort the `*apiError` code into the "retry next tick" path. That is optional, so rule on it in the ledger.

- [ ] **Step 4: Run them and see them pass**

Run: `go test ./pkg/driver/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/driver/monitor.go pkg/driver/monitor_test.go
git commit -m "driver: monitor forced read-only volumes and delete their pods

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Internal re-stage on publish, and the in-memory key cache

**Files:**
- Modify: `pkg/loop/state.go`. `Mapping` gains `StageContext map[string]string \`json:"stageContext,omitempty"\`` and `MountFlags []string \`json:"mountFlags,omitempty"\``.
- Modify: `pkg/driver/node.go`. Split `NodeStageVolume` into the existing handler plus `stageLocked`. Save `StageContext` and `MountFlags`. Add a key cache. Make NodePublishVolume handle a failed volume. Zero the cached key on unstage.
- Test: `pkg/loop/state_test.go`. A state file without the new fields still loads, and the new fields survive a round trip.
- Test: `pkg/driver/restage_test.go`.

**Interfaces:**
- Consumes: `failures` (Task 2), `mount.ReadMountInfo`, `mount.LastAt`, `mount.PublishTargets` (Task 1).
- Produces:
  - `func (n *NodeServer) stageLocked(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error)`: the body of today's `NodeStageVolume` from the idempotency check onward. The caller holds `lockVolume`.
  - `NodeServer.keys map[string]crypt.Keys` (guarded by `n.mu`), set in `stageLocked` for encrypted volumes and zeroed and deleted in `NodeUnstageVolume`;
  - `NodeServer.mountInfo func() ([]mount.MountInfo, error)`, defaulting to `/proc/self/mountinfo` and overridable in tests;
  - `NodeServer.kube kube.Client` (may be nil), with `func (n *NodeServer) SetKube(k kube.Client)`.

**Steps of the refactor:**
1. Move everything in `NodeStageVolume` **after** `unlock := n.lockVolume(volumeID); defer unlock()` into `stageLocked`. It needs `cfg`, `backing`, `encrypted`, `keys` and `format`, so re-derive them inside `stageLocked` from `req`. Keep the validation that runs before the lock in `NodeStageVolume`. `stageLocked` re-runs it, which is cheap and keeps it self-contained. So move **all** of the body below the argument checks into `stageLocked`, taking the lock in `NodeStageVolume` before calling it: `unlock := n.lockVolume(volumeID); defer unlock(); return n.stageLocked(ctx, req)`. The only behaviour change is that the lock is now taken before `registry.Get` instead of after. That is acceptable: `Get` has its own timeout-bounded staleness check. Record that in the ledger as a ruling.
2. In the `state.Put` inside `stageLocked`, add `StageContext: req.GetVolumeContext(), MountFlags: mountOptionsFromCap(req.GetVolumeCapability())`.
3. In `stageLocked`, once an encrypted stage has succeeded, cache the key: `n.mu.Lock(); n.keys[volumeID] = crypt.Keys{Current: bytes.Clone(keys.Current), Previous: bytes.Clone(keys.Previous)}; n.mu.Unlock()`.
4. In `NodeUnstageVolume`, after the detach loop, zero and drop the key:

```go
	n.mu.Lock()
	if k, ok := n.keys[volumeID]; ok {
		clear(k.Current)
		clear(k.Previous)
		delete(n.keys, volumeID)
	}
	n.mu.Unlock()
```

5. At the top of `NodePublishVolume`, after validation:

```go
	if _, failed := n.failures.Get(req.GetVolumeId()); failed {
		if err := n.restageFailed(ctx, req); err != nil {
			return nil, err
		}
	}
```

6. `restageFailed`:

```go
// restageFailed rebuilds a failed volume's staging mount from a fresh open of
// its image when nothing else still publishes it. The kubelet never unstages
// a volume a pod on this node still wants, so a same-node replacement would
// otherwise be handed the broken filesystem.
func (n *NodeServer) restageFailed(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	volumeID := req.GetVolumeId()
	unlock := n.lockVolume(volumeID)
	defer unlock()
	if _, failed := n.failures.Get(volumeID); !failed {
		return nil // another publish already re-staged it
	}
	m, ok := n.state.Get(volumeID)
	if !ok {
		return status.Errorf(codes.Unavailable, "volume %s failed and has no state entry; waiting for the kubelet to unstage it", volumeID)
	}
	entries, err := n.mountInfo()
	if err != nil {
		return status.Errorf(codes.Internal, "read mountinfo: %v", err)
	}
	if e, ok := mount.LastAt(entries, m.StagePath); ok {
		for _, t := range mount.PublishTargets(entries, e.MajorMinor) {
			if t.Path != req.GetTargetPath() {
				return status.Errorf(codes.Unavailable,
					"volume %s failed and is still published at %s; waiting for that pod to go", volumeID, t.Path)
			}
		}
	}
	var secrets map[string]string
	if m.StageContext[ParamEncrypted] == "true" {
		n.mu.Lock()
		k, ok := n.keys[volumeID]
		n.mu.Unlock()
		if !ok {
			n.publishEvent(ctx, req, "fileblock volume "+volumeID+
				" failed and its key is not in memory (the node plugin restarted); delete this pod to recover")
			return status.Errorf(codes.Unavailable, "volume %s failed and cannot be re-staged without its key; delete the pod", volumeID)
		}
		secrets = map[string]string{secretKey: string(k.Current)}
		if len(k.Previous) > 0 {
			secrets[secretPreviousKey] = string(k.Previous)
		}
	}
	// Tear down: a publish target left behind by this same request is
	// unmounted first so the staging unmount isn't busy.
	_ = n.mnt.Unmount(ctx, req.GetTargetPath())
	if err := n.mnt.Unmount(ctx, m.StagePath); err != nil {
		return status.Errorf(codes.Internal, "re-stage: umount stage: %v", err)
	}
	if m.CryptDev != "" {
		if err := n.luks.Close(ctx, filepath.Base(m.CryptDev)); err != nil {
			return status.Errorf(codes.Internal, "re-stage: cryptsetup close: %v", err)
		}
	}
	if err := n.losetup.Detach(ctx, m.LoopDev); err != nil {
		return status.Errorf(codes.Internal, "re-stage: losetup --detach: %v", err)
	}
	_ = n.state.Delete(volumeID)
	_, err = n.stageLocked(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: m.StagePath,
		VolumeContext:     m.StageContext,
		Secrets:           secrets,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: m.MountFlags}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	})
	if err != nil {
		return status.Errorf(status.Code(err), "re-stage: %v", status.Convert(err).Message())
	}
	n.log.Warn("re-staged failed volume from a fresh open of its image", "volumeID", volumeID)
	return nil
}
```

   `stageLocked` already calls `n.failures.Clear` (Task 2), so a successful re-stage clears the state.

   A mapping from before this release has no `StageContext`. If `m.StageContext` is nil, return `Unavailable` with the message "volume %s failed and was staged by an older version; delete the pod to recover", and emit the same event.

   `publishEvent` emits a Warning event on the pod named by `req.GetVolumeContext()["csi.storage.k8s.io/pod.name"]` only if it's present. The CSIDriver has `podInfoOnMount: false`, so usually it isn't, and then it just logs. **Do not** turn on `podInfoOnMount` in this task.

- [ ] **Step 1: Write the failing tests**

In `pkg/loop/state_test.go`:

```go
func TestStateLoadsWithoutStageContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"v":{"volumeId":"v","loopDev":"/dev/loop1","imagePath":"/i","stagePath":"/s"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := st.Get("v")
	if !ok || m.StageContext != nil || m.MountFlags != nil {
		t.Fatalf("old file: %+v", m)
	}
}

func TestStageContextRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	st, _ := LoadState(p)
	_ = st.Put(Mapping{VolumeID: "v", StageContext: map[string]string{"a": "b"}, MountFlags: []string{"errors=remount-ro"}})
	st2, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := st2.Get("v")
	if m.StageContext["a"] != "b" || len(m.MountFlags) != 1 {
		t.Fatalf("round trip: %+v", m)
	}
}
```

In `pkg/driver/restage_test.go`, drive `NodePublishVolume` with:
- a `NodeServer` from the same fake-runner setup the existing stage tests in `node_test.go` use (read `TestNodeStageVolume*` and reuse its helpers: fake `losetup`, `findmnt`, `mount`, `e2fsck` and `resize2fs` responses);
- `n.mountInfo` stubbed to return a fixed `[]mount.MountInfo`;
- `n.failures.Mark("vol-1", time.Now())`.

The cases:
1. **Another live target → `Unavailable`.** mountinfo lists a publish target at `/var/lib/kubelet/pods/<other-uid>/.../mount`. Assert `status.Code(err) == codes.Unavailable` and that no `losetup --detach` call was recorded.
2. **The only target is this request's own target → re-stage** (Review Focus 5). mountinfo's single publish target equals `req.TargetPath`. Assert the recorded calls include `umount <stage>`, `losetup --detach /dev/loopN`, a fresh `losetup --find --show`, and a `mount -t ext4`. Assert `n.failures.Get` reports not failed afterwards.
3. **Encrypted, key cached → re-stage passes the key to `cryptsetup`.** Assert the `CmdFunc` saw the key in `Secrets`.
4. **Encrypted, no cached key → `Unavailable`.** No detach call.
5. **Mapping without `StageContext` → `Unavailable`.**
6. **Not failed → plain bind.** Today's behaviour, with no mountinfo read: make `n.mountInfo` call `t.Fatal`.

Also add an unstage test: after a stage that cached a key, `NodeUnstageVolume` removes it from `n.keys`, and the original byte slice is all zeros. Keep a reference to the slice before unstaging to check.

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./pkg/loop/ ./pkg/driver/ -run 'StageContext|Restage|Unstage.*Key' -v`
Expected: FAIL. Unknown fields, `undefined: restageFailed`.

- [ ] **Step 3: Implement the refactor and the functions above**

- [ ] **Step 4: Run the whole driver and loop suites**

Run: `go test ./pkg/... -v 2>&1 | tail -40`
Expected: PASS, including every existing stage, unstage and adopt test. Those are the regression net for the `stageLocked` split.

- [ ] **Step 5: Commit**

```bash
git add pkg/loop/state.go pkg/loop/state_test.go pkg/driver/node.go pkg/driver/restage_test.go
git commit -m "driver: re-stage a failed volume on publish when nothing else uses it

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: RBAC, the admission policy, and its CEL tests

**Files:**
- Modify: `deploy/kustomize/base/rbac.yaml`. Add rules to the `fileblock-node` ClusterRole.
- Create: `deploy/kustomize/base/remediation-policy.yaml`
- Modify: `deploy/kustomize/base/kustomization.yaml`. Add `remediation-policy.yaml` to `resources`.
- Test: `deploy/policy_test.go`. Uses `github.com/google/cel-go` (test-only dependency; run `go get github.com/google/cel-go@latest && go mod tidy`).

**RBAC rules to append to the `fileblock-node` ClusterRole:**

```yaml
  # Failed-volume remediation (--auto-delete-pod-when-volume-failed). The
  # delete is cluster-wide here; remediation-policy.yaml limits it to pods
  # on the caller's own node that a controller owns and that mount a PVC.
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list, delete]
  - apiGroups: [""]
    resources: [events]
    verbs: [create, patch]
  - apiGroups: [admissionregistration.k8s.io]
    resources: [validatingadmissionpolicies, validatingadmissionpolicybindings]
    resourceNames: [fileblock-node-pod-delete]
    verbs: [get]
```

Update the comment above the ClusterRole: it currently says the registrar "only needs to GET its own pod and list nodes".

**`remediation-policy.yaml`:**

```yaml
# Limits fileblock-node's pod deletes (granted cluster-wide in rbac.yaml) to
# pods on the caller's own node that a controller owns and that mount a
# PVC. fileblock-node checks at startup that this policy and its binding
# exist, with Deny, before it deletes anything. Requires Kubernetes 1.30+:
# GA ValidatingAdmissionPolicy, and pod-bound service-account tokens that
# carry the node name in userInfo.extra.
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: fileblock-node-pod-delete
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [DELETE]
        resources: [pods]
  # The namespace is a wildcard because kustomize's namespace: rewrites the
  # ServiceAccount but not this string. Matching more callers only
  # restricts more callers.
  matchConditions:
    - name: is-fileblock-node
      expression: >-
        request.userInfo.username.startsWith('system:serviceaccount:') &&
        request.userInfo.username.endsWith(':fileblock-node')
  validations:
    - expression: >-
        'authentication.kubernetes.io/node-name' in request.userInfo.extra &&
        oldObject.spec.?nodeName.orValue('') ==
        request.userInfo.extra['authentication.kubernetes.io/node-name'][0]
      message: fileblock-node may only delete pods on its own node
    - expression: >-
        oldObject.metadata.?ownerReferences.orValue([]).exists(r,
        r.?controller.orValue(false))
      message: fileblock-node may only delete pods a controller will recreate
    - expression: >-
        oldObject.spec.?volumes.orValue([]).exists(v,
        has(v.persistentVolumeClaim))
      message: fileblock-node may only delete pods that mount a PVC
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: fileblock-node-pod-delete
spec:
  policyName: fileblock-node-pod-delete
  validationActions: [Deny]
```

**Test approach.** Parse the YAML (find the validations by splitting on `expression: >-`, or use `sigs.k8s.io/yaml` only if it is already an indirect dependency; otherwise use a minimal line parser), then compile each expression with cel-go. Set it up with `cel.OptionalTypes()`, `ext.Strings()`, and the variables `request` and `oldObject` as `cel.DynType`. Evaluate every validation **and** the matchCondition against these cases:

| Case | username | extra node-name | pod nodeName | owner controller | volumes | Expect |
|---|---|---|---|---|---|---|
| allowed | `system:serviceaccount:fileblock-system:fileblock-node` | `node-a` | `node-a` | true | pvc | all validations true |
| other node | same | `node-a` | `node-b` | true | pvc | node validation false |
| unowned | same | `node-a` | `node-a` | none | pvc | owner validation false |
| owner not controller | same | `node-a` | `node-a` | `controller: false` | pvc | owner validation false |
| no PVC | same | `node-a` | `node-a` | true | emptyDir only | PVC validation false |
| no node-name extra | same | *(absent)* | `node-a` | true | pvc | node validation false (no error) |
| renamed namespace | `system:serviceaccount:storage:fileblock-node` | — | — | — | — | matchCondition true |
| other caller | `system:serviceaccount:kube-system:replicaset-controller` | — | — | — | — | matchCondition false |

Also assert `failurePolicy: Fail` and `validationActions: [Deny]` with `strings.Contains` checks, in the style of `deploy/manifests_test.go`.

- [ ] **Step 1: Write `deploy/policy_test.go` with the table above, then run it and see it fail**

Run: `go test ./deploy/ -run Policy -v`
Expected: FAIL. The policy file is missing.

- [ ] **Step 2: Add the manifests and RBAC**

- [ ] **Step 3: Run the tests and render the base**

Run: `go test ./deploy/ -v && kubectl kustomize deploy/kustomize/base | grep -c 'kind: ValidatingAdmissionPolicy'`
Expected: PASS, and `2` (one policy, one binding; `grep -c` counts the binding's line too).

- [ ] **Step 4: Commit**

```bash
git add deploy/ go.mod go.sum
git commit -m "deploy: node RBAC and admission policy for failed-volume remediation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Wire it up in `cmd/node/main.go`

**Files:**
- Modify: `cmd/node/main.go`

**Interfaces:**
- Consumes: `kube.InCluster` (Task 3); `driver.Monitor`, `driver.PodDeletePolicyName`, `NodeServer.Failures()` and `NodeServer.SetKube` (Tasks 4 and 5); `mount.ReadMountInfo` (Task 1).

- [ ] **Step 1: Add the flag and the wiring**

Add the flag next to the others:

```go
	autoDelete := flag.Bool("auto-delete-pod-when-volume-failed", true,
		"delete the controller-owned pods on this node that use a volume whose ext4 was forced read-only, so it is re-staged; needs the fileblock-node-pod-delete admission policy")
```

After `node := driver.NewNodeServer(...)`:

```go
	kc, err := kube.InCluster()
	if err != nil {
		log.Warn("no Kubernetes API access; failed volumes are only logged", "err", err)
	}
	deleting := false
	if kc != nil {
		node.SetKube(kc)
		if *autoDelete {
			ok, perr := kc.PolicyEnforced(context.Background(), driver.PodDeletePolicyName)
			switch {
			case perr != nil:
				log.Error("cannot verify the pod-delete admission policy; failed volumes are only reported", "err", perr)
			case !ok:
				log.Error("admission policy " + driver.PodDeletePolicyName + " or its binding is missing, or does not Deny; " +
					"failed volumes are only reported until it is applied and the plugin restarts")
			default:
				deleting = true
			}
		}
	}
	mon := &driver.Monitor{
		Node:       *nodeID,
		State:      state,
		Failures:   node.Failures(),
		AutoDelete: deleting,
		MountInfo:  func() ([]mount.MountInfo, error) { return mount.ReadMountInfo("/proc/self/mountinfo") },
		Now:        time.Now,
		Log:        log,
	}
	if kc != nil {
		mon.Kube = kc
	}
```

After `ctx, stop := signal.NotifyContext(...)` and before `srv.Serve(ctx)`, add `go mon.Run(ctx)`.

Assign `mon.Kube = kc` only when `kc != nil`. A typed-nil `*HTTPClient` stored in the interface would not compare equal to nil.

Policy presence is checked **once at startup**. The log message tells operators to restart after applying the policy. Re-checking on a timer is not in the spec, so don't add it.

- [ ] **Step 2: Build and vet**

Run: `make build vet lint`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add cmd/node/main.go
git commit -m "node: run the failed-volume monitor; --auto-delete-pod-when-volume-failed

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: e2e coverage and the manual ESTALE script

**Files:**
- Create: `test/e2e/remediation_test.go`
- Create: `hack/estale-repro.sh`
- Check: `hack/e2e-kind.yaml`. The kind node image must be Kubernetes ≥ 1.30. If `hack/e2e.sh` pins an older `kindest/node`, bump it in this task and say so in the commit.

**Test 1, `TestFailedVolumeIsRemediated`:**
1. Make a namespace. Apply a StorageClass `fileblock-remount-ro` that clones the e2e `fileblock` class's parameters (follow `encryptedSCYAML` in `encryption_test.go`) and adds `mountOptions: [errors=remount-ro]`. Apply a PVC on it.
2. Apply a **Deployment** with 1 replica (not a bare pod, since it must be controller-owned) running `sleep 3600` with the PVC at `/data`. Wait for the pod to be Ready and record its name, node and UID.
3. Find the device. Run `kubectl exec <pod> -- sh -c "grep ' /data ' /proc/self/mountinfo | awk '{print \$3}'"`, which gives `MAJ:MIN`. Then on the node: `docker exec <node> sh -c 'basename $(readlink /sys/dev/block/MAJ:MIN)'`, which gives `loopN` (or `dm-N`).
4. Force the error: `docker exec <node> sh -c 'echo 1 > /sys/fs/ext4/<dev>/trigger_fs_error'`. With `errors=remount-ro`, ext4 makes the superblock read-only.
5. Within 60s, expect all of:
   - an event with reason `FileblockVolumeFailed` on the old pod (`kubectl get events -n ns --field-selector reason=FileblockVolumeFailed`);
   - the old pod UID gone;
   - a new pod Ready;
   - `kubectl exec <new> -- sh -c 'touch /data/ok'` succeeds.

**Test 2, `TestFailedVolumeRestagesOnSameNode`:** the same, but the Deployment has `nodeName: <node>` pinned, so the replacement must land on the same node. After recovery, the fileblock-node log on that node must contain `re-staged failed volume`, or alternatively show `NodeUnstageVolume` followed by `NodeStageVolume`. Either proves the replacement didn't get the broken mount; assert the write succeeds.

**Test 3, `TestPodDeletePolicyDeniesOtherNode`:**
1. Create a pod pinned to node B with a PVC.
2. Mint a token: `kubectl create token fileblock-node -n fileblock-system --bound-object-kind Pod --bound-object-name <fileblock-node pod on node A>`. A pod-bound token carries the node name.
3. Run `kubectl --token <tok> delete pod <B-pod> -n ns`, expecting a non-zero exit and `may only delete pods on its own node` in stderr.
4. The same token deleting a controller-owned PVC pod on node A succeeds.

**`hack/estale-repro.sh`:**

```sh
#!/usr/bin/env bash
# Manual reproduction of an NFS server answering ESTALE for a short window,
# the condition failed-volume remediation exists for. Not run in CI: it
# depends on timing. Run against an `make e2e-nfs` cluster with a pod
# writing continuously to a fileblock volume.
#
#   hack/estale-repro.sh /path/to/export 30
#
# Unexports the directory for N seconds (default 30) and exports it again.
# In-flight I/O gets ESTALE, ext4 aborts, and the monitor should delete the
# writer pod within one check interval of the export returning.
set -euo pipefail
EXPORT="${1:?usage: $0 EXPORT_DIR [SECONDS]}"
SECS="${2:-30}"
SUDO="${SUDO:-sudo}"
$SUDO exportfs -u "*:${EXPORT}"
echo "unexported ${EXPORT}; sleeping ${SECS}s"
sleep "${SECS}"
$SUDO exportfs -o rw,sync,no_root_squash,no_subtree_check "*:${EXPORT}"
echo "re-exported ${EXPORT}"
```

Check `hack/e2e.sh`'s `prepare_backing_nfs` for the export options it actually uses, and match them.

- [ ] **Step 1: Write the three tests and the script**
- [ ] **Step 2: Compile the e2e package**

Run: `go vet -tags=e2e ./test/e2e/`
Expected: success. The suite itself runs in CI (`e2e.yml`) on push to main and via workflow_dispatch. If a kind and Docker environment is available locally, `make e2e` also runs it; otherwise say in the report that it was compile-checked only.
- [ ] **Step 3: Commit**

```bash
git add test/e2e/remediation_test.go hack/estale-repro.sh hack/e2e-kind.yaml hack/e2e.sh
git commit -m "e2e: failed-volume remediation, same-node re-stage, policy denial

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `CHANGELOG.md`

**Content:**
- **README.** Add a new section, *Recovering from backing-store errors*. It should cover:
  1. Why a short ESTALE burst leaves a volume read-only for good. Summarise the spec's Problem in about six sentences.
  2. What the monitor does, and that it's passive.
  3. The `--auto-delete-pod-when-volume-failed` flag (default `true`), the three conditions a pod must meet, and the 5-minute hold-off.
  4. The admission policy, what it allows, and the Kubernetes 1.30 requirement. Without the policy, the plugin only reports.
  5. The same-node re-stage, and the one manual case: an encrypted volume after a plugin restart.
  6. How to spot it: the `FileblockVolumeFailed` events, the `VolumeCondition` (and the kubelet `CSIVolumeHealth` gate), and the log lines.
  7. The not-covered case: volumes that only read.

  Also update the *Limitations* section and the flag table, if there is one.
- **CLAUDE.md:**
  - *CSI surface*: add `VOLUME_CONDITION`.
  - *State file invariants*: add `StageContext` and `MountFlags` as optional fields, and that encryption keys are never persisted (process memory only, zeroed on unstage).
  - *Repo map*: add `pkg/kube/` with "minimal API client (no client-go)".
- **CHANGELOG `## [Unreleased]`:**

```markdown
### Added

- **Failed volumes recover on their own.** A burst of `ESTALE` from an NFS
  server (common while it restarts) marks a staged image's NFS inode
  permanently stale; the loop device keeps failing, ext4 aborts its journal
  and goes read-only, and until now only deleting the pod recovered it.
  `fileblock-node` now notices a staging mount whose superblock was forced
  read-only, reports it (a `FileblockVolumeFailed` event on each pod using
  it, `VolumeCondition` from `NodeGetVolumeStats`), and by default deletes
  those pods on its own node so their controller recreates them on a
  freshly opened image. Only controller-owned pods that started before the
  failure are deleted, at most once per volume every 5 minutes.
  **Behaviour change:** after upgrading and applying the base manifests,
  this happens automatically. Set `--auto-delete-pod-when-volume-failed=false`
  on the node DaemonSet to keep reporting without deleting.
- `deploy/kustomize/base/remediation-policy.yaml`: a ValidatingAdmissionPolicy
  limiting the node plugin's pod deletes to its own node, controller-owned
  pods, and pods that mount a PVC. Needs Kubernetes 1.30+; without it the
  plugin only reports.
- Node capability `VOLUME_CONDITION`.
```

- [ ] **Step 1: Write the docs**
- [ ] **Step 2: Check for leaks and for a clean build**

Run: `grep -rniE 'homelab|hasknet|haskins' README.md CLAUDE.md CHANGELOG.md docs/ || echo clean; make fmt-check vet lint tidy-check test build`
Expected: `clean`, and every gate passes.
- [ ] **Step 3: Commit**

```bash
git add README.md CLAUDE.md CHANGELOG.md
git commit -m "docs: failed-volume remediation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
