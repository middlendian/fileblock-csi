package driver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

	// Per-failure memory, forgotten once the volume is no longer failed.
	offLogged   map[string]bool // volumeID: "auto-delete is off" logged
	eventSent   map[string]bool // volumeID: first-failure events delivered
	skipEvented map[string]bool // volumeID/podUID: skip event delivered
}

func (m *Monitor) forget(volumeID string) {
	delete(m.offLogged, volumeID)
	delete(m.eventSent, volumeID)
	for k := range m.skipEvented {
		if strings.HasPrefix(k, volumeID+"/") {
			delete(m.skipEvented, k)
		}
	}
}

// Failures returns the tracker the monitor must share with this server.
func (n *NodeServer) Failures() *failures { return n.failures }

// Run ticks every MonitorInterval until ctx is done.
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

// Tick makes one detection and remediation pass.
func (m *Monitor) Tick(ctx context.Context) {
	if m.MountInfo == nil {
		m.MountInfo = func() ([]mount.MountInfo, error) { return mount.ReadMountInfo("/proc/self/mountinfo") }
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.Log == nil {
		m.Log = slog.Default()
	}
	entries, err := m.MountInfo()
	if err != nil {
		m.Log.Warn("read mountinfo", "err", err)
		return
	}
	for _, vol := range m.State.All() {
		e, ok := mount.LastAt(entries, vol.StagePath)
		if !ok || !e.ForcedReadOnly() {
			// A healthy stage clears any mark a stale snapshot left after
			// a re-stage; an absent one may be mid-re-stage, so it waits.
			if ok {
				m.Failures.Clear(vol.VolumeID)
			}
			m.forget(vol.VolumeID)
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
		m.forget(vol.VolumeID)
		m.Log.Warn("staged filesystem was forced read-only; the volume must be re-staged",
			"volumeID", vol.VolumeID, "loopDev", vol.LoopDev, "cryptDev", vol.CryptDev, "stagePath", vol.StagePath)
	}
	if m.Kube == nil {
		return
	}
	if m.eventSent == nil {
		m.eventSent, m.skipEvented = map[string]bool{}, map[string]bool{}
	}
	needPods := !m.eventSent[vol.VolumeID] || m.AutoDelete && !now.Before(m.Failures.HeldUntil(vol.VolumeID))
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
	if !m.eventSent[vol.VolumeID] {
		m.eventSent[vol.VolumeID] = true
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
			if key := vol.VolumeID + "/" + p.Metadata.UID; !m.skipEvented[key] {
				m.skipEvented[key] = true
				m.event(ctx, p, fmt.Sprintf("fileblock volume %s failed but this pod was not deleted automatically: %s", vol.VolumeID, reason))
			}
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
