package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/store"
)

// fakePWTester answers the password test per device and records the calls; the first call can be held, so a test
// can start a second set while the run is busy.
type fakePWTester struct {
	mu      sync.Mutex
	calls   []model.DeviceID
	active  int
	maxAct  int
	answers map[model.DeviceID]error
	hold    chan struct{} // the first call waits for it
	started chan struct{} // closed when the first call began
	once    sync.Once
}

func (f *fakePWTester) TestMQTTPassword(_ context.Context, id model.DeviceID) error {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	f.active++
	f.maxAct = max(f.maxAct, f.active)
	f.mu.Unlock()
	f.once.Do(func() {
		if f.started != nil {
			close(f.started)
		}
		if f.hold != nil {
			<-f.hold
		}
	})
	f.mu.Lock()
	f.active--
	err := f.answers[id]
	f.mu.Unlock()
	return err
}

func newPWFleet(t *testing.T, g *pwRig, tester *fakePWTester) (*mqttPasswordFleet, *sync.WaitGroup) {
	t.Helper()
	var wg sync.WaitGroup
	f := &mqttPasswordFleet{learner: g.l, st: g.st, tester: tester, audit: g.audit, clk: clock.Real{},
		log: slog.New(slog.NewTextHandler(g.logs, nil)), ctx: t.Context(),
		spawn: func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }}
	return f, &wg
}

// Why (#609, "Test each device afterwards"): the test is how a bulk-set password is checked, but it holds the
// device's topic for about a second and puts a settings chunk on the broker, so it runs one device at a time in
// the background, never inside the request. A device deleted meanwhile is skipped, a failing one does not stop
// the others, and a second bulk set that comes while the run is busy joins the queue: a set applied without its
// tests would leave the devices "Not tested" with nobody to test them. Only the devices the store really wrote
// are tested, each audit entry names the admin who confirmed and holds no password, and when the run is done the
// flag is clear again so the next set starts a new run.
func TestBackgroundPasswordTestsRunOneAtATimeAndSkipGoneDevices(t *testing.T) {
	g := newPWRig(t)
	a, b, gone, learned, late := g.device(t, "1"), g.device(t, "2"), g.device(t, "3"), g.device(t, "4"), g.device(t, "5")
	if ok, err := g.st.ApplyMQTTPasswordFromBackup(t.Context(), learned, model.MQTTPasswordOwn, "taught-secret-1", pwTaken); err != nil || !ok {
		t.Fatal(ok, err)
	}
	tester := &fakePWTester{hold: make(chan struct{}), started: make(chan struct{}),
		answers: map[model.DeviceID]error{b: mqttcmd.ErrPasswordRefused, gone: store.ErrNotFound}}
	f, wg := newPWFleet(t, g, tester)

	applied, err := f.UseBroker(t.Context(), "alice", []model.DeviceID{a, b, gone, learned}, true)
	if err != nil || len(applied) != 3 {
		t.Fatalf("applied %v, %v: the learned device must be left alone", applied, err)
	}
	<-tester.started
	if n := f.Testing(); n != 3 {
		t.Fatalf("Testing() = %d while the first test is in flight, want 3", n)
	}
	if applied, err = f.UseBroker(t.Context(), "bob", []model.DeviceID{late}, true); err != nil || len(applied) != 1 {
		t.Fatalf("a second set while busy: %v, %v", applied, err)
	}
	close(tester.hold)
	wg.Wait()

	if len(tester.calls) != 4 || tester.maxAct != 1 {
		t.Fatalf("calls %v, at most %d at once: want the four written devices, one at a time", tester.calls, tester.maxAct)
	}
	for _, id := range tester.calls {
		if id == learned {
			t.Fatalf("the learned device was tested: %v", tester.calls)
		}
	}
	if n := f.Testing(); n != 0 {
		t.Fatalf("Testing() = %d after the run, want 0", n)
	}
	var byActor = map[string]int{}
	for _, e := range g.audit.all() {
		if e.Action != "device.mqtt_password_test" {
			continue
		}
		byActor[e.Actor]++
		if strings.Contains(e.Detail, "secret") || strings.Contains(e.Detail, "broker-pw") {
			t.Errorf("audit holds a password: %+v", e)
		}
		if e.Target == "device:3" {
			t.Errorf("a deleted device was audited: %+v", e)
		}
	}
	if byActor["alice"] != 2 || byActor["bob"] != 1 {
		t.Fatalf("audit entries per admin %v, want alice 2 (the device that went away is skipped) and bob 1", byActor)
	}
	// the flag is clear: a new set starts a new run
	again := g.device(t, "6")
	tester.answers[again] = errors.New("no answer")
	if _, err := f.UseBroker(t.Context(), "alice", []model.DeviceID{again}, true); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(tester.calls) != 5 {
		t.Fatalf("the next set was not tested: %v", tester.calls)
	}
	// without the box nothing is queued
	off := g.device(t, "7")
	if _, err := f.UseBroker(t.Context(), "alice", []model.DeviceID{off}, false); err != nil {
		t.Fatal(err)
	}
	if n := f.Testing(); n != 0 { // the queue is filled before UseBroker returns
		t.Fatalf("a set without tests queued %d", n)
	}
	wg.Wait()
	if len(tester.calls) != 5 {
		t.Fatalf("a set without tests queued one: %v", tester.calls)
	}
}
