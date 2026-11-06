package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Why (#466): a delete under a running job leaves the engine writing to a job row that is gone (or,
// after id reuse, to somebody else's). The page says so up front, the post is refused with the same
// reason, and above all the person's backups are not deleted before the refusal.

const (
	updateRunningText  = "An update is running on this device. Cancel it or wait until it ends, then delete the device."
	restoreRunningText = "A restore is running on this device. Wait until it ends, then delete the device."
)

var deleteButton = regexp.MustCompile(`<button[^>]*dd-go[^>]*>`)

func busyEnv(t *testing.T, jobs []model.Job, rj *fakeRestore) (*env, *keepBackups) {
	t.Helper()
	k := &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}
	e := setupWith(t, func(d *Deps) {
		d.Backups = k
		d.Jobs = &fakeJobs{jobs: jobs}
		if rj != nil {
			d.Restore = rj
		}
	})
	return e, k
}

var runningUpdate = []model.Job{{ID: 5, BatchID: 3, DeviceID: 1, State: model.JobAwaitReboot}}

func TestDeletePageRefusesWhileAnUpdateRuns(t *testing.T) {
	e, _ := busyEnv(t, runningUpdate, nil)
	resp, body := e.do(t, "GET", "/devices/1/delete", "admin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, updateRunningText) || !strings.Contains(body, `href="/tf/transactions/3"`) {
		t.Fatalf("the page must say why and link to the transaction:\n%.1500s", body)
	}
	if b := deleteButton.FindString(body); b == "" || !strings.Contains(b, "disabled") {
		t.Fatalf("the Delete button must be disabled, got %q", b)
	}
	// the other device has no job: its page is the normal one
	if _, body := e.do(t, "GET", "/devices/2/delete", "admin", nil); strings.Contains(body, updateRunningText) || strings.Contains(deleteButton.FindString(body), "disabled") {
		t.Fatalf("a device without a job must stay deletable:\n%.800s", body)
	}
}

func TestDeletePostRefusedWhileAnUpdateRunsKeepsDeviceAndBackups(t *testing.T) {
	e, k := busyEnv(t, runningUpdate, nil)
	form := url.Values{"backups": {"delete"}, "ack": {"1"}}
	resp, body := e.do(t, "POST", "/devices/1/delete", "admin", form)
	if resp.StatusCode != 409 || !strings.Contains(body, updateRunningText) {
		t.Fatalf("status %d: %.800s", resp.StatusCode, body)
	}
	if _, err := e.inv.Get(context.Background(), 1); err != nil {
		t.Fatal("the refused delete removed the device")
	}
	if len(k.all) != 0 {
		t.Fatalf("the backups were deleted (%v) although the device stayed: the refusal must come first", k.all)
	}
	e.aud.mu.Lock()
	defer e.aud.mu.Unlock()
	var found bool
	for _, ev := range e.aud.events {
		if ev.Action == "device.delete" && ev.Target == "device:1" && strings.HasPrefix(ev.Result, "refused") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal must be audited: %+v", e.aud.events)
	}
}

func TestDeleteFinishedJobDoesNotBlock(t *testing.T) {
	e, _ := busyEnv(t, []model.Job{{ID: 5, BatchID: 3, DeviceID: 1, State: model.JobSucceeded}}, nil)
	if resp, body := e.do(t, "GET", "/devices/1/delete", "admin", nil); strings.Contains(body, "is running") || resp.StatusCode != 200 {
		t.Fatalf("a finished job holds nothing: %d %.600s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"keep"}, "ack": {"1"}}); resp.StatusCode != 303 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
}

func TestDeleteRefusedWhileARestoreRuns(t *testing.T) {
	e, k := busyEnv(t, nil, &fakeRestore{jobs: map[store.RestoreID]restore.Job{9: {ID: 9, DeviceID: 1, State: store.RestoreBackup}}})
	_, body := e.do(t, "GET", "/devices/1/delete", "admin", nil)
	if !strings.Contains(body, restoreRunningText) || !strings.Contains(body, `href="/tf/restores/9"`) {
		t.Fatalf("the page must name the restore and link to it:\n%.1500s", body)
	}
	resp, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"delete"}, "ack": {"1"}})
	if resp.StatusCode != 409 || len(k.all) != 0 {
		t.Fatalf("status %d, backups deleted %v", resp.StatusCode, k.all)
	}
}

// busyInv is an inventory that loses the race: no job was visible when the page checked, but the
// store refuses the delete (a job started in between).
type busyInv struct{ *fakeInv }

func (busyInv) DeleteWith(context.Context, string, model.DeviceID, inventory.DeleteOptions) error {
	return store.ErrDeviceBusy
}

func TestDeleteStoreRefusalIsShownAsBusy(t *testing.T) {
	k := &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}
	e := setupWith(t, func(d *Deps) {
		d.Backups = k
		d.Inventory = busyInv{d.Inventory.(*fakeInv)}
	})
	resp, body := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"keep"}, "ack": {"1"}})
	if resp.StatusCode != 409 || !strings.Contains(body, "busy") {
		t.Fatalf("the store's refusal must come back as 409 with a reason, not a 500: %d %.600s", resp.StatusCode, body)
	}
}

// Why: when a job starts between the page's check and the delete, "Delete the backups too" has
// already run. The person must be told that, not left to believe the backups are still there.
func TestDeleteStoreRefusalAfterBackupsWentSaysSo(t *testing.T) {
	for choice, wantGone := range map[string]bool{"delete": true, "keep": false} {
		k := &keepBackups{adminBackups: &adminBackups{}, deviceOf: 1}
		e := setupWith(t, func(d *Deps) {
			d.Backups = k
			d.Inventory = busyInv{d.Inventory.(*fakeInv)}
		})
		resp, body := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {choice}, "ack": {"1"}})
		if resp.StatusCode != 409 {
			t.Fatalf("%s: status %d", choice, resp.StatusCode)
		}
		if got := strings.Contains(body, "The backups were already deleted."); got != wantGone {
			t.Errorf("%s: backups-gone note shown = %v, want %v", choice, got, wantGone)
		}
		if (len(k.all) == 1) != wantGone {
			t.Errorf("%s: backups deleted %v", choice, k.all)
		}
	}
}
