package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// runStore is what a scheduled run reads and writes (*store.SQLite).
type runStore interface {
	DevicesSeen(ctx context.Context) (map[model.DeviceID]store.DeviceSeen, error)
	SaveBackupRun(ctx context.Context, r model.BackupRun) (int64, error)
}

// scheduledBackup is one scheduled run over the fleet (BACKUP_INTERVAL, #409). It decides per
// device whether to try it: a device that cannot answer (never heard, offline) or that somebody
// else is working on is skipped, not failed, so only real failures colour the bar or notify.
type scheduledBackup struct {
	log     *slog.Logger
	run     *scheduledRun
	devices func(ctx context.Context) ([]model.Device, error)
	health  func() map[model.DeviceID]model.Health
	store   runStore
	// enter takes the device guard (lock and running transactions); an error means the device is busy.
	enter  func(ctx context.Context, id model.DeviceID, owner string) (release func(), err error)
	backup func(ctx context.Context, id model.DeviceID) error
	// notifyFailed tells the person about the failed devices of a run: tried is how many devices
	// were tried, failed their names.
	notifyFailed func(tried int, failed []string, at time.Time)
	now          func() time.Time
	gap          time.Duration // pause after a device that was tried: ESP8266 web servers are slow to recover
}

const (
	skipNever = "never answered"
	skipBusy  = "another operation was running"
)

// backupSkipReason says why a run leaves a device out, or "" when it tries it. p is what the
// database remembers of the device (zero for a device that never answered); seenKnown is false when
// it could not be read, and "never answered" is then not claimed. The tracker's verdict is used when
// it has one; right after a start it has none ("unknown" for every device until it is heard), and
// the persisted state decides then: a device that was offline before the start and has not been
// heard since is skipped, anything else is tried. Without that, every restart would count the
// offline devices as failed.
func backupSkipReason(h model.Health, p store.DeviceSeen, seenKnown bool, now time.Time) string {
	seen := p.At
	if h.LastSeen.After(seen) {
		seen = h.LastSeen
	}
	if seenKnown && seen.IsZero() {
		return skipNever
	}
	state, since := h.State, h.Since
	if state == "" || state == model.HealthUnknown {
		state, since = p.State, p.At // no verdict yet: what was stored
	}
	if state != model.HealthOffline {
		return ""
	}
	if since.IsZero() {
		since = seen
	}
	if since.IsZero() {
		return "offline"
	}
	return "offline since " + backupWhen(since, now)
}

// backupWhen words a time that can be days old: day and month, the year only when it is another one.
func backupWhen(t, now time.Time) string {
	t = t.Local()
	if t.Year() != now.Local().Year() {
		return t.Format("2 Jan 2006 15:04")
	}
	return t.Format("2 Jan 15:04")
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
	b.run.Start(len(devs))
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
	add := func(d model.Device, out model.BackupOutcome, reason string) {
		rec.Items = append(rec.Items, model.BackupRunItem{DeviceID: d.ID, DeviceName: d.Name, Outcome: out, Reason: reason, At: b.now()})
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
		if why := backupSkipReason(health[d.ID], seen[d.ID], seenKnown, b.now()); why != "" {
			add(d, model.BackupOutcomeSkipped, why)
			continue
		}
		release, err := b.enter(ctx, d.ID, "backup")
		if err != nil { // the lock is held or a transaction is running; a failed check is treated the same: not now
			if !errors.Is(err, devlock.ErrBusy) {
				b.log.Warn("scheduled backup: checking whether the device is busy", "device", d.Name, "err", err)
			}
			add(d, model.BackupOutcomeSkipped, skipBusy)
			continue
		}
		err = b.backup(ctx, d.ID)
		release()
		switch {
		case err == nil:
			add(d, model.BackupOutcomeOK, "")
		case ctx.Err() != nil:
			return // shutdown cut this device short: not a failure of the device
		case errors.Is(err, store.ErrNotFound):
			b.run.Gone() // deleted before its turn: no item, no failure
			continue
		default:
			b.log.Warn("scheduled backup failed", "device", d.Name, "err", err)
			add(d, model.BackupOutcomeFailed, backup.AttemptReason(err))
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
