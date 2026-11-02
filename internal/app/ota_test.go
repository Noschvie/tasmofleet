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

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
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

// fakeHintTarget records what reaches the OTA engine, in order.
type fakeHintTarget struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeHintTarget) Hint(id model.DeviceID, report string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "hint "+report)
}

func (f *fakeHintTarget) PowerReport(id model.DeviceID, relay int, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("power %d/%d", id, relay))
}

func (f *fakeHintTarget) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

var errNotOurs = errors.New("not in the inventory")

func lookupAll(context.Context, string) (model.Device, error) { return model.Device{ID: 7}, nil }

func powerOff(topic, suffix string) mqttbus.Message {
	return mqttbus.Message{Kind: mqttbus.KindPower, Topic: topic, Suffix: suffix, Payload: []byte("OFF")}
}

func upgradeReport(topic, text string) mqttbus.Message {
	return mqttbus.Message{Kind: mqttbus.KindUpgrade, Topic: topic, Suffix: "UPGRADE", Payload: []byte(`{"Upgrade":"` + text + `"}`)}
}

// Why (#507): a broker reconnect makes the fleet report at once. Every UPGRADE report used to
// start its own goroutine with its own database lookup, so the burst became a goroutine and
// query storm. One worker does the lookups one after the other; the queue is bounded, and an
// UPGRADE report that does not fit is dropped and counted (it only ends a wait early, the job
// still stops at its deadline).
func TestMQTTUpgradeBurstRunsOneLookupAtATimeAndDropsTheOverflow(t *testing.T) {
	var inflight, peak atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var lookups atomic.Int32
	var dropped atomic.Int32
	target := &fakeHintTarget{}
	h := newOTAHints(func(context.Context, string) (model.Device, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		lookups.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return model.Device{ID: 7}, nil
	}, target, func(string) { dropped.Add(1) })
	h.setTopics("plug", "plug-0")
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })

	const burst = 2000
	h.onUpgradeReport(upgradeReport("plug-0", "Failed x")) // the worker takes this one and blocks in the lookup
	<-started
	for range burst {
		h.onUpgradeReport(upgradeReport("plug", "Failed x"))
	}
	if d := int(dropped.Load()); d != burst-otaHintQueue {
		t.Fatalf("dropped %d of %d reports with a full queue of %d, want %d", d, burst, otaHintQueue, burst-otaHintQueue)
	}
	close(release)
	deadline := time.After(10 * time.Second)
	for len(target.seen()) < otaHintQueue+1 {
		select {
		case <-deadline:
			t.Fatalf("only %d reports reached the engine", len(target.seen()))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d lookups ran at the same time, want 1 worker", p)
	}
	if n := int(lookups.Load()); n != otaHintQueue+1 {
		t.Fatalf("%d lookups for %d accepted reports", n, otaHintQueue+1)
	}
}

// blockedHints returns hints whose worker is parked in the lookup of the first report until
// release is closed. The lookup maps a topic "plug-N" to device N.
func blockedHints(t *testing.T, dropped func(string), topics ...string) (h *otaHints, target *fakeHintTarget, release chan struct{}, stop func()) {
	t.Helper()
	started := make(chan struct{}, 1)
	release = make(chan struct{})
	target = &fakeHintTarget{}
	h = newOTAHints(func(_ context.Context, topic string) (model.Device, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		var id int
		if _, err := fmt.Sscanf(topic, "plug-%d", &id); err != nil {
			return model.Device{}, err
		}
		return model.Device{ID: model.DeviceID(id)}, nil
	}, target, dropped)
	h.setTopics(append(topics, "plug-0")...)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	h.onPowerReport(powerOff("plug-0", "POWER")) // the worker takes this one and blocks in the lookup
	<-started
	return h, target, release, func() { cancel(); wg.Wait() }
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

// Why (#534): the engine learns from a POWER OFF report that somebody switched a relay off
// during an update; if the report is lost, the power restore after the update switches that
// relay back ON. A flood that fills the queue (UPGRADE reports, or more relays than the queue
// has room for) must therefore never cost an OFF report: every relay's OFF reaches the engine,
// and only the UPGRADE overflow is dropped and counted.
func TestPowerOffBurstLargerThanTheQueueStillReachesTheEngineForEveryRelay(t *testing.T) {
	var dropped atomic.Int32
	topics := []string{"plug-1"}
	const devices, relays = 300, 3 // 900 distinct relays, more than three queues
	for dev := 1; dev <= devices; dev++ {
		topics = append(topics, fmt.Sprintf("plug-%d", dev))
	}
	h, target, release, stop := blockedHints(t, func(string) { dropped.Add(1) }, topics...)
	defer stop()

	for range 2 * otaHintQueue { // the queue for UPGRADE reports is full and overflowing
		h.onUpgradeReport(upgradeReport("plug-1", "Failed x"))
	}
	for dev := 1; dev <= devices; dev++ {
		for relay := 1; relay <= relays; relay++ {
			h.onPowerReport(powerOff(fmt.Sprintf("plug-%d", dev), fmt.Sprintf("POWER%d", relay)))
		}
	}
	if d := int(dropped.Load()); d != otaHintQueue {
		t.Fatalf("dropped %d reports, want exactly the %d UPGRADE reports that did not fit; an OFF report must never be dropped", d, otaHintQueue)
	}
	close(release)
	waitForCalls(t, target, 1+otaHintQueue+devices*relays)

	got := map[string]int{}
	for _, c := range target.seen() {
		got[c]++
	}
	for dev := 1; dev <= devices; dev++ {
		for relay := 1; relay <= relays; relay++ {
			if c := fmt.Sprintf("power %d/%d", dev, relay); got[c] != 1 {
				t.Fatalf("%q reached the engine %d times, want once", c, got[c])
			}
		}
	}
}

// Why (#534): the OFF reports are coalesced per topic and relay, so a flood from one chatty
// device cannot grow the waiting set; the engine only needs to hear once that the relay is
// off. A report that arrives after the earlier one was taken is a new one and is delivered
// again, so a relay switched off a second time during the same update is not forgotten.
func TestRepeatedPowerOffOfOneRelayIsCoalescedWhileWaitingOnly(t *testing.T) {
	h, target, release, stop := blockedHints(t, func(reason string) { t.Error("dropped:", reason) }, "plug-5")
	defer stop()

	// plug-0/1 is in flight (the worker is parked in its lookup); plug-5/2 waits.
	for range 1000 {
		h.onPowerReport(powerOff("plug-5", "POWER2"))
	}
	h.onPowerReport(powerOff("plug-0", "POWER")) // same relay as the one in flight: a new report, waits
	if n := len(h.pending); n != 2 {
		t.Fatalf("%d reports waiting, want 2 (one per relay)", n)
	}
	close(release)
	waitForCalls(t, target, 3)
	want := []string{"power 0/1", "power 5/2", "power 0/1"}
	if got := target.seen(); !slices.Equal(got, want) {
		t.Fatalf("engine calls = %q, want %q", got, want)
	}
}

// Why (#507): shutdown must not race the store and the engine. The worker is started with goBG,
// so waiting for the background tasks waits for the lookup in flight, and the report is
// delivered before shutdown moves on to close them.
func TestShutdownWaitsForAnInFlightMQTTHint(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	a := &runtime{svc: &Services{}, bgContext: ctx}
	started := make(chan struct{})
	release := make(chan struct{})
	target := &fakeHintTarget{}
	h := a.startOTAHints(func(context.Context, string) (model.Device, error) {
		close(started)
		<-release
		return model.Device{ID: 7}, nil
	}, func(context.Context) ([]model.Device, error) { return []model.Device{{ID: 7, Topic: "plug"}}, nil }, target)

	h.onUpgradeReport(upgradeReport("plug", "Failed File Not Found (404)"))
	<-started
	cancel() // the application shuts down while the lookup runs
	done := make(chan struct{})
	go func() { a.bgWG.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown finished while a hint lookup was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish after the lookup returned")
	}
	if got := target.seen(); !slices.Equal(got, []string{"hint Failed File Not Found (404)"}) {
		t.Fatalf("engine calls at shutdown = %q", got)
	}
}

// Why (#507): two reports of one device must reach the engine in the order the device sent
// them; a goroutine per message lets the later one overtake the earlier one.
func TestMQTTHintsReachTheEngineInArrivalOrder(t *testing.T) {
	target := &fakeHintTarget{}
	h := newOTAHints(lookupAll, target, func(reason string) { t.Error("dropped with room in the queue:", reason) })
	h.setTopics("plug")
	var want []string
	for i := range 40 {
		if i%2 == 0 {
			h.onUpgradeReport(upgradeReport("plug", "Failed "+string(rune('A'+i))))
			want = append(want, "hint Failed "+string(rune('A'+i)))
		} else {
			h.onPowerReport(powerOff("plug", fmt.Sprintf("POWER%d", 1+i/2)))
			want = append(want, fmt.Sprintf("power 7/%d", 1+i/2))
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	deadline := time.After(10 * time.Second)
	for len(target.seen()) < len(want) {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d reports delivered", len(target.seen()), len(want))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, want) {
		t.Fatalf("order changed:\n got %q\nwant %q", got, want)
	}
}

// Why (#534, security review): the topic of an MQTT report is whatever a client of the broker
// publishes. If every new topic took a slot in the waiting set, a flood of made-up topics would
// grow it without bound and could crowd out the OFF report of a real device. Only topics of the
// inventory are taken in; the flood is counted and leaves the set empty, and a real device's OFF
// still arrives.
func TestAFloodOfMadeUpTopicsNeitherGrowsTheHintSetsNorCrowdsOutADevice(t *testing.T) {
	reasons := map[string]int{}
	target := &fakeHintTarget{}
	h := newOTAHints(lookupAll, target, func(reason string) { reasons[reason]++ })
	h.setTopics("plug")
	const flood = 100000
	for i := range flood {
		h.onPowerReport(powerOff(fmt.Sprintf("junk-%d", i), "POWER1"))
		h.onUpgradeReport(upgradeReport(fmt.Sprintf("junk-%d", i), "Failed x"))
	}
	h.mu.Lock()
	waiting, offs, upgrades := len(h.pending), len(h.offs), h.upgrades
	h.mu.Unlock()
	if waiting != 0 || offs != 0 || upgrades != 0 {
		t.Fatalf("%d reports waiting (%d OFF, %d UPGRADE) after a flood of unknown topics, want none", waiting, offs, upgrades)
	}
	if got := reasons[hintDropUnknownTopic]; got != 2*flood || len(reasons) != 1 {
		t.Fatalf("drops by reason = %v, want %d x %s", reasons, 2*flood, hintDropUnknownTopic)
	}
	h.onPowerReport(powerOff("plug", "POWER2"))
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	waitForCalls(t, target, 1)
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, []string{"power 7/2"}) {
		t.Fatalf("engine calls = %q", got)
	}
}

// Why (#534): the set of admitted topics must follow the inventory, or a device added after the
// start would never get its OFF reports through, and a deleted one would keep a slot. A failed
// read must not empty the set (every OFF would then be dropped until the next read), and the
// worker itself re-reads it on its timer.
func TestHintTopicsFollowTheInventory(t *testing.T) {
	var mu sync.Mutex
	inventory := []model.Device{{ID: 1, Topic: "plug-a"}, {ID: 2}} // a device without a topic never matches
	var listErr error
	var reads atomic.Int32
	h := newOTAHints(lookupAll, &fakeHintTarget{}, func(string) {})
	h.list = func(context.Context) ([]model.Device, error) {
		reads.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(inventory), listErr
	}
	known := func(topic string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		_, ok := h.topics[topic]
		return ok
	}
	h.refreshTopics(t.Context())
	if !known("plug-a") || known("plug-b") || known("") {
		t.Fatalf("topics after the first read = %v", h.topics)
	}
	mu.Lock()
	inventory = []model.Device{{ID: 2, Topic: "plug-b"}}
	mu.Unlock()
	h.refreshTopics(t.Context())
	if known("plug-a") || !known("plug-b") {
		t.Fatalf("topics after a device was replaced = %v", h.topics)
	}
	mu.Lock()
	listErr = errors.New("database is locked")
	mu.Unlock()
	h.refreshTopics(t.Context())
	if !known("plug-b") {
		t.Fatal("a failed read emptied the known topics")
	}
	mu.Lock()
	listErr = nil
	inventory = []model.Device{{ID: 3, Topic: "plug-c"}}
	mu.Unlock()
	h.every = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	deadline := time.After(10 * time.Second)
	for !known("plug-c") {
		select {
		case <-deadline:
			t.Fatal("the worker never re-read the inventory")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}

// Why: only a relay switched OFF by somebody (not a retained or old value, not ON) may stop the
// power restore, and only a device in the inventory gets a hint. Everything else must neither
// take queue room nor reach the engine. A relay the engine does not know and a topic that is no
// device are counted, so a broker that carries junk shows up; a device deleted since the last
// refresh of the topics is still looked up and found missing.
func TestMQTTHintsIgnoreWhatTheEngineCannotUse(t *testing.T) {
	target := &fakeHintTarget{}
	lookup := func(_ context.Context, topic string) (model.Device, error) {
		if topic == "stranger" {
			return model.Device{}, errNotOurs
		}
		return model.Device{ID: 7}, nil
	}
	reasons := map[string]int{}
	h := newOTAHints(lookup, target, func(reason string) { reasons[reason]++ })
	h.setTopics("plug", "stranger")
	retained := powerOff("plug", "POWER")
	retained.Retained = true
	on := powerOff("plug", "POWER")
	on.Payload = []byte("ON")
	for _, m := range []mqttbus.Message{retained, on, powerOff("plug", "STATE"), powerOff("plug", "POWER0"), powerOff("plug", "POWER33")} {
		h.onPowerReport(m)
	}
	h.onUpgradeReport(mqttbus.Message{Kind: mqttbus.KindUpgrade, Topic: "plug", Suffix: "UPGRADE", Payload: []byte("not json")})
	if n := len(h.pending); n != 0 {
		t.Fatalf("%d ignorable reports were queued", n)
	}
	h.onPowerReport(powerOff("stranger", "POWER"))
	h.onUpgradeReport(upgradeReport("stranger", "Failed x"))
	h.onPowerReport(powerOff("nobody", "POWER"))
	h.onUpgradeReport(upgradeReport("nobody", "Failed x"))
	h.onPowerReport(powerOff("plug", "POWER2"))
	if want := map[string]int{hintDropInvalidRelay: 3, hintDropUnknownTopic: 2}; !maps.Equal(reasons, want) {
		t.Fatalf("drops by reason = %v, want %v", reasons, want)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { h.run(ctx) })
	deadline := time.After(10 * time.Second)
	for len(target.seen()) < 1 {
		select {
		case <-deadline:
			t.Fatal("the report of a known device never reached the engine")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
	if got := target.seen(); !slices.Equal(got, []string{"power 7/2"}) {
		t.Fatalf("engine calls = %q", got)
	}
}
