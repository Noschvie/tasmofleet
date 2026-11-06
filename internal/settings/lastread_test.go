package settings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// gateClient answers like fakeClient but holds every command until the gate is opened, which is a
// slow device. started is closed by the first command.
type gateClient struct {
	fakeClient
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (g *gateClient) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.fakeClient.Command(ctx, t, cmnd)
}

func (g *gateClient) sentCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sent)
}

func newGated() *gateClient {
	return &gateClient{
		fakeClient: fakeClient{replies: map[string]string{"Topic": `{"Topic":"plug-1"}`}},
		gate:       make(chan struct{}), started: make(chan struct{}),
	}
}

// Why: the pages show what was last read, with its age. The age is only honest if it is the time of
// the read, and the "stale" line is the one the maintainer fixed at one hour (#599).
func TestLastReadRemembersValuesAndWhenTheyWereRead(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	c := &fakeClient{replies: map[string]string{"Topic": `{"Topic":"plug-1"}`}}
	m := New(Options{Devices: fakeDevices{}, Client: c, Now: func() time.Time { return now }})
	if _, ok := m.Last(1); ok {
		t.Fatal("a device that was never read has no last values")
	}
	if _, err := m.Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	s, ok := m.Last(1)
	if !ok || s.Values["Topic"] != "plug-1" || !s.At.Equal(now) {
		t.Fatalf("Last = %+v, %v", s, ok)
	}
	if s.Stale(now.Add(StaleAfter)) {
		t.Error("exactly one hour old is not yet stale")
	}
	if !s.Stale(now.Add(StaleAfter + time.Second)) {
		t.Error("older than one hour must be marked stale, or an old value looks current")
	}
	s.Values["Topic"] = "changed by a caller"
	if again, _ := m.Last(1); again.Values["Topic"] != "plug-1" {
		t.Error("a caller changed the remembered values through its copy")
	}
}

// Why: a device that stops answering must not blank the page. The failed read leaves the last
// values and their old time, so the page can say "refresh failed, showing values from 3 min ago".
func TestFailedReadKeepsTheLastValues(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	c := &fakeClient{replies: map[string]string{"Topic": `{"Topic":"plug-1"}`}}
	m := New(Options{Devices: fakeDevices{}, Client: c, Now: func() time.Time { return now }})
	if _, err := m.Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	c.mu.Lock()
	c.errs = map[string]error{"Hostname": tasmota.ErrUnreachable} // the first field read fails
	c.mu.Unlock()
	if _, err := m.Read(context.Background(), 1); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("Read = %v, want the device error", err)
	}
	s, ok := m.Last(1)
	if !ok || s.Values["Topic"] != "plug-1" || !s.At.Equal(now.Add(-3*time.Minute)) {
		t.Fatalf("after a failed read Last = %+v, %v; want the old values with their old time", s, ok)
	}
}

// Why: opening the page twice (a second tab, a reload while the device is slow) must not send a
// second round of ~30 queries to a small ESP: the second reader waits for the read under way and
// gets its result.
func TestSecondReadDuringAReadSendsNothing(t *testing.T) {
	base := newGated()
	close(base.gate)
	one := New(Options{Devices: fakeDevices{}, Client: base})
	if _, err := one.Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	perRead := base.sentCount() // what one complete Read sends

	c := newGated()
	m := New(Options{Devices: fakeDevices{}, Client: c})
	type res struct {
		vals map[string]string
		err  error
	}
	out := make(chan res, 2)
	go func() { v, err := m.Read(context.Background(), 1); out <- res{v, err} }()
	select {
	case <-c.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first read never reached the device")
	}
	go func() { v, err := m.Read(context.Background(), 1); out <- res{v, err} }()
	time.Sleep(100 * time.Millisecond) // the second reader arrives while the device is still answering
	close(c.gate)
	for i := 0; i < 2; i++ {
		select {
		case r := <-out:
			if r.err != nil || r.vals["Topic"] != "plug-1" {
				t.Fatalf("read %d: %v %v", i, r.vals, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a reader never got its result")
		}
	}
	if got := c.sentCount(); got != perRead {
		t.Errorf("two overlapping reads sent %d commands, one read sends %d", got, perRead)
	}
}

// Why: a reader that gives up (the person closed the tab) must not fail the others, and the read
// it started is finished so the next open has the values.
func TestAReaderLeavingDoesNotAbortTheSharedRead(t *testing.T) {
	c := newGated()
	m := New(Options{Devices: fakeDevices{}, Client: c})
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() { _, err := m.Read(ctx, 1); gone <- err }()
	<-c.started
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leaving reader got %v", err)
	}
	close(c.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := m.Last(1); ok && s.Values["Topic"] == "plug-1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the read did not finish after its first reader left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Why: after a write the remembered values describe a device that no longer exists; showing them as
// "last known" would put the old value back in front of the person.
func TestApplyForgetsTheRememberedValues(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"Topic": `{"Topic":"plug-1"}`}}
	m := New(Options{Devices: fakeDevices{}, Client: c})
	if _, err := m.Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(context.Background(), "u", model.DeviceID(1), map[string]string{"FriendlyName1": "Lamp"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Last(1); ok {
		t.Error("values remembered from before a write are still offered")
	}
}

// writeRace is a device in the middle of a slow read when a write arrives: FriendlyName1 answers "Old"
// until the write "FriendlyName1 Lamp" is sent. The first query of a read is answered at once, every
// later one waits for the gate (a write is never held), so the read has the old name before the write
// and finishes after it.
type writeRace struct {
	fakeClient
	gate    chan struct{}
	held    chan struct{} // closed when a read first waits for the gate
	once    sync.Once
	queries atomic.Int32
}

func newWriteRace() *writeRace {
	return &writeRace{
		fakeClient: fakeClient{replies: map[string]string{"FriendlyName1": `{"FriendlyName1":"Old"}`}},
		gate:       make(chan struct{}), held: make(chan struct{}),
	}
}

func (w *writeRace) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if cmnd == "FriendlyName1 Lamp" {
		w.mu.Lock()
		w.replies["FriendlyName1"] = `{"FriendlyName1":"Lamp"}`
		w.mu.Unlock()
		return w.fakeClient.Command(ctx, t, cmnd)
	}
	if w.queries.Add(1) > 1 {
		w.once.Do(func() { close(w.held) })
		select {
		case <-w.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return w.fakeClient.Command(ctx, t, cmnd)
}

// startRead runs Read in the background; its result arrives on the channel.
func startRead(m *Manager) <-chan map[string]string {
	out := make(chan map[string]string, 1)
	go func() {
		v, _ := m.Read(context.Background(), 1)
		out <- v
	}()
	return out
}

func waitFor[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// Why: a read that was under way when the person saved holds values from before the save, and it ends
// after it. Remembering it (stamped with the time it ended) would put the old name back on the tab as
// "read just now", which is what Apply forgetting the values is there to prevent.
func TestAReadUnderWayDuringAWriteIsNotRemembered(t *testing.T) {
	c := newWriteRace()
	m := New(Options{Devices: fakeDevices{}, Client: c})
	before := startRead(m)
	waitFor(t, c.held, "the read to be under way")
	if _, err := m.Apply(context.Background(), "u", 1, map[string]string{"FriendlyName1": "Lamp"}); err != nil {
		t.Fatal(err)
	}
	close(c.gate)
	if v := waitFor(t, before, "the read that started before the write"); v["FriendlyName1"] != "Old" {
		t.Fatalf("the read under way got %q; the test needs it to have read before the write", v["FriendlyName1"])
	}
	if s, ok := m.Last(1); ok {
		t.Errorf("Last = %q read at %v: a read that started before the write is remembered as the newest values", s.Values["FriendlyName1"], s.At)
	}
}

// Why: a page opened after a save (the result page's "Read the settings again") must show the device as
// it is now. Joining a read that started before the save would hand it the old values as fresh ones. It
// gets a read of its own, sent after the old one ends, so the device still answers one read at a time.
func TestAReadAfterAWriteDoesNotJoinTheReadFromBefore(t *testing.T) {
	c := newWriteRace()
	m := New(Options{Devices: fakeDevices{}, Client: c})
	before := startRead(m)
	waitFor(t, c.held, "the read to be under way")
	if _, err := m.Apply(context.Background(), "u", 1, map[string]string{"FriendlyName1": "Lamp"}); err != nil {
		t.Fatal(err)
	}
	after := startRead(m)
	time.Sleep(100 * time.Millisecond) // the second reader arrives while the first read is still under way
	close(c.gate)
	waitFor(t, before, "the read that started before the write")
	if v := waitFor(t, after, "the read asked for after the write"); v["FriendlyName1"] != "Lamp" {
		t.Errorf("a read asked for after the write got FriendlyName1 %q, want the written \"Lamp\"", v["FriendlyName1"])
	}
	if s, ok := m.Last(1); !ok || s.Values["FriendlyName1"] != "Lamp" {
		t.Errorf("Last = %q, %v; want the values read after the write", s.Values["FriendlyName1"], ok)
	}
	plain := &fakeClient{}
	if _, err := New(Options{Devices: fakeDevices{}, Client: plain}).Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	read := plain.sent // what one complete Read sends, in order
	want := append(append([]string{read[0], "FriendlyName1 Lamp"}, read[1:]...), read...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.sent, want) {
		t.Errorf("the device got\n%q\nwant the old read to its end, then the new one:\n%q", c.sent, want)
	}
}

// heldDevices holds the first GetDevice after it is armed (the start of a read) until hold is closed,
// and counts the armed calls.
type heldDevices struct {
	fakeDevices
	armed         atomic.Bool
	calls         atomic.Int32
	once          sync.Once
	reached, hold chan struct{}
}

func (h *heldDevices) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	if h.armed.Load() {
		h.calls.Add(1)
		h.once.Do(func() { close(h.reached); <-h.hold })
	}
	return h.fakeDevices.GetDevice(ctx, id)
}

// Why: after a write the new read is the one every later reader shares. The read from before the write
// ending must not drop it from the books, or the next open would start yet another round on the device.
func TestReadersAfterAWriteShareOneNewRead(t *testing.T) {
	c := newWriteRace()
	d := &heldDevices{reached: make(chan struct{}), hold: make(chan struct{})}
	m := New(Options{Devices: d, Client: c})
	before := startRead(m)
	waitFor(t, c.held, "the read to be under way")
	if _, err := m.Apply(context.Background(), "u", 1, map[string]string{"FriendlyName1": "Lamp"}); err != nil {
		t.Fatal(err)
	}
	d.armed.Store(true)
	after := startRead(m)
	time.Sleep(100 * time.Millisecond) // it is waiting for the old read when that one ends
	close(c.gate)
	waitFor(t, before, "the read from before the write")
	waitFor(t, d.reached, "the new read to start")
	third := startRead(m)
	time.Sleep(100 * time.Millisecond) // the third reader arrives while the new read is under way
	close(d.hold)
	waitFor(t, after, "the second reader")
	waitFor(t, third, "the third reader")
	if n := d.calls.Load(); n != 1 {
		t.Errorf("%d reads started after the write, want 1 that both readers share", n)
	}
}

// Why (#662): the console and Make compliant write device settings without Apply. They tell the
// service with Forget; a read that started before such a write must not be remembered as current,
// the same as for Apply.
func TestAReadUnderWayDuringAnotherWritersWriteIsNotRemembered(t *testing.T) {
	c := newWriteRace()
	m := New(Options{Devices: fakeDevices{}, Client: c})
	before := startRead(m)
	waitFor(t, c.held, "the read to be under way")
	c.mu.Lock()
	c.replies["FriendlyName1"] = `{"FriendlyName1":"Lamp"}` // the console wrote it
	c.mu.Unlock()
	m.Forget(1)
	close(c.gate)
	if v := waitFor(t, before, "the read that started before the write"); v["FriendlyName1"] != "Old" {
		t.Fatalf("the read under way got %q; the test needs it to have read before the write", v["FriendlyName1"])
	}
	if s, ok := m.Last(1); ok {
		t.Errorf("Last = %q: a read that started before a console write is remembered as the newest values", s.Values["FriendlyName1"])
	}
}

// Why (#662): the detached read outlives the request that started it, but TasmoFleet stopping must end
// it: it kept sending to the device for up to ReadTimeout (60 s) after the stop, ten requests in three
// seconds on a device that answers, while a container is killed a few seconds after the signal.
func TestTheDetachedReadStopsWithTheStopSignal(t *testing.T) {
	c := newWriteRace() // the first query is answered, every later one waits for the gate: a slow device
	stop, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	m := New(Options{Devices: fakeDevices{}, Client: c, Stop: stop, ReadTimeout: time.Minute})
	done := make(chan error, 1)
	go func() {
		_, err := m.Read(context.Background(), 1)
		done <- err
	}()
	waitFor(t, c.held, "the read to be under way")
	stopNow()
	if err := waitFor(t, done, "the read to end with the stop signal"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after the stop signal = %v, want context.Canceled", err)
	}
	if s, ok := m.Last(1); ok {
		t.Errorf("a read that was stopped is remembered: %v", s.Values)
	}
}

// Why (#662): a read that waits for the one from before a write must not outlive the stop signal either,
// even when that older read does not end with it (a request stuck past its cancellation): the wait itself
// ends with the signal.
func TestAReadWaitingForTheOldOneStopsWithTheStopSignal(t *testing.T) {
	c := &deafClient{gate: make(chan struct{}), started: make(chan struct{})}
	defer close(c.gate) // lets the older read end after the test
	stop, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	m := New(Options{Devices: fakeDevices{}, Client: c, Stop: stop})
	go func() { _, _ = m.Read(context.Background(), 1) }() // its result does not matter: it never ends in time
	waitFor(t, c.started, "the first read to be under way")
	m.Forget(1) // the second read must wait for the first
	second := make(chan error, 1)
	go func() { _, err := m.Read(context.Background(), 1); second <- err }()
	waiting := func() bool { // the second flight is registered: it waits on the first
		m.mu.Lock()
		defer m.mu.Unlock()
		f := m.flights[1]
		return f != nil && f.gen == m.gen[1]
	}
	for end := time.Now().Add(5 * time.Second); !waiting(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("timed out waiting for the second read to wait on the first")
		}
	}
	stopNow()
	if err := waitFor(t, second, "the waiting read to end"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiting read = %v, want context.Canceled", err)
	}
}

// deafClient is a device whose request ends only at the gate, whatever its context says.
type deafClient struct {
	fakeClient
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (d *deafClient) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	d.once.Do(func() { close(d.started) })
	<-d.gate
	return d.fakeClient.Command(ctx, t, cmnd)
}

// Why (#662): another device answering at the stored address must not fill this device's last-known
// values. The service asks Verify after the reads; a refusal keeps the values out and reaches the caller.
func TestValuesOfAnotherDeviceAreNotRemembered(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"Topic": `{"Topic":"stranger"}`}}
	errOther := errors.New("another device answers at the stored address")
	var other atomic.Bool
	other.Store(true)
	m := New(Options{Devices: fakeDevices{}, Client: c, Verify: func(context.Context, model.Device) error {
		if other.Load() {
			return errOther
		}
		return nil
	}})
	if _, err := m.Read(context.Background(), 1); !errors.Is(err, errOther) {
		t.Fatalf("Read = %v, want the refusal", err)
	}
	if s, ok := m.Last(1); ok {
		t.Fatalf("values of another device were remembered: %v", s.Values)
	}
	other.Store(false)
	if _, err := m.Read(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if s, ok := m.Last(1); !ok || s.Values["Topic"] != "stranger" {
		t.Fatalf("the stored device's values must be remembered: %v %v", s, ok)
	}
}
