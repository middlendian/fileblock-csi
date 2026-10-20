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

func (f *fakeKube) ListPodsOnNode(context.Context, string) ([]kube.Pod, error) {
	return f.pods, f.listErr
}
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
