package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// runStore is what a scheduled run reads and writes (*store.SQLite).
type runStore interface {
	DevicesSeen(ctx context.Context) (map[model.DeviceID]store.DeviceSeen, error)
	SaveBackupRun(ctx context.Context, r model.BackupRun) (int64, error)
}

// scheduledBackup is one scheduled run over the fleet (BACKUP_DAYS and BACKUP_TIME, #409, #601). It decides per
// device whether to try it: a device that cannot answer (never heard, offline) or that somebody
// else is working on is skipped, not failed, so only real failures colour the bar or notify.
type scheduledBackup struct {
	log     *slog.Logger
	run     *scheduledRun
	devices func(ctx context.Context) ([]model.Device, error)
	health  func() map[model.DeviceID]model.Health
	store   runStore
	// enter is backupGuard: the device lock, a running transaction, a firmware hold; an error means busy.
	enter  func(ctx context.Context, id model.DeviceID, owner string) (release func(), err error)
	backup func(ctx context.Context, id model.DeviceID) error
	// notifyFailed tells the person about the failed devices of a run: tried is how many devices
	// were tried, failed their names.
	notifyFailed func(tried int, failed []string, at time.Time)
	// apiOffSkip says "HTTP API off" for an online device whose HTTP API is off and which no other path can back up
	// (apiOffSkip in the runtime); nil or "" tries the device.
	apiOffSkip func(ctx context.Context, id model.DeviceID, h model.Health) string
	now        func() time.Time
	gap        time.Duration // pause after a device that was tried: ESP8266 web servers are slow to recover
}

const (
	skipOffline = "offline"
	skipNever   = "never answered"
	skipBusy    = "another operation was running"
	// skipAPIOff: online, but its HTTP API is off (SetOption128 0) and MQTT cannot carry a backup, so a run could
	// only fail (#447). Skipped like an offline device: no failure, no notification.
	skipAPIOff = tasmota.APIOffName
)

// backupGuard is what a scheduled backup must not run into: the device guard (its lock and a running
// transaction) and a firmware update typed in the console, sent as a bulk command or found running by an
// update job, which keeps the device flashing after its job ended (#347). The hold is checked with the
// lock held, as lockedOTA.Submit does. What uses it is started by TasmoFleet itself (a scheduled backup, the
// background read, the HTTP probe a device page asks for while it renders): it never waits for the background
// read (tryOnly, #670).
func backupGuard(g deviceGuard) func(ctx context.Context, id model.DeviceID, owner string) (func(), error) {
	g.tryOnly = true
	return func(ctx context.Context, id model.DeviceID, owner string) (func(), error) {
		release, err := g.enter(ctx, id, owner)
		if err != nil {
			return nil, err
		}
		if g.holds != nil {
			if _, held := g.holds.Holding(id); held {
				release()
				return nil, devlock.BusyError{Holder: "firmware"}
			}
		}
		return release, nil
	}
}

// backupSkipReason says why a run leaves a device out, or "" when it tries it. p is what the
// database remembers of the device (zero for a device that never answered); seenKnown is false when
// it could not be read, and "never answered" is then not claimed. The tracker's verdict is used when
// it has one; right after a start it has none ("unknown" for every device until it is heard), and
// the persisted state decides then: a device that was offline before the start and has not been
// heard since is skipped, anything else is tried. Without that, every restart would count the
// offline devices as failed.
func backupSkipReason(h model.Health, p store.DeviceSeen, seenKnown bool) (reason string, since time.Time) {
	seen := p.At
	if h.LastSeen.After(seen) {
		seen = h.LastSeen
	}
	if seenKnown && seen.IsZero() {
		return skipNever, time.Time{}
	}
	state, since := h.State, h.Since
	if state == "" || state == model.HealthUnknown {
		state, since = p.State, p.At // no verdict yet: what was stored
	}
	if state != model.HealthOffline {
		return "", time.Time{}
	}
	if since.IsZero() {
		since = seen
	}
	return skipOffline, since // the pages word the time themselves
}

// Run backs up every device one after the other and records the run.
func (b *scheduledBackup) Run(ctx context.Context) {
	devs, err := b.devices(ctx)
	if err != nil {
		b.log.Warn("scheduled backup: list devices", "err", err)
		return
	}
	seen, err := b.store.DevicesSeen(ctx)
	seenKnown := err == nil
	if err != nil {
		b.log.Warn("scheduled backup: read when devices were last seen", "err", err)
	}
	rec := model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: b.now()}
	b.run.Start(len(devs), rec.StartedAt)
	completed := false
	defer func() {
		rec.Completed, rec.FinishedAt = completed, b.now()
		// A shutdown cancels ctx; the record is still written, with a limit of its own.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := b.store.SaveBackupRun(sctx, rec); err != nil {
			b.log.Warn("scheduled backup: storing the run", "err", err)
		}
		b.run.Finish(completed)
	}()
	var failed []string
	add := func(d model.Device, out model.BackupOutcome, reason string, since time.Time) {
		rec.Items = append(rec.Items, model.BackupRunItem{DeviceID: d.ID, DeviceName: d.Name, Outcome: out, Reason: reason, Since: since, At: b.now()})
		switch out {
		case model.BackupOutcomeOK:
			rec.OK++
			rec.Tried++
		case model.BackupOutcomeFailed:
			rec.Failed++
			rec.Tried++
			failed = append(failed, d.Name)
		case model.BackupOutcomeSkipped:
			rec.Skipped++
		}
		b.run.Step(out)
	}
	health := b.health()
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		if why, since := backupSkipReason(health[d.ID], seen[d.ID], seenKnown); why != "" {
			add(d, model.BackupOutcomeSkipped, why, since)
			continue
		}
		if b.apiOffSkip != nil {
			if why := b.apiOffSkip(ctx, d.ID, health[d.ID]); why != "" {
				add(d, model.BackupOutcomeSkipped, why, health[d.ID].HTTPRefusedSince)
				continue
			}
		}
		release, err := b.enter(ctx, d.ID, "backup")
		if err != nil { // the lock is held or a transaction is running; a failed check is treated the same: not now
			if !errors.Is(err, devlock.ErrBusy) {
				b.log.Warn("scheduled backup: checking whether the device is busy", "device", d.Name, "err", err)
			}
			add(d, model.BackupOutcomeSkipped, skipBusy, time.Time{})
			continue
		}
		err = b.backup(ctx, d.ID)
		release()
		switch {
		case err == nil:
			add(d, model.BackupOutcomeOK, "", time.Time{})
		case ctx.Err() != nil:
			return // shutdown cut this device short: not a failure of the device
		case errors.Is(err, store.ErrNotFound):
			b.run.Gone() // deleted before its turn: no item, no failure
			continue
		default:
			b.log.Warn("scheduled backup failed", "device", d.Name, "err", err)
			add(d, model.BackupOutcomeFailed, backup.AttemptReason(err), time.Time{})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.gap):
		}
	}
	completed = true
	if len(failed) > 0 && b.notifyFailed != nil { // one message per run, listing the failed devices
		b.notifyFailed(rec.Tried, failed, b.now())
	}
}
