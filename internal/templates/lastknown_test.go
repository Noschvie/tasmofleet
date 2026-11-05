package templates

import (
	"context"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
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
