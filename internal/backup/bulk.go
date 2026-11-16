package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Bulk actions of Devices -> Backups (#622): delete the backups of deleted devices, delete the
// unreferenced files, and "Prune now" for the fleet. Each one is one action with one audit entry.
// The form posts what the confirmation showed; every function works out again what it may delete
// and removes only the overlap, so a stale page, a forged form or a backup taken in between can
// never delete more than was confirmed, and never something a rule forbids.

// ErrNoRestoreGuard: the store cannot say which backups an unfinished restore uses, so nothing is
// pruned. Failing closed keeps a restore point the guard would have protected (#494).
var ErrNoRestoreGuard = errors.New("backup: cannot tell which backups a restore is using")

// Why a run the retention lists is not removed by Prune now.
const (
	HeldNewest = "newest backup of the device"
	HeldInUse  = "used by a restore"
	heldGone   = "no longer listed"
	heldKept   = "kept"
)

// Held is a run the retention lists for the next pruning that Prune now keeps.
type Held struct {
	TakenAt time.Time
	Why     string
}

// PruneNowSet is what "Prune now" removes from list (the backups of one device): the files
// PrunedAtNextRun lists, minus what must never go in a bulk action. A device must never be left
// without a backup, and PrunedAtNextRun counts the backup that is yet to be taken, so with a
// retention of 1 it lists the current newest run: that run and the device's newest run of its own
// (replacement hardware is counted apart) stay. A run that an unfinished restore or replace reads
// from (inUse holds the source rows) stays too: a failed restore must be retryable (#494). The
// safety backup of a restore is the device's newest run, so the first rule covers it.
func PruneNowSet(list []model.Backup, keep int, maxAge time.Duration, now time.Time, inUse map[model.BackupID]bool) (prune []model.Backup, held []Held) {
	usedRun := map[time.Time]bool{}
	for _, b := range list {
		if inUse[b.ID] {
			usedRun[b.TakenAt] = true
		}
	}
	newest := NewestRun(list)
	own, hasOwn := NewestOwnRun(list)
	seen := map[time.Time]bool{}
	for _, b := range PrunedAtNextRun(list, keep, maxAge, now) {
		why := ""
		switch {
		case b.TakenAt.Equal(newest) || (hasOwn && b.TakenAt.Equal(own)):
			why = HeldNewest
		case usedRun[b.TakenAt]:
			why = HeldInUse
		}
		if why == "" {
			prune = append(prune, b)
			continue
		}
		if !seen[b.TakenAt] {
			seen[b.TakenAt] = true
			held = append(held, Held{TakenAt: b.TakenAt, Why: why})
		}
	}
	return prune, held
}

// activeSources reads the dump rows of the unfinished restores and replaces. It fails when the
// store cannot say (ErrNoRestoreGuard) or the read fails.
func (m *Manager) activeSources(ctx context.Context) (map[model.BackupID]bool, error) {
	s, ok := m.o.Store.(store.ActiveRestoreStore)
	if !ok {
		return nil, ErrNoRestoreGuard
	}
	ids, err := s.ActiveRestoreBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: restores in use: %w", err)
	}
	out := make(map[model.BackupID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// PruneNowPlan is PruneNowSet for one device at the current retention and time: the one rule the
// Storage list, the confirmation and the deletion all use. It reads, deletes nothing and takes no
// lock; PruneNow reads it again under the device's lock.
func (m *Manager) PruneNowPlan(ctx context.Context, device model.DeviceID) (prune []model.Backup, held []Held, err error) {
	list, err := m.o.Store.ListBackups(ctx, device)
	if err != nil {
		return nil, nil, err
	}
	inUse, err := m.activeSources(ctx)
	if err != nil {
		return nil, nil, err
	}
	r := m.Retention()
	prune, held = PruneNowSet(list, r.Keep, r.MaxAge, m.o.Clock.Now(), inUse)
	return prune, held, nil
}

// BulkResult is what a bulk action removed.
type BulkResult struct {
	Devices int   // PruneNow: devices that lost a run
	Runs    int   // runs removed (unreferenced files: 0)
	Files   int   // files removed
	Bytes   int64 // their size
	Left    int   // asked for and kept: a kept run, the newest run, a run a restore uses, something that is gone
}

// bulkRows resolves ids to rows. A row that is gone is counted, not an error: the page may be
// older than the retention or another admin's delete.
func (m *Manager) bulkRows(ctx context.Context, ids []model.BackupID) (rows []model.Backup, gone int, err error) {
	if len(ids) == 0 {
		return nil, 0, ErrNothingSelected
	}
	for _, id := range ids {
		b, err := m.o.Store.GetBackup(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			gone++
		case err != nil:
			return nil, 0, err
		default:
			rows = append(rows, b)
		}
	}
	return rows, gone, nil
}

// removedRun is one run of an audit entry.
type removedRun struct {
	Device string `json:"device,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Taken  string `json:"taken"`
	Files  int    `json:"files"`
}

// leftRun is a confirmed run that was not removed, with the reason.
type leftRun struct {
	Device string `json:"device,omitempty"`
	Taken  string `json:"taken"`
	Why    string `json:"why"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// runsOfFiles lists the runs of files for an audit entry, one per owner and time.
func runsOfFiles(files []model.Backup) []removedRun {
	type key struct {
		name, mac string
		t         time.Time
	}
	idx := map[key]int{}
	var out []removedRun
	for _, b := range files {
		k := key{b.DeviceName, b.DeviceMAC, b.TakenAt}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, removedRun{Device: b.DeviceName, MAC: b.DeviceMAC, Taken: stamp(b.TakenAt)})
		}
		out[i].Files++
	}
	return out
}

func sizeOf(files []model.Backup) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// removeCounted removes files in order and returns the ones that went (a prefix of files).
func (m *Manager) removeCounted(ctx context.Context, files []model.Backup) ([]model.Backup, error) {
	n, err := m.removeFilesN(ctx, files)
	return files[:n], err
}

// PruneNow applies the retention to the devices at once ("Prune now"): it removes the runs the
// confirmation named (ids: the dump row of each run) as far as PruneNowPlan still allows them, one
// device at a time under its lock, and writes one audit entry listing every run it removed and
// every confirmed run it left. A row of a deleted device is refused (ErrForeignBackup). It stops at
// the first file that cannot be removed; what went before is audited with the error.
//
// A restore that starts after the check waits behind the lock of the same device when it takes its
// safety backup, but it may already have chosen its source row before this ran. The window is from
// Backups.Open to CreateRestore (the preflight): a restore inside it has read its dump into memory
// and runs from those bytes. What is left of the race is only that a retry after such a restore
// failed finds its source gone.
func (m *Manager) PruneNow(ctx context.Context, actor string, ids []model.BackupID) (BulkResult, error) {
	rows, gone, err := m.bulkRows(ctx, ids)
	if err != nil {
		return BulkResult{}, err
	}
	confirmed := map[model.DeviceID]map[time.Time]bool{}
	names := map[model.DeviceID]string{}
	var order []model.DeviceID
	for _, b := range rows {
		if b.DeviceID == 0 {
			return BulkResult{}, ErrForeignBackup
		}
		names[b.DeviceID] = b.DeviceName
		if confirmed[b.DeviceID] == nil {
			confirmed[b.DeviceID] = map[time.Time]bool{}
			order = append(order, b.DeviceID)
		}
		confirmed[b.DeviceID][b.TakenAt] = true
	}
	slices.Sort(order)

	res := BulkResult{Left: gone}
	var removed []removedRun
	var left []leftRun
	var failure error
	for _, dev := range order {
		gone, kept, err := m.pruneDevice(ctx, dev, confirmed[dev])
		removed = append(removed, runsOfFiles(gone)...)
		res.Files += len(gone)
		res.Bytes += sizeOf(gone)
		res.Runs += countRuns(gone)
		if len(gone) > 0 {
			res.Devices++
			m.publishDeleted(dev)
		}
		res.Left += len(kept)
		for _, k := range kept {
			left = append(left, leftRun{Device: names[dev], Taken: stamp(k.TakenAt), Why: k.Why})
		}
		if err != nil {
			failure = err
			break
		}
	}
	m.record(ctx, actor, "backup.prune_now", "fleet", map[string]any{
		"devices": res.Devices, "runs": res.Runs, "files": res.Files, "bytes": res.Bytes,
		"removed": removed, "left": left}, failure)
	return res, failure
}

// pruneDevice removes the confirmed runs of one device that PruneNowPlan still lists, under the
// device lock. It returns the files that went (also on an error: the ones before it) and the
// confirmed runs it kept.
func (m *Manager) pruneDevice(ctx context.Context, dev model.DeviceID, confirmed map[time.Time]bool) ([]model.Backup, []Held, error) {
	l := m.deviceLock(dev)
	l.Lock()
	defer l.Unlock()
	plan, held, err := m.PruneNowPlan(ctx, dev) // read under the lock: no backup is half done
	if err != nil {
		return nil, nil, err
	}
	var files []model.Backup
	allowed := map[time.Time]bool{}
	for _, b := range plan {
		if confirmed[b.TakenAt] {
			files = append(files, b)
			allowed[b.TakenAt] = true
		}
	}
	var kept []Held
	for t := range confirmed {
		if allowed[t] {
			continue
		}
		why := heldGone
		for _, h := range held {
			if h.TakenAt.Equal(t) {
				why = h.Why
			}
		}
		kept = append(kept, Held{TakenAt: t, Why: why})
	}
	slices.SortFunc(kept, func(a, b Held) int { return a.TakenAt.Compare(b.TakenAt) })
	gone, err := m.removeCounted(ctx, files)
	return gone, kept, err
}

// planOrphanRuns resolves ids (the dump row of each run) to the files of the runs of deleted
// devices that a bulk delete may remove: whole runs, never a kept one. The runs it leaves are
// listed with the reason; a row that is gone is counted. A row of a current device is refused.
func (m *Manager) planOrphanRuns(ctx context.Context, ids []model.BackupID) (files []model.Backup, left []leftRun, gone int, err error) {
	rows, gone, err := m.bulkRows(ctx, ids)
	if err != nil {
		return nil, nil, 0, err
	}
	type owner struct{ name, mac string }
	type runKey struct {
		o owner
		t time.Time
	}
	picked := map[runKey]bool{}
	for _, b := range rows {
		if b.DeviceID != 0 {
			return nil, nil, 0, ErrForeignBackup
		}
		picked[runKey{owner{b.DeviceName, b.DeviceMAC}, b.TakenAt}] = true
	}
	all, err := m.o.Store.ListOrphanBackups(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	keptRun := map[runKey]bool{} // a run is kept when any of its files is
	for _, b := range all {
		if b.Kept {
			keptRun[runKey{owner{b.DeviceName, b.DeviceMAC}, b.TakenAt}] = true
		}
	}
	taking := map[runKey]bool{}
	for _, b := range all {
		k := runKey{owner{b.DeviceName, b.DeviceMAC}, b.TakenAt}
		if picked[k] && !keptRun[k] {
			files = append(files, b)
			taking[k] = true
		}
	}
	for k := range picked {
		if !taking[k] {
			why := heldGone
			if keptRun[k] {
				why = heldKept
			}
			left = append(left, leftRun{Device: k.o.name, Taken: stamp(k.t), Why: why})
		}
	}
	slices.SortFunc(left, func(a, b leftRun) int { return strings.Compare(a.Device+a.Taken, b.Device+b.Taken) })
	return files, left, gone, nil
}

// PlanOrphanRuns says what DeleteOrphanRuns would remove, for the confirmation page: Devices are
// the deleted devices the runs belong to, Left the runs it would leave (kept, or gone).
func (m *Manager) PlanOrphanRuns(ctx context.Context, ids []model.BackupID) (BulkResult, error) {
	files, left, gone, err := m.planOrphanRuns(ctx, ids)
	if err != nil {
		return BulkResult{}, err
	}
	return orphanResult(files, len(left)+gone), nil
}

func orphanResult(files []model.Backup, left int) BulkResult {
	owners := map[[2]string]bool{}
	for _, b := range files {
		owners[[2]string{b.DeviceName, b.DeviceMAC}] = true
	}
	return BulkResult{Devices: len(owners), Runs: countRunsOf(files), Files: len(files), Bytes: sizeOf(files), Left: left}
}

// countRunsOf counts the runs of files of deleted devices: one per owner and time.
func countRunsOf(files []model.Backup) int { return len(runsOfFiles(files)) }

// DeleteOrphanRuns deletes the runs of deleted devices that the confirmation named (ids: the dump
// row of each run), whole, on disk and in the index, under one audit entry. Never a kept run (a
// bulk delete leaves what someone pinned; the single Delete is still there) and never a backup of
// a current device (ErrForeignBackup: a form must not reach one).
func (m *Manager) DeleteOrphanRuns(ctx context.Context, actor string, ids []model.BackupID) (BulkResult, error) {
	l := m.deviceLock(0)
	l.Lock()
	defer l.Unlock()
	files, left, gone, err := m.planOrphanRuns(ctx, ids)
	if err != nil {
		return BulkResult{}, err
	}
	removedFiles, failure := m.removeCounted(ctx, files)
	res := orphanResult(removedFiles, len(left)+gone)
	m.record(ctx, actor, "backup.delete_orphans", "orphans", map[string]any{
		"runs": res.Runs, "files": res.Files, "bytes": res.Bytes, "removed": runsOfFiles(removedFiles), "left": left}, failure)
	if res.Files > 0 {
		m.publishDeleted(0)
	}
	return res, failure
}

// DeleteUnreferenced deletes the files the confirmation listed (paths), as far as they are still
// unreferenced: it scans again under the directory lock and removes only a path that the scan
// still lists, never a path taken from the form alone. A file a backup row points to now, a file
// of the current layout (a backup in progress writes there), a path outside the old numbered
// folders and a file that is gone stay and are counted in BulkResult.Left. One audit entry lists
// every file.
func (m *Manager) DeleteUnreferenced(ctx context.Context, actor string, paths []string) (BulkResult, error) {
	if len(paths) == 0 {
		return BulkResult{}, ErrNothingSelected
	}
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	rows, err := m.o.Store.ListAllBackups(ctx)
	if err != nil {
		return BulkResult{}, fmt.Errorf("backup: list for unreferenced files: %w", err)
	}
	used := make(map[string]bool, len(rows))
	for _, b := range rows {
		used[strings.ToLower(b.Path)] = true
	}
	scan := map[string]UnreferencedFile{}
	for _, f := range m.scanUnreferenced(func(rel string) bool { return used[strings.ToLower(rel)] }) {
		scan[f.Path] = f
	}
	var res BulkResult
	var removed []string
	var failure error
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		f, listed := scan[p]
		if !listed {
			res.Left++
			continue
		}
		full, ok := m.safePath(f.Path) // the scan's own value, never the posted one
		if !ok {
			res.Left++
			continue
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			failure = fmt.Errorf("backup: remove unreferenced file: %w", err)
			break
		}
		res.Files++
		res.Bytes += f.Size
		removed = append(removed, f.Path)
	}
	m.removeEmptyNumericDirs()
	m.record(ctx, actor, "backup.delete_unreferenced", "unreferenced", map[string]any{
		"files": res.Files, "bytes": res.Bytes, "removed": removed, "left": res.Left}, failure)
	return res, failure
}
