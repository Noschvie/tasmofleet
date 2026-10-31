package app

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
)

type fakeBusState struct{ enabled, connected atomic.Bool }

func (f *fakeBusState) Enabled() bool   { return f.enabled.Load() }
func (f *fakeBusState) Connected() bool { return f.connected.Load() }

func gotEvent(t *testing.T, ch <-chan events.Event, want bool) {
	t.Helper()
	select {
	case e := <-ch:
		if !want {
			t.Fatalf("unexpected event %+v", e)
		}
	case <-time.After(100 * time.Millisecond):
		if want {
			t.Fatal("no event")
		}
	}
}

// Why (#307): the bar must learn that the broker went away or came back without the browser
// polling; the server tells the event stream exactly when the state flips, and not on every check.
func TestBrokerWatchPublishesOnlyChanges(t *testing.T) {
	bus := &fakeBusState{}
	hub := events.NewHub(8)
	ch, cancel := hub.Subscribe(events.TopicBroker)
	defer cancel()
	w := newBrokerWatch(bus, hub)
	t0 := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }

	w.check()
	gotEvent(t, ch, false) // nothing changed

	bus.enabled.Store(true)
	w.SetHost("tcp://user:pa55@mqtt.lab.home:1883/some/path")
	w.check()
	gotEvent(t, ch, true) // MQTT turned on, broker down
	bus.connected.Store(true)
	t0 = t0.Add(time.Minute)
	w.check()
	gotEvent(t, ch, true)
	w.check()
	gotEvent(t, ch, false)

	st := w.Status()
	if !st.Enabled || !st.Connected || !st.Since.Equal(t0) {
		t.Errorf("status %+v, want connected since %v", st, t0)
	}
	if st.Host != "mqtt.lab.home:1883" {
		t.Errorf("host %q: the bar shows host and port only, never the user, the password or the path", st.Host)
	}

	bus.connected.Store(false)
	w.check()
	gotEvent(t, ch, true)
	if w.Status().Connected {
		t.Error("a lost connection still reads connected")
	}
}

// Why: another broker (settings page) is a change the bar must show even if both are connected.
func TestBrokerWatchSeesAnotherBroker(t *testing.T) {
	bus := &fakeBusState{}
	bus.enabled.Store(true)
	bus.connected.Store(true)
	hub := events.NewHub(8)
	ch, cancel := hub.Subscribe(events.TopicBroker)
	defer cancel()
	w := newBrokerWatch(bus, hub)
	w.SetHost("tcp://a.home:1883")
	w.check()
	gotEvent(t, ch, true)
	w.SetHost("tcp://b.home:1883")
	w.check()
	gotEvent(t, ch, true)
	if w.Status().Host != "b.home:1883" {
		t.Errorf("host %q", w.Status().Host)
	}
}

// Why: the bar says "last scheduled run: 3 of 58 failed". A run that was cut short by shutdown is
// no result and must not replace the last real one; a finished one must show up with its counts.
func TestScheduledRunKeepsTheLastFinishedRun(t *testing.T) {
	hub := events.NewHub(64)
	ch, cancel := hub.Subscribe(events.TopicBackup)
	defer cancel()
	r := newScheduledRun(hub)
	end := time.Date(2026, 10, 31, 3, 5, 0, 0, time.UTC)
	r.now = func() time.Time { return end }

	if s := r.Status(); s.Running || !s.Finished.IsZero() {
		t.Fatalf("fresh: %+v", s)
	}
	r.Start(3)
	r.Step(false)
	if s := r.Status(); !s.Running || s.Done != 1 || s.Total != 3 {
		t.Fatalf("in progress: %+v", s)
	}
	r.Step(true)
	r.Step(false)
	r.Finish(true)
	s := r.Status()
	if s.Running || !s.Finished.Equal(end) || s.LastTotal != 3 || s.LastFailed != 1 {
		t.Fatalf("finished: %+v", s)
	}
	for range 5 { // start, three steps, finish: every change reached the stream
		gotEvent(t, ch, true)
	}

	r.Start(5)
	r.Step(true)
	r.Finish(false) // shutdown
	s = r.Status()
	if s.Running || s.LastTotal != 3 || s.LastFailed != 1 {
		t.Errorf("a cut-short run replaced the last finished one: %+v", s)
	}
}
