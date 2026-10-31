package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// notifyRuntime is a runtime whose webhook is on and points at a local receiver that records
// the bodies it gets.
func notifyRuntime(t *testing.T, extra map[string]string) (*runtime, *Services, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	svc := importServices(t)
	values := map[string]string{"NOTIFY_WEBHOOK": "true", "NOTIFY_WEBHOOK_URL": srv.URL}
	for k, v := range extra {
		values[k] = v
	}
	if _, err := svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "test", Values: values}); err != nil {
		t.Fatal(err)
	}
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	a.setupNotify()
	return a, svc, func() []string {
		a.notifier.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// Why: the person is told when the protection they switched on is not working. A snapshot that
// cannot be written must reach the webhook, a good one must stay silent.
func TestFailedScheduledSnapshotIsNotifiedAndASuccessIsNot(t *testing.T) {
	a, svc, sent := notifyRuntime(t, map[string]string{"SNAPSHOT_INTERVAL": "24h"})
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	a.scheduledSnapshot(t.Context())
	if got := sent(); len(got) != 0 {
		t.Fatalf("a good snapshot sent %v", got)
	}
	// A file where the snapshot folder should be makes the next one fail; reset the schedule's "due".
	if err := os.RemoveAll(filepath.Join(svc.Cfg.DataDir, snapshot.DirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.Cfg.DataDir, snapshot.DirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	a.scheduledSnapshot(t.Context())
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], `"event":"snapshot.failed"`) {
		t.Fatalf("a failed snapshot sent %v, want one snapshot.failed", got)
	}
}

// Why: "one message per job, not per device". A transaction of three devices sends nothing while
// one is still running, then exactly one message, and a second notification for the same
// transaction (two jobs ending together) must not repeat it.
func TestUpdateTransactionSendsOneMessageWhenItsLastJobEnds(t *testing.T) {
	a, svc, sent := notifyRuntime(t, nil)
	ctx := t.Context()
	var jobs []model.Job
	for _, n := range []string{"a", "b", "c"} {
		d, err := svc.Store.CreateDevice(ctx, model.Device{Name: n, Address: "192.0.2." + map[string]string{"a": "1", "b": "2", "c": "3"}[n]})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, model.Job{DeviceID: d, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.1.0", TargetVariant: "tasmota"})
	}
	batch, ids, err := svc.Store.CreateBatch(ctx, "u", jobs)
	if err != nil {
		t.Fatal(err)
	}
	end := func(i int, to model.JobState) model.Job {
		j := jobs[i]
		j.ID, j.BatchID, j.State = ids[i], batch, to
		if err := svc.Store.SaveTransition(ctx, j, model.JobEvent{JobID: ids[i], From: model.JobQueued, To: to, Event: "test"}); err != nil {
			t.Fatal(err)
		}
		return j
	}
	a.notifyBatchDone(end(0, model.JobSucceeded))
	a.notifyBatchDone(end(1, model.JobFailed))
	if got := sent(); len(got) != 0 {
		t.Fatalf("sent %v while a job was still queued", got)
	}
	last := end(2, model.JobSucceeded)
	a.notifyBatchDone(last)
	a.notifyBatchDone(last)
	got := sent()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want exactly one", len(got))
	}
	for _, want := range []string{`"event":"update.finished"`, `"result":"partial"`, `3 devices to 16.1.0; 1 failed`} {
		if !strings.Contains(got[0], want) {
			t.Errorf("body lacks %s: %s", want, got[0])
		}
	}
}

// Why: only update transactions are an "update finished". A policy fix or a bulk backup ending
// must not message anyone.
func TestOnlyUpdateJobsAreAnUpdateFinished(t *testing.T) {
	a, _, sent := notifyRuntime(t, nil)
	a.notifyBatchDone(model.Job{ID: 1, BatchID: 1, Kind: model.KindBackup, State: model.JobSucceeded})
	a.notifyBatchDone(model.Job{ID: 2, BatchID: 2, Kind: model.KindPolicy, State: model.JobFailed})
	if got := sent(); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}

// Why: "scheduled device backup failed" is one message per run that names the failing devices; a
// device that answers badly must be in it, and the run must still end normally.
func TestScheduledBackupRunWithAFailureSendsOneMessageNamingTheDevice(t *testing.T) {
	a, svc, sent := notifyRuntime(t, nil)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(broken.Close)
	port, err := strconv.Atoi(broken.URL[strings.LastIndex(broken.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateDevice(t.Context(), model.Device{Name: "workshop-lamp", Address: "127.0.0.1", Port: port}); err != nil {
		t.Fatal(err)
	}
	a.backupRun = newScheduledRun(nil)
	if err := a.setupBackups(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.backupAll(t.Context())
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], `"event":"backup.scheduled_failed"`) || !strings.Contains(got[0], "workshop-lamp") {
		t.Fatalf("sent %v, want one backup.scheduled_failed naming workshop-lamp", got)
	}
}

// Why: a nil dispatcher (a runtime built without one) must never panic the engine's callback.
func TestNotifyHooksAreNoOpsWithoutADispatcher(t *testing.T) {
	a := &runtime{}
	a.notifyBatchDone(model.Job{BatchID: 1, State: model.JobFailed})
}
