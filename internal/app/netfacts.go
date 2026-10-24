package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const (
	netFactsEvery   = 15 * time.Second // how often devices without network facts are looked for
	netFactsTimeout = 30 * time.Second // one device's read
	netFactsRetry   = 10 * time.Minute // a device whose read failed is asked again after this
	netFactsQueue   = 64               // refreshes waiting for their read; more are picked up by the next pass
)

// netFactsReader reads the network facts (Prober.ReadNet) in the background, never as part of
// first contact or a refresh: devices that are online and were never read (import, add), and
// devices whose identity was just refreshed (later). One device at a time, so a device sees at
// most one Status command at a time from here.
type netFactsReader struct {
	list   func(context.Context) ([]model.Device, error)
	online func(model.DeviceID) bool
	read   func(context.Context, model.DeviceID) error
	now    func() time.Time
	every  time.Duration
	log    *slog.Logger

	kick   chan model.DeviceID
	failed map[model.DeviceID]time.Time // next attempt after a failed read
}

func newNetFactsReader(list func(context.Context) ([]model.Device, error), online func(model.DeviceID) bool,
	read func(context.Context, model.DeviceID) error, log *slog.Logger) *netFactsReader {
	return &netFactsReader{list: list, online: online, read: read, now: time.Now, every: netFactsEvery, log: log,
		kick: make(chan model.DeviceID, netFactsQueue), failed: map[model.DeviceID]time.Time{}}
}

// later queues a read of id and returns at once.
func (r *netFactsReader) later(id model.DeviceID) {
	select {
	case r.kick <- id:
	default: // queue full: the device keeps its facts until it is refreshed again
	}
}

func (r *netFactsReader) run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	r.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.kick:
			r.readOne(ctx, id)
		case <-t.C:
			r.pass(ctx)
		}
	}
}

// pass reads every online device that has no network facts yet.
func (r *netFactsReader) pass(ctx context.Context) {
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("network facts: list devices", "err", err)
		return
	}
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		if !d.Net.ReadAt.IsZero() || d.Identity.Variant == "minimal" || !r.online(d.ID) {
			continue
		}
		if next, ok := r.failed[d.ID]; ok && r.now().Before(next) {
			continue
		}
		r.readOne(ctx, d.ID)
	}
}

func (r *netFactsReader) readOne(ctx context.Context, id model.DeviceID) {
	rctx, cancel := context.WithTimeout(ctx, netFactsTimeout)
	defer cancel()
	if err := r.read(rctx, id); err != nil {
		if ctx.Err() == nil {
			r.failed[id] = r.now().Add(netFactsRetry)
			r.log.Debug("network facts: read failed", "device", id, "err", err)
		}
		return
	}
	delete(r.failed, id)
}
