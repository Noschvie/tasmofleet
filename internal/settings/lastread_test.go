package settings

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
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
