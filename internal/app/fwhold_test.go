package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

type holdClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *holdClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *holdClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHoldGuard() (deviceGuard, *devlock.FirmwareHolds, *holdClock) {
	c := &holdClock{t: time.Date(2026, 10, 31, 18, 21, 0, 0, time.UTC)}
	holds := devlock.NewFirmwareHolds(c.now, firmwareHoldTimeout)
	return deviceGuard{locks: devlock.New(), jobs: fakeActiveJobs{active: map[model.DeviceID]bool{}}, holds: holds}, holds, c
}

// previewOTA answers a preview with an "upgrade" for every request.
type previewOTA struct{ countingOTA }

func (previewOTA) Preview(_ context.Context, reqs []ota.Request) ([]ota.PreviewItem, error) {
	var out []ota.PreviewItem
	for _, r := range reqs {
		out = append(out, ota.PreviewItem{DeviceID: r.DeviceID, Decision: "upgrade", BuildChoice: true})
	}
	return out, nil
}

// Why: two firmware updates at once can brick a device. A typed "Upgrade 1" ends its job when the device
// answers, but the device flashes on for about a minute; a TasmoFleet update started then is the second
// one. The preview must say so, naming the reason, and the submit behind it must not start anything.
func TestUpdateIsRefusedWhileATypedUpgradeStillRuns(t *testing.T) {
	ctx := context.Background()
	g, holds, clk := newHoldGuard()
	up := &previewOTA{}
	lo := lockedOTA{OTA: up, g: g}
	holds.Start(1, 0)
	clk.advance(3 * time.Minute)

	items, err := lo.Preview(ctx, []ota.Request{{DeviceID: 1}, {DeviceID: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Decision != "refuse" || !strings.Contains(items[0].Reason, "3 min ago") || !strings.Contains(items[0].Reason, "wait until it has restarted") || items[0].BuildChoice {
		t.Errorf("held device must be refused with the reason and no build choice: %+v", items[0])
	}
	if items[1].Decision != "upgrade" {
		t.Errorf("the other device is not affected: %+v", items[1])
	}

	_, err = lo.Submit(ctx, "u", []ota.Request{{DeviceID: 1}, {DeviceID: 2}})
	var refused *ota.RefusedError
	if !errors.As(err, &refused) || len(refused.Refused) != 1 || refused.Refused[0].DeviceID != 1 {
		t.Fatalf("submit must be refused for device 1 only, got %v", err)
	}
	if up.submits != 0 {
		t.Fatal("a refused submit reached the engine")
	}
	if _, err := lo.Submit(ctx, "u", []ota.Request{{DeviceID: 2}}); err != nil || up.submits != 1 {
		t.Fatalf("another device is free: %v", err)
	}
}

// Why: a retry starts updates again; a device a typed command is still flashing must not be retried
// either. A device outside the retried set (succeeded) does not matter.
func TestRetryIsRefusedWhileATypedUpgradeStillRuns(t *testing.T) {
	ctx := context.Background()
	g, holds, _ := newHoldGuard()
	g.jobs = fakeActiveJobs{batch: map[model.BatchID][]model.Job{7: {
		{DeviceID: 1, State: model.JobFailed}, {DeviceID: 2, State: model.JobSucceeded},
	}}}
	up := &countingOTA{}
	lo := lockedOTA{OTA: up, g: g}
	holds.Start(2, 0)
	if _, err := lo.SubmitRetry(ctx, "u", 7, nil); err != nil || up.retries != 1 {
		t.Fatalf("device 2 is not retried, so its hold is irrelevant: %v", err)
	}
	holds.Start(1, 0)
	if _, err := lo.SubmitRetry(ctx, "u", 7, nil); !errors.Is(err, ota.ErrRefused) || up.retries != 1 {
		t.Fatalf("retry of a held device must be refused: %v", err)
	}
}

// Why: the hold may only follow a firmware command that may have reached the device, from the console and
// from bulk jobs alike (both send through this service). An unknown outcome (HTTP read timeout, MQTT lost
// reply, unconfirmed publish) counts: holding ten minutes too long is cheap, too short risks a brick. A
// routine command, a refusal and a bare OtaUrl (it only stores the address, no restart follows) do not.
func TestOnlyAFirmwareCommandThatMayHaveReachedTheDeviceHoldsIt(t *testing.T) {
	ctx := context.Background()
	_, holds, _ := newHoldGuard()
	httpTimeout := fmt.Errorf("device: no answer from 192.0.2.7: %w", tasmota.ErrTimeout)
	mqttLost := fmt.Errorf("mqtt: the device went offline before it answered Upgrade (outcome unknown): %w", tasmota.ErrTimeout)
	cases := []struct {
		id   model.DeviceID
		cmd  string
		err  error
		want bool
	}{
		{1, "Upgrade 1", nil, true},
		{2, "Status 2", nil, false},
		{3, "Upgrade 1", cmdpolicy.ErrRole, false},
		{4, "Backlog OtaUrl http://x/y.bin; Upgrade 1", nil, true},
		{5, "Upgrade 1", devicecmd.ErrUnconfirmed, true},
		{6, "Upgrade 1", httpTimeout, true},
		{7, "Upgrade 1", mqttLost, true},
		{8, "Upgrade 1", store.ErrNotFound, false},
		{9, "Upgrade 1", devlock.BusyError{Holder: "restore"}, false},
		{10, "Upgrade 1", cmdpolicy.ErrUpdateJob, false},
		{11, "OtaUrl http://x/y.bin", nil, false},
		{12, "Backlog OtaUrl http://x/y.bin", nil, false},
		{13, "Upgrade 1", errors.New("anything else is an unknown outcome"), true},
	}
	for _, c := range cases {
		h := holdingCmd{Service: errCmd{err: c.err}, holds: holds, uptime: func(model.DeviceID) int64 { return 0 }}
		_, _ = h.Run(ctx, model.Principal{Role: model.RoleAdmin}, c.id, c.cmd) // the error is the fake's, the test looks at the hold
		if _, got := holds.Holding(c.id); got != c.want {
			t.Errorf("%q with %v: held = %v, want %v", c.cmd, c.err, got, c.want)
		}
	}
}

// Why: a console command records its hold before it lets go of the device lock, so a TasmoFleet update that
// takes the lock afterwards must see it. Checked before the lock, the update could slip in between.
func TestHoldRecordedUnderTheConsoleLockIsSeenBySubmit(t *testing.T) {
	ctx := context.Background()
	g, holds, _ := newHoldGuard()
	lc := lockedDeviceCmd{DeviceCmd: holdingCmd{Service: errCmd{}, holds: holds}, g: g}
	lo := lockedOTA{OTA: &countingOTA{}, g: g}
	if _, err := lc.Run(ctx, model.Principal{Role: model.RoleAdmin}, 1, "Upgrade 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := lo.Submit(ctx, "u", []ota.Request{{DeviceID: 1}}); !errors.Is(err, ota.ErrRefused) {
		t.Fatalf("submit after the console command: %v", err)
	}
	if _, _, ok := g.locks.TryAcquire(1, "x"); !ok {
		t.Fatal("the refused submit must release its lock")
	}
}

// Why: the restart is what ends the hold. The tracker's health events on the hub must reach the holds, so a
// device that went offline and came back is free again at once instead of after ten minutes.
func TestARestartSeenInHealthEventsEndsTheHold(t *testing.T) {
	_, holds, _ := newHoldGuard()
	hub := events.NewHub(16)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { observeFirmwareHolds(ctx, hub, holds, nil); close(done) }()
	defer func() { stop(); <-done }()

	holds.Start(1, 0)
	deadline := time.Now().Add(5 * time.Second)
	publish := func(h model.Health) { hub.Publish(events.Event{Topic: events.TopicHealth, Payload: h}) }
	for { // the subscription starts in the goroutine: publish until the first event is seen to arrive
		publish(model.Health{DeviceID: 1, LWT: "Offline"})
		time.Sleep(10 * time.Millisecond)
		publish(model.Health{DeviceID: 1, LWT: "Online"})
		time.Sleep(10 * time.Millisecond)
		if _, ok := holds.Holding(1); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an LWT Offline then Online on the hub must end the hold")
		}
	}
}

type errCmd struct {
	devicecmd.Service
	err error
}

func (e errCmd) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	return nil, e.err
}

// Why: the observer must hand the tracker's "minimal firmware" state to the holds, or a two-stage update is
// released at its first restart.
func TestObserverKeepsTheHoldWhileTheDeviceRunsMinimalFirmware(t *testing.T) {
	_, holds, _ := newHoldGuard()
	hub := events.NewHub(16)
	var minimal atomic.Bool
	minimal.Store(true)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		observeFirmwareHolds(ctx, hub, holds, func(model.DeviceID) bool { return minimal.Load() })
		close(done)
	}()
	defer func() { stop(); <-done }()
	holds.Start(1, 0)
	publish := func(lwt string) {
		hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, LWT: lwt}})
	}
	for i := 0; i < 20; i++ { // the subscription starts in the goroutine
		publish("Offline")
		publish("Online")
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := holds.Holding(1); !ok {
		t.Fatal("a restart into minimal firmware must not release the hold")
	}
	minimal.Store(false)
	for i := 0; i < 100; i++ {
		publish("Online")
		time.Sleep(10 * time.Millisecond)
		if _, ok := holds.Holding(1); !ok {
			return
		}
	}
	t.Fatal("the real firmware back after an Offline must release the hold")
}
