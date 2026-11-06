package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
)

// MQTT stat/<t>/UPGRADE reports reach the engine as hints; a "Failed ..."
// report lets a job stop waiting for a download that will never come.
// Anything that is not {"Upgrade": "<text>"} must be ignored, not guessed.
func TestUpgradeReportText(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"Upgrade":"Failed File Not Found (404)"}`, "Failed File Not Found (404)", true},
		{`{"Upgrade":"Successful. Restarting"}`, "Successful. Restarting", true},
		{`{"Upgrade":""}`, "", true},
		{`{"Upload":"Failed"}`, "", false},
		{`{"Upgrade":1}`, "", false},
		{`not json`, "", false},
	}
	for _, c := range cases {
		got, ok := upgradeReportText([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// fakeHintTarget records what reaches the OTA engine, in order. gate (may be nil) runs first in
// every call, so a test can park the worker in the engine.
type fakeHintTarget struct {
	mu    sync.Mutex
	calls []string
	gate  func()
}

func (f *fakeHintTarget) Hint(id model.DeviceID, report string) {
	if f.gate != nil {
		f.gate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("hint %d %s", id, report))
}

func (f *fakeHintTarget) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func dev(id int, topic string) model.Device {
	return model.Device{ID: model.DeviceID(id), Topic: topic}
}

func upgradeReport(topic, text string) mqttbus.Message {
	return mqttbus.Message{Kind: mqttbus.KindUpgrade, Topic: topic, Suffix: "UPGRADE", Payload: []byte(`{"Upgrade":"` + text + `"}`)}
}

// parkedTarget is a target whose first call blocks until release is closed (entered says it got there),
// and which counts how many calls run at the same time.
func parkedTarget() (target *fakeHintTarget, entered chan struct{}, release chan struct{}, peak *atomic.Int32) {
	entered, release, peak = make(chan struct{}, 1), make(chan struct{}), new(atomic.Int32)
	var inflight atomic.Int32
	target = &fakeHintTarget{}
	target.gate = func() {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	return target, entered, release, peak
}

func waitForCalls(t *testing.T, target *fakeHintTarget, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for len(target.seen()) < n {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d reports reached the engine", len(target.seen()), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Why (#507): a broker reconnect makes the fleet report at once. Every UPGRADE report used to
// start its own goroutine, so the burst became a goroutine storm. One worker hands the reports
// over one after the other; the queue is bounded, and an UPGRADE report that does not fit is
// dropped and counted (it only ends a wait early, the job still stops at its deadline).
func TestMQTTUpgradeBurstIsHandedOverOneAtATimeAndDropsTheOverflow(t *testing.T) {
	target, entered, release, peak := parkedTarget()
	var dropped atomic.Int32
	h := newOTAHints(target, func(string) { dropped.Add(1) })
	h.setDevices(dev(7, "plug"))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })

	const burst = 2000
	h.onUpgradeReport(upgradeReport("plug", "Failed x")) // the worker takes this one and parks in the engine
	<-entered
	for range burst {
		h.onUpgradeReport(upgradeReport("plug", "Failed x"))
	}
	if d := int(dropped.Load()); d != burst-otaHintQueue {
		t.Fatalf("dropped %d of %d reports with a full queue of %d, want %d", d, burst, otaHintQueue, burst-otaHintQueue)
	}
	close(release)
	waitForCalls(t, target, otaHintQueue+1)
	cancel()
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d reports were handed over at the same time, want 1 worker", p)
	}
	if n := len(target.seen()); n != otaHintQueue+1 {
		t.Fatalf("%d reports reached the engine, want the %d that were accepted", n, otaHintQueue+1)
	}
}

// blockedHints returns hints whose worker is parked in the engine with the report of the
// device "parked" (ID 9999) until release is closed; devs are the other devices of the inventory.
func blockedHints(t *testing.T, dropped func(string), devs ...model.Device) (h *otaHints, target *fakeHintTarget, release chan struct{}, stop func()) {
	t.Helper()
	target, entered, release, _ := parkedTarget()
	h = newOTAHints(target, dropped)
	h.setDevices(append(devs, dev(9999, "parked"))...)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	h.onUpgradeReport(upgradeReport("parked", "Failed x")) // the worker takes this one and parks in the engine
	<-entered
	return h, target, release, func() { cancel(); wg.Wait() }
}

// Why (#534, review): a report used to be looked up in the database when the worker got to it,
// and a lookup that failed or timed out lost the report silently. The device is now known when
// the report arrives (from the topics of the inventory), so nothing that can fail stands between
// an accepted report and the engine, not even a device that is deleted or renamed in between.
func TestAnAcceptedReportIsHandedOverToTheDeviceItCameFrom(t *testing.T) {
	var dropped atomic.Int32
	h, target, release, stop := blockedHints(t, func(string) { dropped.Add(1) }, dev(5, "plug-5"))
	defer stop()
	h.onUpgradeReport(upgradeReport("plug-5", "Failed a")) // waits behind the parked one
	h.setDevices(dev(9999, "parked"), dev(6, "plug-5"))
	h.onUpgradeReport(upgradeReport("plug-5", "Failed b")) // the topic now belongs to another device
	h.setDevices(dev(9999, "parked"))
	h.onUpgradeReport(upgradeReport("plug-5", "Failed c")) // and now to none: dropped and counted
	if dropped.Load() != 1 {
		t.Fatalf("%d reports dropped, want the one of the unknown topic", dropped.Load())
	}
	close(release)
	waitForCalls(t, target, 3)
	if got, want := target.seen(), []string{"hint 9999 Failed x", "hint 5 Failed a", "hint 6 Failed b"}; !slices.Equal(got, want) {
		t.Fatalf("engine calls = %q, want %q", got, want)
	}
}

// Why (#507): shutdown must not race the engine. The worker is started with goBG, so waiting
// for the background tasks waits for the report in flight, and the report is handed over before
// shutdown moves on to close the engine.
func TestShutdownWaitsForAnInFlightMQTTHint(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	a := &runtime{svc: &Services{}, bgContext: ctx}
	target, entered, release, _ := parkedTarget()
	h := a.startOTAHints(func(context.Context) ([]model.Device, error) { return []model.Device{dev(7, "plug")}, nil }, target)

	h.onUpgradeReport(upgradeReport("plug", "Failed File Not Found (404)"))
	<-entered
	cancel() // the application shuts down while the report is being handed over
	done := make(chan struct{})
	go func() { a.bgWG.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown finished while a hint was still being handed over")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish after the hand-over returned")
	}
	if got := target.seen(); !slices.Equal(got, []string{"hint 7 Failed File Not Found (404)"}) {
		t.Fatalf("engine calls at shutdown = %q", got)
	}
}

// Why (#507): two reports of one device must reach the engine in the order the device sent
// them; a goroutine per message lets the later one overtake the earlier one.
func TestMQTTHintsReachTheEngineInArrivalOrder(t *testing.T) {
	target := &fakeHintTarget{}
	h := newOTAHints(target, func(reason string) { t.Error("dropped with room in the queue:", reason) })
	h.setDevices(dev(7, "plug"))
	var want []string
	for i := range 40 {
		h.onUpgradeReport(upgradeReport("plug", fmt.Sprintf("Failed %d", i)))
		want = append(want, fmt.Sprintf("hint 7 Failed %d", i))
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	waitForCalls(t, target, len(want))
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, want) {
		t.Fatalf("order changed:\n got %q\nwant %q", got, want)
	}
}

// Why (#534, security review): the topic of an MQTT report is whatever a client of the broker
// publishes. If every new topic took a slot in the waiting set, a flood of made-up topics would
// grow it without bound and could crowd out the report of a real device. Only topics of the
// inventory are taken in; the flood is counted and leaves the set empty, and a real device's
// report still arrives.
func TestAFloodOfMadeUpTopicsNeitherGrowsTheHintSetNorCrowdsOutADevice(t *testing.T) {
	reasons := map[string]int{}
	target := &fakeHintTarget{}
	h := newOTAHints(target, func(reason string) { reasons[reason]++ })
	h.setDevices(dev(7, "plug"))
	const flood = 100000
	for i := range flood {
		h.onUpgradeReport(upgradeReport(fmt.Sprintf("junk-%d", i), "Failed x"))
	}
	h.mu.Lock()
	waiting := len(h.pending)
	h.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d reports waiting after a flood of unknown topics, want none", waiting)
	}
	if got := reasons[hintDropUnknownTopic]; got != flood || len(reasons) != 1 {
		t.Fatalf("drops by reason = %v, want %d x %s", reasons, flood, hintDropUnknownTopic)
	}
	h.onUpgradeReport(upgradeReport("plug", "Failed x"))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	waitForCalls(t, target, 1)
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, []string{"hint 7 Failed x"}) {
		t.Fatalf("engine calls = %q", got)
	}
}

// Why (#534): the set of admitted topics must follow the inventory, or a device added after the
// start would never get its OFF reports through, and a deleted one would keep a slot. A failed
// read must not empty the set (every OFF would then be dropped until the next read), and the
// worker itself re-reads it on its timer.
func TestHintTopicsFollowTheInventory(t *testing.T) {
	var mu sync.Mutex
	inventory := []model.Device{dev(1, "plug-a"), {ID: 2}} // a device without a topic never matches
	var listErr error
	h := newOTAHints(&fakeHintTarget{}, func(string) {})
	h.list = func(context.Context) ([]model.Device, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(inventory), listErr
	}
	idOf := func(topic string) (model.DeviceID, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		id, ok := h.ids[topic]
		return id, ok
	}
	h.refreshTopics(t.Context())
	if id, ok := idOf("plug-a"); !ok || id != 1 {
		t.Fatalf("plug-a = %v %v after the first read", id, ok)
	}
	if _, ok := idOf("plug-b"); ok {
		t.Fatal("plug-b is known before it exists")
	}
	if _, ok := idOf(""); ok {
		t.Fatal("the empty topic is known")
	}
	mu.Lock()
	inventory = []model.Device{dev(2, "plug-b")}
	mu.Unlock()
	h.refreshTopics(t.Context())
	if _, ok := idOf("plug-a"); ok {
		t.Fatal("a deleted device keeps its topic")
	}
	if id, ok := idOf("plug-b"); !ok || id != 2 {
		t.Fatalf("plug-b = %v %v after the device was replaced", id, ok)
	}
	mu.Lock()
	listErr = errors.New("database is locked")
	mu.Unlock()
	h.refreshTopics(t.Context())
	if _, ok := idOf("plug-b"); !ok {
		t.Fatal("a failed read emptied the known topics")
	}
	mu.Lock()
	listErr = nil
	inventory = []model.Device{dev(3, "plug-c")}
	mu.Unlock()
	h.every = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	deadline := time.After(10 * time.Second)
	for {
		if id, ok := idOf("plug-c"); ok && id == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the worker never re-read the inventory")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}

// Why: only {"Upgrade": "<text>"} of a device in the inventory is a hint. Everything else must
// neither take queue room nor reach the engine, and a topic that is no device is counted, so a
// broker that carries junk shows up. POWER reports are not hints at all any more (#551): the
// engine does not look at relays, so nothing subscribes to them.
func TestMQTTHintsIgnoreWhatTheEngineCannotUse(t *testing.T) {
	target := &fakeHintTarget{}
	reasons := map[string]int{}
	h := newOTAHints(target, func(reason string) { reasons[reason]++ })
	h.setDevices(dev(7, "plug"))
	h.onUpgradeReport(mqttbus.Message{Kind: mqttbus.KindUpgrade, Topic: "plug", Suffix: "UPGRADE", Payload: []byte("not json")})
	if n := len(h.pending); n != 0 {
		t.Fatalf("%d ignorable reports were queued", n)
	}
	h.onUpgradeReport(upgradeReport("nobody", "Failed x"))
	if want := map[string]int{hintDropUnknownTopic: 1}; !maps.Equal(reasons, want) {
		t.Fatalf("drops by reason = %v, want %v", reasons, want)
	}
	h.onUpgradeReport(upgradeReport("plug", "Failed y"))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	waitForCalls(t, target, 1)
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, []string{"hint 7 Failed y"}) {
		t.Fatalf("engine calls = %q", got)
	}
}
