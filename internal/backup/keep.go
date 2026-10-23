package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Prunable returns the files a retention pass at now removes from list (the
// backups of one device, newest first): every run beyond the newest keep runs
// that is not younger than maxAge (0 = no age rule). Kept runs are never
// returned and do not count against keep. A run is the set of files sharing
// one TakenAt; it is kept when any of its files is. This is the one rule the
// pruning applies and the "Pruned at next run" label shows, so the label
// cannot disagree with what happens.
func Prunable(list []model.Backup, keep int, maxAge time.Duration, now time.Time) []model.Backup {
	kept := map[time.Time]bool{}
	for _, b := range list {
		if b.Kept {
			kept[b.TakenAt] = true
		}
	}
	var out []model.Backup
	runs := 0
	var last time.Time
	first := true
	for _, b := range list {
		if kept[b.TakenAt] {
			continue
		}
		if first || !b.TakenAt.Equal(last) {
			runs++
			last, first = b.TakenAt, false
		}
		if runs <= keep || (maxAge > 0 && now.Sub(b.TakenAt) < maxAge) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// PrunedAtNextRun returns the files the pruning after the next backup removes
// if that backup is taken at now: Prunable on list plus one new run. Pruning
// runs only after a successful backup, so this is what "Pruned at next run"
// means. Later backups move the age limit further; the label is for the next
// one.
func PrunedAtNextRun(list []model.Backup, keep int, maxAge time.Duration, now time.Time) []model.Backup {
	next := append([]model.Backup{{ID: -1, TakenAt: now}}, list...)
	var out []model.Backup
	for _, b := range Prunable(next, keep, maxAge, now) {
		if b.ID != -1 {
			out = append(out, b)
		}
	}
	return out
}

// Retention is the pruning rule in force.
type Retention struct {
	Keep   int           // newest runs kept per device, kept runs not counted
	MaxAge time.Duration // runs younger than this are kept; 0 = no age rule
}

// Retention returns the rule the next pruning applies.
func (m *Manager) Retention() Retention {
	r := m.ret.Load()
	return Retention{Keep: r.keep, MaxAge: r.maxAge}
}

// PrunedAtNextRun lists the device's backups the pruning after its next
// backup removes (see the package function).
func (m *Manager) PrunedAtNextRun(ctx context.Context, device model.DeviceID) ([]model.Backup, error) {
	list, err := m.o.Store.ListBackups(ctx, device)
	if err != nil {
		return nil, err
	}
	r := m.Retention()
	return PrunedAtNextRun(list, r.Keep, r.MaxAge, m.o.Clock.Now()), nil
}

// ownerFiles lists every backup of b's owner: its device, or for a backup
// whose device was deleted the other orphans of the same device name and MAC.
func (m *Manager) ownerFiles(ctx context.Context, b model.Backup) ([]model.Backup, error) {
	if b.DeviceID != 0 {
		return m.o.Store.ListBackups(ctx, b.DeviceID)
	}
	all, err := m.o.Store.ListOrphanBackups(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Backup
	for _, o := range all {
		if o.DeviceName == b.DeviceName && o.DeviceMAC == b.DeviceMAC {
			out = append(out, o)
		}
	}
	return out, nil
}

// SetKept keeps (pins) or releases the run b belongs to: all its files. A
// kept run is never pruned; a released one counts against the retention from
// the next pruning on. Audited as backup.keep / backup.release.
func (m *Manager) SetKept(ctx context.Context, actor string, id model.BackupID, kept bool) (model.Backup, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return model.Backup{}, err
	}
	l := m.deviceLock(b.DeviceID)
	l.Lock()
	defer l.Unlock()
	all, err := m.ownerFiles(ctx, b)
	if err != nil {
		return model.Backup{}, err
	}
	var ids []model.BackupID
	for _, f := range runsOf(all, b.TakenAt) {
		ids = append(ids, f.ID)
	}
	err = m.o.Store.SetBackupsKept(ctx, ids, kept)
	action := "backup.keep"
	if !kept {
		action = "backup.release"
	}
	m.record(ctx, actor, action, fmt.Sprintf("backup:%d", id), map[string]any{
		"device": b.DeviceID, "taken": b.TakenAt.UTC().Format(time.RFC3339), "files": len(ids)}, err)
	if err != nil {
		return model.Backup{}, err
	}
	b.Kept = kept
	return b, nil
}

// planRuns resolves the selected backups of one owner to whole runs. Every id
// must belong to device (0: to the same deleted device); otherwise
// ErrForeignBackup, so a form cannot reach another device's backups.
func (m *Manager) planRuns(ctx context.Context, device model.DeviceID, ids []model.BackupID) (Deletion, []model.Backup, error) {
	if len(ids) == 0 {
		return Deletion{}, nil, ErrNothingSelected
	}
	var owner model.Backup
	taken := map[time.Time]bool{}
	for i, id := range ids {
		b, err := m.o.Store.GetBackup(ctx, id)
		if err != nil {
			return Deletion{}, nil, err
		}
		if b.DeviceID != device || (i > 0 && device == 0 && (b.DeviceName != owner.DeviceName || b.DeviceMAC != owner.DeviceMAC)) {
			return Deletion{}, nil, ErrForeignBackup
		}
		if i == 0 {
			owner = b
		}
		taken[b.TakenAt] = true
	}
	all, err := m.ownerFiles(ctx, owner)
	if err != nil {
		return Deletion{}, nil, err
	}
	var files []model.Backup
	for _, b := range all {
		if taken[b.TakenAt] {
			files = append(files, b)
		}
	}
	newest := NewestRun(all)
	d := Deletion{Device: device, Runs: len(taken), Files: len(files), Newest: taken[newest], Remaining: countRuns(all) - len(taken)}
	return d, files, nil
}

// PlanDeleteRuns says what DeleteRuns would remove: the count for the one
// confirmation and whether the newest backup is among them.
func (m *Manager) PlanDeleteRuns(ctx context.Context, device model.DeviceID, ids []model.BackupID) (Deletion, error) {
	d, _, err := m.planRuns(ctx, device, ids)
	return d, err
}

// DeleteRuns removes the runs the selected backups belong to ("Delete
// selected"), on disk and in the index, after one confirmation that named the
// count and whether the newest goes. Kept runs are deleted too: the selection
// is explicit.
func (m *Manager) DeleteRuns(ctx context.Context, actor string, device model.DeviceID, ids []model.BackupID) (Deletion, error) {
	l := m.deviceLock(device)
	l.Lock()
	defer l.Unlock()
	d, files, err := m.planRuns(ctx, device, ids)
	if err != nil {
		return Deletion{}, err
	}
	err = m.removeFiles(ctx, files)
	m.auditDelete(ctx, actor, "backup.delete", fmt.Sprintf("device:%d", device), d, time.Time{}, time.Time{}, err)
	if err != nil {
		return Deletion{}, err
	}
	return d, nil
}

// DeleteAll removes every backup of the device, kept ones included: "Delete
// the backups too" when the device is deleted. The caller deletes the device
// afterwards; a backup that slips in between stays as an orphan, listed
// in the settings, never lost silently.
func (m *Manager) DeleteAll(ctx context.Context, actor string, device model.DeviceID) (Deletion, error) {
	l := m.deviceLock(device)
	l.Lock()
	defer l.Unlock()
	all, err := m.o.Store.ListBackups(ctx, device)
	if err != nil {
		return Deletion{}, err
	}
	d := Deletion{Device: device, Runs: countRuns(all), Files: len(all), Newest: len(all) > 0}
	err = m.removeFiles(ctx, all)
	m.auditDelete(ctx, actor, "backup.delete", fmt.Sprintf("device:%d", device), d, time.Time{}, time.Time{}, err)
	if err != nil {
		return Deletion{}, err
	}
	return d, nil
}

// Attempts lists the device's failed backups, newest first (limit <= 0: all).
func (m *Manager) Attempts(ctx context.Context, device model.DeviceID, limit int) ([]model.BackupAttempt, error) {
	return m.o.Store.ListBackupAttempts(ctx, device, limit)
}

// Orphans lists the kept backups of deleted devices, newest first.
func (m *Manager) Orphans(ctx context.Context) ([]model.Backup, error) {
	return m.o.Store.ListOrphanBackups(ctx)
}

func (m *Manager) record(ctx context.Context, actor, action, target string, detail map[string]any, err error) {
	if m.o.Audit == nil {
		return
	}
	e := model.AuditEvent{Actor: actor, Action: action, Target: target, Result: "ok"}
	if b, merr := json.Marshal(detail); merr == nil {
		e.Detail = string(b)
	}
	if err != nil {
		e.Result = "error: " + err.Error()
	}
	m.o.Audit.Record(ctx, e)
}

// ErrNoUsage: the store cannot aggregate the backups (a test fake).
var ErrNoUsage = errors.New("backup: the store has no usage summary")

// Usage is the backup storage per device and of the orphans (Settings ->
// Backups, design v2 N9), from one aggregate query of the store.
func (m *Manager) Usage(ctx context.Context) ([]store.BackupUsage, error) {
	u, ok := m.o.Store.(store.BackupUsageStore)
	if !ok {
		return nil, ErrNoUsage
	}
	return u.BackupUsage(ctx)
}
