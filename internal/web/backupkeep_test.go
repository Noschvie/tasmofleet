package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// keepBackups adds the N1/N2 parts of the backup service to adminBackups.
type keepBackups struct {
	*adminBackups
	keptCalls  []string // "id:kept"
	orphan     bool     // SetKept answers with a backup of a deleted device
	selected   [][]model.BackupID
	deviceOf   model.DeviceID // owner the ids belong to
	all        []model.DeviceID
	pruned     []model.Backup
	attempts   []model.BackupAttempt
	planNewest bool // PlanDeleteRuns says the newest run goes
}

func (k *keepBackups) SetKept(_ context.Context, _ string, id model.BackupID, kept bool) (model.Backup, error) {
	k.keptCalls = append(k.keptCalls, fmt.Sprintf("%d:%d", id, b2i(kept)))
	if id == 99 {
		return model.Backup{}, store.ErrNotFound
	}
	b := model.Backup{ID: id, DeviceID: 1, Kept: kept}
	if k.orphan {
		b.DeviceID = 0
	}
	return b, nil
}
func (k *keepBackups) Retention() backup.Retention { return backup.Retention{Keep: 5} }
func (k *keepBackups) PrunedAtNextRun(context.Context, model.DeviceID) ([]model.Backup, error) {
	return k.pruned, nil
}
func (k *keepBackups) Attempts(context.Context, model.DeviceID, int) ([]model.BackupAttempt, error) {
	return k.attempts, nil
}
func (k *keepBackups) PlanDeleteRuns(_ context.Context, _ model.DeviceID, ids []model.BackupID) (backup.Deletion, error) {
	return backup.Deletion{Runs: len(ids), Newest: k.planNewest}, nil
}
func (k *keepBackups) DeleteRuns(_ context.Context, _ string, device model.DeviceID, ids []model.BackupID) (backup.Deletion, error) {
	if len(ids) == 0 {
		return backup.Deletion{}, backup.ErrNothingSelected
	}
	if device != k.deviceOf {
		return backup.Deletion{}, backup.ErrForeignBackup
	}
	k.selected = append(k.selected, ids)
	return backup.Deletion{Device: device, Runs: len(ids)}, nil
}
func (k *keepBackups) DeleteAll(_ context.Context, _ string, device model.DeviceID) (backup.Deletion, error) {
	k.all = append(k.all, device)
	return backup.Deletion{Device: device}, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func keepEnv(t *testing.T) (*env, *keepBackups) {
	t.Helper()
	k := &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}
	e := setupWith(t, func(d *Deps) { d.Backups = k })
	return e, k
}

// Why: Keep only protects a backup from the retention, so operators may set
// it; the toggle posts the state it switches to, so a double click cannot
// undo it, and a request without a state changes nothing.
func TestKeepIsOperatorAndExplicit(t *testing.T) {
	e, k := keepEnv(t)
	if resp, _ := e.do(t, "POST", "/backups/4/keep", "", url.Values{"kept": {"1"}}); resp.StatusCode != 401 {
		t.Errorf("no session: %d", resp.StatusCode)
	}
	resp, _ := e.do(t, "POST", "/backups/4/keep", "operator", url.Values{"kept": {"1"}})
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/devices/1/backups?ok=kept") {
		t.Fatalf("keep: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e.do(t, "POST", "/backups/4/keep", "operator", url.Values{}); resp.StatusCode != 422 {
		t.Errorf("no state: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/backups/99/keep", "operator", url.Values{"kept": {"0"}}); resp.StatusCode != 404 {
		t.Errorf("unknown backup: %d", resp.StatusCode)
	}
	k.orphan = true
	resp, _ = e.do(t, "POST", "/backups/5/keep", "admin", url.Values{"kept": {"0"}})
	if !strings.HasSuffix(resp.Header.Get("Location"), "/settings/system?tab=backups&ok=released") {
		t.Errorf("orphan goes back to the settings: %q", resp.Header.Get("Location"))
	}
	if strings.Join(k.keptCalls, " ") != "4:1 99:0 5:0" {
		t.Errorf("calls %v", k.keptCalls)
	}
}

// Why: "Delete selected" destroys backups, so it is admin-only, needs the
// confirmation, and refuses ids of another device without deleting anything.
func TestDeleteSelectedIsAdminConfirmedAndScoped(t *testing.T) {
	e, k := keepEnv(t)
	form := url.Values{"id": {"4", "1"}, "confirm": {"1"}}
	if resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "operator", form); resp.StatusCode != 403 {
		t.Errorf("operator: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "admin", url.Values{"id": {"4"}}); resp.StatusCode != 422 {
		t.Errorf("unconfirmed: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "admin", url.Values{"confirm": {"1"}}); resp.StatusCode != 422 {
		t.Errorf("nothing selected: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/2/backups/delete", "admin", form); resp.StatusCode != 404 {
		t.Errorf("foreign ids: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "admin", url.Values{"id": {"x"}, "confirm": {"1"}}); resp.StatusCode != 404 {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
	if len(k.selected) != 0 {
		t.Fatalf("a refused request deleted: %v", k.selected)
	}
	resp, _ := e.do(t, "POST", "/devices/1/backups/delete", "admin", form)
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/devices/1/backups?ok=deleted&n=2") || len(k.selected) != 1 {
		t.Errorf("delete: %d %q %v", resp.StatusCode, resp.Header.Get("Location"), k.selected)
	}
}

// Why (N2): deleting a device asks what happens to its backups, and the
// acknowledgement is checked on the server, not only by the checkbox. "Keep"
// must not touch a backup; "Delete" removes them (kept ones too) before the
// device goes. The audit entry says which.
func TestDeviceDeleteKeepsOrDeletesBackups(t *testing.T) {
	e, k := keepEnv(t)
	if resp, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"keep"}}); resp.StatusCode != 422 {
		t.Fatalf("without ack: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"maybe"}, "ack": {"1"}}); resp.StatusCode != 422 {
		t.Fatalf("bad choice: %d", resp.StatusCode)
	}
	if _, err := e.inv.Get(context.Background(), 1); err != nil {
		t.Fatal("a refused delete removed the device")
	}
	resp, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"keep"}, "ack": {"1"}})
	if resp.StatusCode != 303 || len(k.all) != 0 || e.inv.lastDelete.Backups != "keep" {
		t.Fatalf("keep: %d all=%v opt=%+v", resp.StatusCode, k.all, e.inv.lastDelete)
	}
	resp, _ = e.do(t, "POST", "/devices/2/delete", "admin", url.Values{"backups": {"delete"}, "ack": {"1"}})
	if resp.StatusCode != 303 || len(k.all) != 1 || k.all[0] != 2 || e.inv.lastDelete.Backups != "delete" {
		t.Fatalf("delete: %d all=%v opt=%+v", resp.StatusCode, k.all, e.inv.lastDelete)
	}
	// The v0.10 dialogs post no choice: the backups go, as they did.
	e.inv.devs[7] = model.Device{ID: 7, Name: "x", Address: "10.0.0.7"}
	if resp, _ := e.do(t, "POST", "/devices/7/delete", "admin", url.Values{}); resp.StatusCode != 303 || len(k.all) != 2 {
		t.Errorf("legacy form: %d all=%v", resp.StatusCode, k.all)
	}
	if resp, _ := e.do(t, "POST", "/devices/42/delete", "admin", url.Values{"backups": {"delete"}, "ack": {"1"}}); resp.StatusCode != 404 || len(k.all) != 2 {
		t.Errorf("unknown device: %d all=%v", resp.StatusCode, k.all)
	}
}

// Why: the retention line and the row labels must say what the retention
// really does; the trigger text names what started each backup.
func TestRetentionViewAndTriggerText(t *testing.T) {
	k := &keepBackups{adminBackups: &adminBackups{}}
	h := &handler{d: Deps{Backups: k}}
	t0 := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)
	k.pruned = []model.Backup{{ID: 1, TakenAt: t0}, {ID: 2, TakenAt: t0}, {ID: 3, TakenAt: t0.Add(24 * time.Hour)}}
	k.attempts = []model.BackupAttempt{{Reason: "HTTP 401: the device rejected the web password"}}
	v, ok, err := h.backupRetention(context.Background(), 1)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := v.Line(); got != "Keeping the last 5 backups and anything kept · 2 will be pruned at the next run" {
		t.Errorf("line %q", got)
	}
	if !v.IsPruned(model.Backup{TakenAt: t0}) || v.IsPruned(model.Backup{TakenAt: t0.Add(time.Hour)}) || len(v.Attempts) != 1 {
		t.Errorf("view %+v", v)
	}
	if got := (backupRetentionView{Keep: 1, MaxAge: 48 * time.Hour}).Line(); got != "Keeping the last 1 backup and everything from the last "+longDuration(48*time.Hour)+" and anything kept" {
		t.Errorf("line with age %q", got)
	}
	for trig, want := range map[model.BackupTrigger]string{
		model.TriggerManual: "Manual", model.TriggerScheduled: "Scheduled", model.TriggerUpdate: "Automatic, before update to 14.3.0",
		model.TriggerRestore: "Automatic, before restore", model.TriggerReplace: "Automatic, during replace", model.TriggerBulk: "Bulk backup", "": "",
	} {
		if got := backupTriggerText(trig, "14.3.0"); got != want {
			t.Errorf("%q: %q, want %q", trig, got, want)
		}
	}
}
