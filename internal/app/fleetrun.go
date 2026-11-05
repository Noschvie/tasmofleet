package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// runStore is what a scheduled run reads and writes (*store.SQLite).
type runStore interface {
	DevicesSeen(ctx context.Context) (map[model.DeviceID]time.Time, error)
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
	locks   *devlock.Locks
	backup  func(ctx context.Context, id model.DeviceID) error
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

// backupSkipReason says why a run leaves a device out, or "" when it tries it. seen is the
// persisted last_seen_at (zero = never heard); seenKnown is false when it could not be read, and
// "never answered" is then not claimed. The tracker's verdict alone is not enough: right after a
// start it says "unknown" for every device until it is heard, and a run then must not skip the fleet.
func backupSkipReason(h model.Health, seen time.Time, seenKnown bool, now time.Time) string {
	if h.LastSeen.After(seen) {
		seen = h.LastSeen
	}
	switch {
	case seenKnown && seen.IsZero():
		return skipNever
	case h.State == model.HealthOffline:
		since := h.Since
		if since.IsZero() {
			since = seen
		}
		if since.IsZero() {
			return "offline"
		}
		return "offline since " + backupWhen(since, now)
	}
	return ""
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
		release, _, ok := b.locks.TryAcquire(d.ID, "backup")
		if !ok {
			add(d, model.BackupOutcomeSkipped, skipBusy)
			continue
		}
		err := b.backup(ctx, d.ID)
		release()
		switch {
		case err == nil:
			add(d, model.BackupOutcomeOK, "")
		case ctx.Err() != nil:
			return // shutdown cut this device short: not a failure of the device
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
