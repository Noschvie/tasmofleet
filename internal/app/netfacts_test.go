package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: devices created by an import or "Add device" carry no network facts (first contact does
// not wait for them). The background pass must read exactly the devices that are up and were
// never read, leave out minimal firmware (it answers neither command) and not hammer a device
// whose read failed.
func TestNetFactsReaderPassReadsOnlineUnreadDevices(t *testing.T) {
	read := model.NetFacts{ReadAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	devs := []model.Device{
		{ID: 1},            // online, never read: read
		{ID: 2, Net: read}, // already read
		{ID: 3},            // offline
		{ID: 4, Identity: model.Identity{Variant: "minimal"}},
		{ID: 5}, // online, the read fails
	}
	var got []model.DeviceID
	r := newNetFactsReader(
		func(context.Context) ([]model.Device, error) { return devs, nil },
		func(id model.DeviceID) bool { return id != 3 },
		func(_ context.Context, id model.DeviceID) error {
			got = append(got, id)
			if id == 5 {
				return errors.New("timeout")
			}
			return nil
		}, slog.New(slog.DiscardHandler))
	r.pass(context.Background())
	if !slices.Equal(got, []model.DeviceID{1, 5}) {
		t.Fatalf("first pass read %v, want [1 5]", got)
	}
	got = nil
	devs[0].Net = read // device 1 is stored now
	r.pass(context.Background())
	if len(got) != 0 {
		t.Fatalf("second pass read %v: device 5 failed a moment ago and must wait for its retry", got)
	}
}

// Why: a person's refresh shares the device topic with these reads, one command at a time, so every
// background read a refresh queued makes the next refresh wait one identity window (1.5 s). Refreshes
// that arrive while the reader is busy (the startup pass) must therefore leave ONE queued read per
// device, not one per refresh (#1015: the fourth "Refresh identity" of a spec took over 5 s).
func TestNetFactsReaderReadsAQueuedDeviceOnce(t *testing.T) {
	var got []model.DeviceID
	r := newNetFactsReader(
		func(context.Context) ([]model.Device, error) { return nil, nil },
		func(model.DeviceID) bool { return true },
		func(_ context.Context, id model.DeviceID) error { got = append(got, id); return nil },
		slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	for _, id := range []model.DeviceID{1, 1, 1, 2} {
		r.later(id)
	}
	if n := len(r.kick); n != 2 {
		t.Fatalf("%d reads queued, want 2: three refreshes of device 1 queued three reads", n)
	}
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for len(r.kick) > 0 {
		select {
		case <-deadline:
			t.Fatal("the queue was not read")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if !slices.Equal(got, []model.DeviceID{1, 2}) {
		t.Fatalf("read %v, want [1 2]: three refreshes of device 1 queued three reads", got)
	}
}

// Why: the mark that drops a duplicate kick ends when the read starts. A refresh that arrives while
// the device is being read has to queue a new read: the read in flight may have asked before that
// refresh stored its facts.
func TestNetFactsReaderQueuesAgainWhileTheDeviceIsRead(t *testing.T) {
	reading := make(chan model.DeviceID)
	release := make(chan struct{})
	r := newNetFactsReader(
		func(context.Context) ([]model.Device, error) { return nil, nil },
		func(model.DeviceID) bool { return true },
		func(ctx context.Context, id model.DeviceID) error {
			select {
			case reading <- id:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	r.later(1)
	<-reading // read 1 runs
	r.later(1)
	r.later(1) // these two are one queued read
	if n := len(r.kick); n != 1 {
		t.Fatalf("%d reads queued during the read, want 1: a refresh during a read needs one more, not one each", n)
	}
	release <- struct{}{}
	<-reading // the queued read of device 1 starts
	if n := len(r.kick); n != 0 {
		t.Fatalf("%d reads still queued, want 0", n)
	}
	cancel()
	<-done
}

// Why: a kick that finds the queue full is dropped, and its mark must go with it; a stale mark would
// make every later kick of that device a no-op for good.
func TestNetFactsReaderDroppedKickLeavesNoMark(t *testing.T) {
	r := newNetFactsReader(
		func(context.Context) ([]model.Device, error) { return nil, nil },
		func(model.DeviceID) bool { return true },
		func(context.Context, model.DeviceID) error { return nil }, slog.New(slog.DiscardHandler))
	for i := 1; i <= netFactsQueue; i++ {
		r.later(model.DeviceID(i))
	}
	over := model.DeviceID(netFactsQueue + 1)
	r.later(over) // queue full: dropped
	<-r.kick      // room again
	r.later(over)
	if len(r.kick) != netFactsQueue {
		t.Fatalf("queue holds %d, want %d: the dropped kick left a mark that swallowed the next one", len(r.kick), netFactsQueue)
	}
}
