package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/store"
)

const (
	fwRecheckTick    = time.Minute
	fwRecheckEvery   = time.Hour // a device on limited firmware is looked at again about this often
	fwRecheckTimeout = 30 * time.Second
	fwRecheckSoon    = 10 * time.Minute // a new INFO1 naming limited firmware makes a device due once the last try is this old
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
	// busy reports a device an update or a typed firmware command is working on: its firmware changes
	// on purpose then (staged minimal and safeboot images), and a refresh would only get in the way.
	// nil = never busy.
	busy func(context.Context, model.DeviceID) bool
	// sighted is when a live INFO1 last named limited firmware for the device (zero = never); nil = unused.
	sighted func(model.DeviceID) time.Time
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
		if r.busy != nil && r.busy(ctx, d.ID) {
			continue // no try is recorded: it is looked at as soon as the job is over
		}
		last, was := r.tried[d.ID]
		// A device that announces limited firmware again after the last try (it booted into it) is
		// due once that try is fwRecheckSoon old, not only after the hour.
		again := r.sighted != nil && r.sighted(d.ID).After(last) && now.Sub(last) >= fwRecheckSoon
		if was && now.Sub(last) < fwRecheckEvery && !again {
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

// activeJobStates are the states of a job that is working on its device (ota's activeStates).
var activeJobStates = []model.JobState{
	model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL,
	model.JobSafebootSwitch, model.JobSafebootRecover,
	model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobApplying,
}

// startFWRecheck starts the reader that brings devices back from safeboot and minimal firmware (#558).
func (a *runtime) startFWRecheck(ctx context.Context) {
	rc := a.newFWRecheck()
	a.goBG("fw-recheck", func() error { rc.run(ctx); return nil })
}

// newFWRecheck wires the reader to the runtime: devices with their effective firmware, health,
// the ordinary identity refresh, and the checks that keep it away from a device in an update.
func (a *runtime) newFWRecheck() *fwRecheck {
	rc := newFWRecheck(a.fw.list(a.svc.Inventory.List), a.tracker.Get,
		func(ctx context.Context, id model.DeviceID) error { return a.svc.Prober.Refresh(ctx, id, "") }, a.log)
	rc.sighted = a.fw.sightedLimitedAt
	rc.busy = func(ctx context.Context, id model.DeviceID) bool {
		if _, held := a.fwHolds.Holding(id); held {
			return true
		}
		jobs, err := a.svc.Store.ListJobs(ctx, store.JobFilter{DeviceID: id, States: activeJobStates, Limit: 1})
		if err != nil {
			a.log.Warn("firmware recheck: cannot list the jobs of a device, leaving it alone this tick", "device", id, "err", err)
		}
		return err != nil || len(jobs) > 0 // unknown counts as busy: better to wait a tick than to disturb a job
	}
	return rc
}
