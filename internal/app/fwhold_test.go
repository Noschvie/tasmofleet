package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
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

// Why: the hold may only follow a firmware command that reached the device, from the console and from bulk
// jobs alike (both send through this service); a routine command or a refused one must not block updates.
func TestOnlyAFirmwareCommandThatWasSentHoldsTheDevice(t *testing.T) {
	ctx := context.Background()
	_, holds, _ := newHoldGuard()
	run := func(id model.DeviceID, cmd string, err error) {
		c := holdingCmd{Service: errCmd{err: err}, holds: holds, uptime: func(model.DeviceID) int64 { return 0 }}
		_, _ = c.Run(ctx, model.Principal{Role: model.RoleAdmin}, id, cmd) // the error is the fake's, the test looks at the hold
	}
	run(1, "Upgrade 1", nil)
	run(2, "Status 2", nil)
	run(3, "Upgrade 1", errors.New("refused before the send"))
	run(4, "Backlog OtaUrl http://x/y.bin; Upgrade 1", nil)
	run(5, "Upgrade 1", devicecmd.ErrUnconfirmed)
	want := map[model.DeviceID]bool{1: true, 2: false, 3: false, 4: true, 5: true}
	for id, w := range want {
		if _, got := holds.Holding(id); got != w {
			t.Errorf("device %d held = %v, want %v", id, got, w)
		}
	}
}

// Why: the restart is what ends the hold. The tracker's health events on the hub must reach the holds, so a
// device that went offline and came back is free again at once instead of after ten minutes.
func TestARestartSeenInHealthEventsEndsTheHold(t *testing.T) {
	_, holds, _ := newHoldGuard()
	hub := events.NewHub(16)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { observeFirmwareHolds(ctx, hub, holds); close(done) }()
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
