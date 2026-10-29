package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// IsReplacementHardware reports whether b is the dump of replacement hardware
// taken by "Replace device" (NoteReplacementHardware). A bulk backup's note
// starts with the user's label, so a label that reads like the marker must
// not hide the dump from Replace.
func IsReplacementHardware(b model.Backup) bool {
	return b.Trigger != model.TriggerBulk && strings.HasPrefix(b.Note, NoteReplacementHardware)
}

// NewestRun returns the TakenAt of the newest run in list (any order); the
// zero time for an empty list. A run is the set of files sharing one TakenAt.
func NewestRun(list []model.Backup) time.Time {
	var newest time.Time
	for _, b := range list {
		if b.TakenAt.After(newest) {
			newest = b.TakenAt
		}
	}
	return newest
}

// IsNewestRun reports whether b belongs to the newest run of list.
func IsNewestRun(list []model.Backup, b model.Backup) bool {
	return b.TakenAt.Equal(NewestRun(list))
}

// Deletion is the outcome (or, from Plan*, the preview) of a delete.
type Deletion struct {
	Device model.DeviceID
	// Runs and Files count what goes (or went): a run is dump, snapshot and
	// decoded twin of one backup.
	Runs, Files int
	// Newest is true when the newest backup of the device is among them.
	Newest bool
	// Remaining is the number of runs the device keeps.
	Remaining int
}

// runsOf returns the files of the run b belongs to, from list.
func runsOf(list []model.Backup, taken time.Time) []model.Backup {
	var out []model.Backup
	for _, b := range list {
		if b.TakenAt.Equal(taken) {
			out = append(out, b)
		}
	}
	return out
}

func countRuns(list []model.Backup) int {
	seen := map[time.Time]bool{}
	for _, b := range list {
		seen[b.TakenAt] = true
	}
	return len(seen)
}

// PlanDelete says what DeleteRun would remove, without removing anything.
func (m *Manager) PlanDelete(ctx context.Context, id model.BackupID) (Deletion, error) {
	d, _, _, err := m.planRun(ctx, id)
	return d, err
}

func (m *Manager) planRun(ctx context.Context, id model.BackupID) (Deletion, []model.Backup, time.Time, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return Deletion{}, nil, time.Time{}, err
	}
	all, err := m.ownerFiles(ctx, b)
	if err != nil {
		return Deletion{}, nil, time.Time{}, err
	}
	files := runsOf(all, b.TakenAt)
	d := Deletion{Device: b.DeviceID, Runs: 1, Files: len(files), Newest: IsNewestRun(all, b), Remaining: countRuns(all) - 1}
	return d, files, b.TakenAt, nil
}

// DeleteRun removes the backup b belongs to: its dump, snapshot and decoded
// twin, on disk and in the index. It serialises with Backup, so it never
// overlaps a backup in progress. The caller has already asked for the
// confirmation; Deletion.Newest tells whether the device's newest backup went.
func (m *Manager) DeleteRun(ctx context.Context, actor string, id model.BackupID) (Deletion, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return Deletion{}, err
	}
	l := m.deviceLock(b.DeviceID)
	l.Lock()
	defer l.Unlock()
	d, files, taken, err := m.planRun(ctx, id)
	if err != nil {
		return Deletion{}, err
	}
	if err := m.removeFiles(ctx, files); err != nil {
		m.auditDelete(ctx, actor, "backup.delete", fmt.Sprintf("backup:%d", id), d, taken, time.Time{}, err)
		return Deletion{}, err
	}
	m.auditDelete(ctx, actor, "backup.delete", fmt.Sprintf("backup:%d", id), d, taken, time.Time{}, nil)
	return d, nil
}

// olderRuns lists the files of the runs of device taken strictly before.
func (m *Manager) olderRuns(ctx context.Context, device model.DeviceID, before time.Time) (Deletion, []model.Backup, error) {
	all, err := m.o.Store.ListBackups(ctx, device)
	if err != nil {
		return Deletion{}, nil, err
	}
	newest := NewestRun(all)
	var files []model.Backup
	runs := map[time.Time]bool{}
	for _, b := range all {
		if b.TakenAt.Before(before) {
			files = append(files, b)
			runs[b.TakenAt] = true
		}
	}
	d := Deletion{Device: device, Runs: len(runs), Files: len(files), Newest: runs[newest], Remaining: countRuns(all) - len(runs)}
	return d, files, nil
}

// PlanDeleteOlderThan says what DeleteOlderThan would remove.
func (m *Manager) PlanDeleteOlderThan(ctx context.Context, device model.DeviceID, before time.Time) (Deletion, error) {
	d, _, err := m.olderRuns(ctx, device, before)
	return d, err
}

// DeleteOlderThan removes every backup of the device taken before the given
// time. Like DeleteRun it is an admin decision made after a confirmation that
// named the count and whether the newest backup is included.
func (m *Manager) DeleteOlderThan(ctx context.Context, actor string, device model.DeviceID, before time.Time) (Deletion, error) {
	l := m.deviceLock(device)
	l.Lock()
	defer l.Unlock()
	d, files, err := m.olderRuns(ctx, device, before)
	if err != nil {
		return Deletion{}, err
	}
	err = m.removeFiles(ctx, files)
	m.auditDelete(ctx, actor, "backup.prune", fmt.Sprintf("device:%d", device), d, time.Time{}, before, err)
	if err != nil {
		return Deletion{}, err
	}
	return d, nil
}

// removeFiles deletes the files and then their index rows. A file that is
// already gone is fine; any other failure stops with the rows still indexed,
// so nothing is left on disk without an index entry.
func (m *Manager) removeFiles(ctx context.Context, files []model.Backup) error {
	var removed []string
	defer func() { m.removeEmptyDirs(removed...) }()
	for _, b := range files {
		full, ok := m.safePath(b.Path)
		if !ok {
			return fmt.Errorf("backup: refusing stored path %q", b.Path)
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: remove %s: %w", b.Kind, err)
		}
		if err := m.o.Store.DeleteBackup(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		removed = append(removed, b.Path)
	}
	return nil
}

func (m *Manager) auditDelete(ctx context.Context, actor, action, target string, d Deletion, taken, before time.Time, err error) {
	if m.o.Audit == nil {
		return
	}
	detail := map[string]any{"device": d.Device, "runs": d.Runs, "files": d.Files, "newest": d.Newest}
	if !taken.IsZero() {
		detail["taken"] = taken.UTC().Format(time.RFC3339)
	}
	if !before.IsZero() {
		detail["before"] = before.UTC().Format(time.RFC3339)
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
