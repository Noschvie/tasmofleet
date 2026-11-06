package templates

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
)

// clockedManager is a Manager whose clock the test moves.
func clockedManager(f *fakeFleet, now *time.Time) *Manager {
	return New(Options{Devices: f, Client: f, Backups: f, Settings: f, Now: func() time.Time { return *now }})
}

func deviceOf(t *testing.T, ov Overview, id model.DeviceID) DeviceView {
	t.Helper()
	for _, g := range ov.Groups {
		for _, d := range g.Devices {
			if d.ID == id {
				return d
			}
		}
	}
	for _, d := range ov.NotJudged {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("device %d is on no card", id)
	return DeviceView{}
}

// Why (#599): a reading older than the hour used to vanish from the tab (replaced by an older backup
// or by "not read yet"). The page must show the last known template with its age and a stale marker,
// but never offer it for applying: only fresh reads are applied.
func TestAnOldLiveReadingIsStaleNotGone(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	now := t0
	m := clockedManager(f, &now)
	_, _ = m.Overview(context.Background(), nil, true)

	now = t0.Add(StaleAfter)
	ov, _ := m.Overview(context.Background(), nil, false)
	if d := deviceOf(t, ov, a); d.Source != SourceLive || !d.Reading.Live {
		t.Fatalf("exactly one hour old is still live: %+v", d)
	}

	now = t0.Add(StaleAfter + time.Second)
	ov, _ = m.Overview(context.Background(), nil, false)
	d := deviceOf(t, ov, a)
	if d.Source != SourceStale || !d.ReadAt.Equal(t0) || d.Reading.Template == nil {
		t.Fatalf("an old reading must stay, as stale, with its time: %+v", d)
	}
	if d.Reading.Live || d.Reading.State() != policy.OnTemplate {
		t.Errorf("a stale reading must be shown but never applied (live=%v): %+v", d.Reading.Live, d.Reading)
	}
}

// Why (#599): a backup that is newer than the stale reading is the better last known value.
func TestAStaleReadingYieldsToANewerBackup(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	f.snap[a] = []byte(`{"Module":` + f.module[a] + `,"Template":` + f.template[a] + `}`) // taken t0-48h
	now := t0.Add(-72 * time.Hour)
	m := clockedManager(f, &now)
	_, _ = m.Overview(context.Background(), nil, true)
	now = t0
	ov, _ := m.Overview(context.Background(), nil, false)
	if d := deviceOf(t, ov, a); d.Source != SourceBackup {
		t.Fatalf("source %q, want the newer backup", d.Source)
	}
}

// Why (#599): a refresh that fails must not take the last values away; it says which devices could not
// be read, so the page can tell the person the numbers shown are old, and a later success clears it.
func TestAFailedRefreshKeepsTheLastReadingAndIsReported(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	now := t0
	m := clockedManager(f, &now)
	_, _ = m.Overview(context.Background(), nil, true)

	f.failRead[a] = true
	now = t0.Add(2 * time.Minute) // past the rate limit
	ov, _ := m.Overview(context.Background(), nil, true)
	d := deviceOf(t, ov, a)
	if !d.ReadFailed || ov.ReadFailed != 1 || d.Reading.Template == nil || !d.ReadAt.Equal(t0) {
		t.Fatalf("failed refresh: failed=%v count=%d reading=%+v at=%v", d.ReadFailed, ov.ReadFailed, d.Reading.Template, d.ReadAt)
	}

	f.failRead[a] = false
	now = t0.Add(4 * time.Minute)
	ov, _ = m.Overview(context.Background(), nil, true)
	if d := deviceOf(t, ov, a); d.ReadFailed || ov.ReadFailed != 0 {
		t.Errorf("a successful read must clear the failure: %+v", d)
	}
}

// Why (#599): two people (or two tabs) opening the page while the fleet is being read must not send a
// second round to the same devices: the second request waits for the first and uses its readings.
func TestOverlappingRefreshesReadEachDeviceOnce(t *testing.T) {
	f := newFleet()
	f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	f.gate = make(chan struct{})
	now := t0
	m := clockedManager(f, &now)
	done := make(chan Overview, 2)
	go func() { ov, _ := m.Overview(context.Background(), nil, true); done <- ov }()
	for i := 0; m.mu.TryLock(); i++ { // not held yet: the first request has not started its refresh
		m.mu.Unlock()
		if i > 500 {
			t.Fatal("the first refresh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() { ov, _ := m.Overview(context.Background(), nil, true); done <- ov }()
	time.Sleep(100 * time.Millisecond) // the second request arrives while the devices are slow
	close(f.gate)
	var second Overview
	for i := 0; i < 2; i++ {
		select {
		case ov := <-done:
			if ov.Throttled {
				second = ov
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a request never finished")
		}
	}
	if !second.Throttled {
		t.Error("the second request ran its own refresh")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cmds) != 4 { // Module and Template of two devices, once
		t.Errorf("%d commands sent: %v; want 4 (one round)", len(f.cmds), f.cmds)
	}
}

// Why (#599): a stale reading is the last thing known about the device; a backup taken before it is older
// news and must not replace it (only a newer backup does).
func TestAStaleReadingBeatsAnOlderBackup(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	f.snap[a] = []byte(`{"Module":` + f.module[a] + `,"Template":` + f.template[a] + `}`) // taken t0-48h
	now := t0.Add(-2 * time.Hour)
	m := clockedManager(f, &now)
	_, _ = m.Overview(context.Background(), nil, true)
	now = t0
	ov, _ := m.Overview(context.Background(), nil, false)
	if d := deviceOf(t, ov, a); d.Source != SourceStale || !d.ReadAt.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("source %q read %v, want the stale reading (2 h old) over the backup (48 h old)", d.Source, d.ReadAt)
	}
}

// Why (#599): the tab reads the fleet by itself when it opens, as the page's own request. A person who
// clicks on (or presses Read again) before it is done ends that request. The devices did not fail: they
// must be read all the same (the rate limit keeps the run's result for a minute, so a cut-short run
// would leave them unread that long) and none may be shown as "refresh failed".
func TestARefreshWhoseRequestEndsStillReadsEveryDevice(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	b := f.add("b", "NOUS A1T", 21)
	f.gate = make(chan struct{})
	now := t0
	m := clockedManager(f, &now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = m.Overview(ctx, nil, true); close(done) }()
	for i := 0; m.mu.TryLock(); i++ { // not held yet: the refresh has not started
		m.mu.Unlock()
		if i > 500 {
			t.Fatal("the refresh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel() // the person left the page while the devices were still answering
	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never ended")
	}
	ov, _ := m.Overview(context.Background(), nil, false)
	if ov.ReadFailed != 0 {
		t.Errorf("%d devices shown as failed after the request ended", ov.ReadFailed)
	}
	for _, id := range []model.DeviceID{a, b} {
		if d := deviceOf(t, ov, id); d.Source != SourceLive {
			t.Errorf("device %d: source %q, want the reading of the refresh that was asked for", id, d.Source)
		}
	}
}

// Why: the preview reads every device live; a person who abandons it ends the request. That says
// nothing about the devices, so none may be marked as "refresh failed" on the tab.
func TestAnAbandonedPreviewMarksNoDeviceFailed(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	b := f.add("b", "NOUS A1T", 21)
	f.gate = make(chan struct{})
	defer close(f.gate)
	now := t0
	m := clockedManager(f, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = m.Preview(ctx, Request{Group: policy.GroupKey{Family: model.ChipESP8266, Name: "NOUS A1T"}, Reference: a, Devices: []model.DeviceID{b}})
	ov, _ := m.Overview(context.Background(), nil, false)
	if ov.ReadFailed != 0 {
		t.Errorf("%d devices shown as failed after an abandoned preview", ov.ReadFailed)
	}
}

// Why (#659): a device whose HTTP API is off and which MQTT cannot read is left alone by the template refresh like an
// offline one: nothing is sent, and it is not reported as a failed read (it was not tried), in the tab's refresh
// and in the background refresh of one device.
func TestRefreshLeavesADeviceNothingCanReadAlone(t *testing.T) {
	f := newFleet()
	a := f.add("a", "NOUS A1T", 21)
	b := f.add("b", "NOUS A1T", 21)
	now := t0
	m := New(Options{Devices: f, Client: f, Backups: f, Settings: f, Now: func() time.Time { return now },
		Skip: func(id model.DeviceID) bool { return id == a }})
	ov, _ := m.Overview(context.Background(), nil, true)
	for _, c := range f.cmds {
		if strings.HasPrefix(c, fmt.Sprintf("%d ", a)) {
			t.Fatalf("the device was asked: %v", f.cmds)
		}
	}
	if ov.ReadFailed != 0 || deviceOf(t, ov, b).Source != SourceLive {
		t.Errorf("failed %d, b: %+v", ov.ReadFailed, deviceOf(t, ov, b))
	}
	n := len(f.cmds)
	if err := m.Refresh(context.Background(), model.Device{ID: a}); err != nil || len(f.cmds) != n {
		t.Fatalf("the background refresh asked the device: %v, %v", err, f.cmds[n:])
	}
}
