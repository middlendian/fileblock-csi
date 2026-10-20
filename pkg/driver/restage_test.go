package driver

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/middlendian/fileblock-csi/pkg/crypt"
	"github.com/middlendian/fileblock-csi/pkg/mount"
)

const (
	uidThis  = "aaaaaaaa-0000-0000-0000-000000000001"
	uidOther = "bbbbbbbb-0000-0000-0000-000000000002"
)

// podTarget is a kubelet-shaped publish target under a temp root, so
// NodePublishVolume can create it and mount.PublishTargets recognizes it.
func podTarget(t *testing.T, uid string) string {
	return filepath.Join(t.TempDir(), "pods", uid, "volumes", "kubernetes.io~csi", "pvc-1", "mount")
}

func publishReq(e *encStage, target string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: e.stage,
		TargetPath:        target,
		VolumeContext:     e.vc,
		VolumeCapability:  stageReq("vol-1", e.vc).VolumeCapability,
	}
}

// failedStaged stages vol-1 for real through the fake runner, marks it
// failed, and makes mountinfo show its stage plus the given publish
// targets on the stage's device. The fake's call log is reset so a test
// sees only the publish's calls.
func failedStaged(t *testing.T, e *encStage, secrets map[string]string, targets ...string) {
	t.Helper()
	req := e.req(secrets)
	req.VolumeCapability.GetMount().MountFlags = []string{"errors=remount-ro"}
	if _, err := e.n.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	mi := []mount.MountInfo{{MajorMinor: "7:7", MountPoint: e.stage, MountOptions: []string{"rw"}, SuperOptions: []string{"ro"}}}
	for _, tp := range targets {
		mi = append(mi, mount.MountInfo{MajorMinor: "7:7", MountPoint: tp, MountOptions: []string{"rw"}, SuperOptions: []string{"ro"}})
	}
	e.n.mountInfo = func() ([]mount.MountInfo, error) { return mi, nil }
	e.n.failures.Mark("vol-1", time.Now())
	e.fake.Reset()
}

func TestRestageRefusedWhileAnotherTargetIsLive(t *testing.T) {
	e := newEncStage(t, false, nil)
	failedStaged(t, e, nil, podTarget(t, uidOther))
	_, err := e.n.NodePublishVolume(context.Background(), publishReq(e, podTarget(t, uidThis)))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if got := detached(e.fake.Calls); len(got) != 0 {
		t.Fatalf("detached %v while another pod still publishes the volume", got)
	}
	if _, failed := e.n.failures.Get("vol-1"); !failed {
		t.Fatal("failure cleared without a re-stage")
	}
}

// Review Focus 5: this request's own target, left behind by an earlier
// attempt, does not block the re-stage.
func TestRestageWhenOnlyTargetIsThisRequest(t *testing.T) {
	e := newEncStage(t, false, nil)
	target := podTarget(t, uidThis)
	failedStaged(t, e, nil, target)
	if _, err := e.n.NodePublishVolume(context.Background(), publishReq(e, target)); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	calls := e.fake.Calls
	um := callIndex(calls, "umount", e.stage)
	det := callIndex(calls, "losetup", "--detach", "/dev/loop7")
	att := callIndex(calls, "losetup", "--find", "--show")
	mnt := callIndex(calls, "mount", "-t", "ext4", "-o", "errors=remount-ro", "/dev/loop7", e.stage)
	if um < 0 || det < 0 || att < 0 || mnt < 0 || um > det || det > att || att > mnt {
		t.Fatalf("want umount stage < detach < attach < mount with saved flags: %d %d %d %d %+v", um, det, att, mnt, calls)
	}
	if _, failed := e.n.failures.Get("vol-1"); failed {
		t.Fatal("still failed after a successful re-stage")
	}
	m, ok := e.n.state.Get("vol-1")
	if !ok || m.StageContext[ParamEncrypted] != "" || !slices.Equal(m.MountFlags, []string{"errors=remount-ro"}) {
		t.Fatalf("state after re-stage = %+v", m)
	}
}

func TestRestageEncryptedUsesCachedKey(t *testing.T) {
	e := newEncStage(t, true, nil)
	target := podTarget(t, uidThis)
	prev := "prev-" + testKey
	failedStaged(t, e, map[string]string{"key": testKey, "previousKey": prev}, target)
	if _, err := e.n.NodePublishVolume(context.Background(), publishReq(e, target)); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	calls := e.fake.Calls
	cl := callIndex(calls, "cryptsetup", "close", crypt.MapperName("vol-1"))
	det := callIndex(calls, "losetup", "--detach", "/dev/loop7")
	open := callIndex(calls, "cryptsetup", "open", "--type")
	if cl < 0 || det < 0 || open < 0 || cl > det || det > open {
		t.Fatalf("want close < detach < open: %d %d %d %+v", cl, det, open, calls)
	}
	if got := string(calls[open].Secrets[0]); got != testKey {
		t.Fatalf("cryptsetup open got key %q, want the cached one", got)
	}
	if _, failed := e.n.failures.Get("vol-1"); failed {
		t.Fatal("still failed after a successful re-stage")
	}
}

func TestRestageEncryptedWithoutCachedKeyIsUnavailable(t *testing.T) {
	e := newEncStage(t, true, nil)
	k := &fakeKube{}
	e.n.SetKube(k)
	target := podTarget(t, uidThis)
	failedStaged(t, e, map[string]string{"key": testKey}, target)
	// A plugin restart loses the in-memory key.
	e.n.mu.Lock()
	delete(e.n.keys, "vol-1")
	e.n.mu.Unlock()
	req := publishReq(e, target)
	req.VolumeContext = map[string]string{
		"csi.storage.k8s.io/pod.name":      "app-0",
		"csi.storage.k8s.io/pod.namespace": "ns",
		"csi.storage.k8s.io/pod.uid":       uidThis,
	}
	_, err := e.n.NodePublishVolume(context.Background(), req)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if got := detached(e.fake.Calls); len(got) != 0 {
		t.Fatalf("detached %v without a key to re-stage", got)
	}
	if !slices.Equal(k.events, []string{"app-0:" + EventReasonVolumeFailed}) {
		t.Fatalf("events = %v", k.events)
	}
}

// Without pod info in the volume context (podInfoOnMount: false) there is
// no pod to event, so the message is logged instead.
func TestRestageEventWithoutPodInfoLogs(t *testing.T) {
	e := newEncStage(t, true, nil)
	k := &fakeKube{}
	e.n.SetKube(k)
	target := podTarget(t, uidThis)
	failedStaged(t, e, map[string]string{"key": testKey}, target)
	e.n.mu.Lock()
	delete(e.n.keys, "vol-1")
	e.n.mu.Unlock()
	_, err := e.n.NodePublishVolume(context.Background(), publishReq(e, target))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if len(k.events) != 0 {
		t.Fatalf("events = %v, want none without pod info", k.events)
	}
	if !strings.Contains(e.log.String(), "delete this pod to recover") {
		t.Fatalf("message not logged: %s", e.log.String())
	}
}

func TestRestageOlderMappingWithoutStageContextIsUnavailable(t *testing.T) {
	e := newEncStage(t, false, nil)
	target := podTarget(t, uidThis)
	failedStaged(t, e, nil, target)
	m, _ := e.n.state.Get("vol-1")
	m.StageContext, m.MountFlags = nil, nil
	if err := e.n.state.Put(m); err != nil {
		t.Fatal(err)
	}
	_, err := e.n.NodePublishVolume(context.Background(), publishReq(e, target))
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "older version") {
		t.Fatalf("got %v, want Unavailable naming an older version", err)
	}
	if got := detached(e.fake.Calls); len(got) != 0 {
		t.Fatalf("detached %v for a mapping it cannot re-stage", got)
	}
}

func TestPublishNotFailedIsPlainBind(t *testing.T) {
	e := newEncStage(t, false, nil)
	e.n.mountInfo = func() ([]mount.MountInfo, error) {
		t.Fatal("mountinfo read for a healthy volume")
		return nil, nil
	}
	target := podTarget(t, uidThis)
	e.depth[target] = 0 // not yet mounted
	if _, err := e.n.NodePublishVolume(context.Background(), publishReq(e, target)); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if callIndex(e.fake.Calls, "mount", "--bind", e.stage, target) < 0 {
		t.Fatalf("no bind mount: %+v", e.fake.Calls)
	}
	if callIndex(e.fake.Calls, "losetup") >= 0 || callIndex(e.fake.Calls, "umount") >= 0 {
		t.Fatalf("healthy publish touched the stage: %+v", e.fake.Calls)
	}
}

func TestUnstageZeroesAndDropsCachedKey(t *testing.T) {
	e := newEncStage(t, true, nil)
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey, "previousKey": "prev-" + testKey})); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	e.n.mu.Lock()
	k, ok := e.n.keys["vol-1"]
	e.n.mu.Unlock()
	if !ok || string(k.Current) != testKey || string(k.Previous) != "prev-"+testKey {
		t.Fatalf("key not cached after stage: %v", ok)
	}
	if _, err := e.n.NodeUnstageVolume(context.Background(), unstageReq(e)); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	e.n.mu.Lock()
	_, still := e.n.keys["vol-1"]
	e.n.mu.Unlock()
	if still {
		t.Fatal("key still cached after unstage")
	}
	for _, b := range append(k.Current, k.Previous...) {
		if b != 0 {
			t.Fatal("cached key bytes not zeroed on unstage")
		}
	}
}

func TestStageSavesContextAndFlags(t *testing.T) {
	e := newEncStage(t, false, nil)
	req := e.req(nil)
	req.VolumeCapability.GetMount().MountFlags = []string{"noatime"}
	if _, err := e.n.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	m, _ := e.n.state.Get("vol-1")
	if !maps.Equal(m.StageContext, e.vc) || !slices.Equal(m.MountFlags, []string{"noatime"}) {
		t.Fatalf("state = %+v", m)
	}
	e.n.mu.Lock()
	_, cached := e.n.keys["vol-1"]
	e.n.mu.Unlock()
	if cached {
		t.Fatal("plaintext stage cached a key")
	}
}
