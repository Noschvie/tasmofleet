package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

const (
	fwRecheckTick    = time.Minute
	fwRecheckEvery   = time.Hour // a device on limited firmware is looked at again about this often
	fwRecheckTimeout = 30 * time.Second
)

// fwRecheck gives a device on safeboot or minimal firmware its way back (#558). Nothing else
// reads such a device, and the stored identity changes only on a probe, so a device that left
// that firmware outside an update job (Tasmota's own fallback, a manual upload) would stay skipped
// for ever. About once an hour (one device per tick) it runs the ordinary identity refresh, which
// stores the variant it finds: a device back on regular firmware is read again from then on, one
// still on limited firmware costs one identity read per hour. It does not depend on the policy
// read interval, and it never touches a device on regular firmware.
type fwRecheck struct {
	list    func(context.Context) ([]model.Device, error) // with the effective firmware variant
	health  func(model.DeviceID) model.Health
	refresh func(context.Context, model.DeviceID) error // Prober.Refresh with no actor: binds no MAC
	now     func() time.Time
	log     *slog.Logger
	tried   map[model.DeviceID]time.Time
}

func newFWRecheck(list func(context.Context) ([]model.Device, error), health func(model.DeviceID) model.Health,
	refresh func(context.Context, model.DeviceID) error, log *slog.Logger) *fwRecheck {
	return &fwRecheck{list: list, health: health, refresh: refresh, now: time.Now, log: log, tried: map[model.DeviceID]time.Time{}}
}

func (r *fwRecheck) run(ctx context.Context) {
	t := time.NewTicker(fwRecheckTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick refreshes the device on limited firmware that has waited longest (never tried first), if it
// is not offline and was not tried within fwRecheckEvery.
func (r *fwRecheck) tick(ctx context.Context) {
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("firmware recheck: list devices", "err", err)
		return
	}
	now := r.now()
	var pick *model.Device
	seen := make(map[model.DeviceID]bool, len(devs))
	for i := range devs {
		d := &devs[i]
		seen[d.ID] = true
		if !policy.LimitedFirmware(d.Identity.Variant) {
			delete(r.tried, d.ID)
			continue
		}
		if r.health(d.ID).State == model.HealthOffline {
			continue // nothing could answer
		}
		last, was := r.tried[d.ID]
		if was && now.Sub(last) < fwRecheckEvery {
			continue
		}
		if pick == nil || last.Before(r.tried[pick.ID]) {
			pick = d
		}
	}
	for id := range r.tried {
		if !seen[id] {
			delete(r.tried, id) // deleted from the inventory
		}
	}
	if pick == nil {
		return
	}
	r.tried[pick.ID] = now
	rctx, cancel := context.WithTimeout(ctx, fwRecheckTimeout)
	defer cancel()
	if err := r.refresh(rctx, pick.ID); err != nil && ctx.Err() == nil && !errors.As(err, new(*TopicKeptError)) {
		r.log.Debug("firmware recheck: identity not read", "device", pick.ID, "err", err)
	}
}
