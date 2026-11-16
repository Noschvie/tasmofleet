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
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/snapshot"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
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
	a.setupNotify(t.Context())
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
	a, svc, sent := notifyRuntime(t, map[string]string{"SNAPSHOT_DAYS": everyDay})
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	a.scheduledSnapshot(t.Context(), time.Now())
	if got := sent(); len(got) != 0 {
		t.Fatalf("a good snapshot sent %v", got)
	}
	// A file where the snapshot folder should be makes the next one fail; start a fresh manager.
	if err := os.RemoveAll(filepath.Join(svc.Cfg.DataDir, snapshot.DirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.Cfg.DataDir, snapshot.DirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.snapshots = snapshot.New(svc.Store, svc.Cfg.DataDir)
	a.scheduledSnapshot(t.Context(), time.Now())
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
	id, err := svc.Store.CreateDevice(t.Context(), model.Device{Name: "workshop-lamp", Address: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	// A device that was never heard is skipped, not failed (#409): this one has answered before.
	if err := svc.Store.SetLastSeen(t.Context(), id, model.HealthOnline, time.Now()); err != nil {
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

// Why (#540): every open tab gets the same event from the hub and has to tell that two events
// are different ones and that one event is the same in all tabs. The id must differ between
// events, and the hub must carry what the person's event settings allow, nothing else.
func TestBrowserNoticesReachTheHubWithUniqueIDs(t *testing.T) {
	a, svc, _ := notifyRuntime(t, map[string]string{"NOTIFY_SNAPSHOT": "false"})
	ch, cancel := svc.Hub.Subscribe(events.TopicNotice)
	defer cancel()
	cfg := a.notifier.Config()
	a.notifier.Emit(notify.BackupFailed(cfg, 3, []notify.FailedDevice{{Name: "a"}}, time.Now()))
	a.notifier.Emit(notify.BackupFailed(cfg, 3, []notify.FailedDevice{{Name: "a"}}, time.Now()))
	a.notifier.Emit(notify.SnapshotFailed(cfg, "disk full", time.Now())) // switched off
	var ids []string
	for range 2 {
		select {
		case ev := <-ch:
			n, ok := ev.Payload.(events.Notice)
			if !ok || n.Name != notify.EventBackupFailed || n.Path != "/devices/backups?filter=failed" {
				t.Fatalf("notice = %+v", ev.Payload)
			}
			ids = append(ids, n.ID)
		case <-time.After(2 * time.Second):
			t.Fatal("no notice on the hub")
		}
	}
	if ids[0] == ids[1] || ids[0] == "" {
		t.Errorf("ids %q: two events must not share one", ids)
	}
	select {
	case ev := <-ch:
		t.Fatalf("a switched-off event reached the hub: %+v", ev.Payload)
	case <-time.After(100 * time.Millisecond):
	}
}

// Why (#603): a scheduled transaction that was not started because TasmoFleet was down sends one message
// that says so ("Scheduled update not started"), not an "update finished" that reads like a cancel by the
// person, and also under "only problems": something the person planned did not happen, and nobody sees it
// otherwise. A device the person cancelled before the start does not change that (the rest still did not
// run), and the message counts only the devices that were not started. A transaction where a device did
// run stays a normal "update finished".
func TestNotStartedTransactionSendsOneNotStartedMessage(t *testing.T) {
	type end struct {
		state      model.JobState
		notStarted bool
	}
	missed := end{model.JobCancelled, true}
	for _, tc := range []struct {
		name string
		when string
		ends []end
		want []string
		not  []string
	}{
		{"all not started", "problems", []end{missed, missed}, []string{`"event":"update.not_started"`, `"result":"not_started"`,
			`"title":"Scheduled update not started"`, `(Update to Tasmota 16.1.0, 2 devices) was planned for`, "Thu, Nov 5 at 03:00"}, nil},
		{"one cancelled by the person before the start", "problems", []end{missed, {model.JobCancelled, false}},
			[]string{`"event":"update.not_started"`, `(Update to Tasmota 16.1.0, 1 device) was planned for`, "Thu, Nov 5 at 03:00"}, nil},
		{"one device ran", "any", []end{missed, {model.JobSucceeded, false}},
			[]string{`"event":"update.finished"`}, []string{`not_started`}},
		{"one device failed", "any", []end{missed, {model.JobFailed, false}},
			[]string{`"event":"update.finished"`}, []string{`not_started`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, svc, sent := notifyRuntime(t, map[string]string{"NOTIFY_UPDATE_WHEN": tc.when})
			ctx := t.Context()
			start := time.Date(2026, 11, 5, 3, 0, 0, 0, time.Local)
			var jobs []model.Job
			for i := range tc.ends {
				d, err := svc.Store.CreateDevice(ctx, model.Device{Name: "d" + strconv.Itoa(i), Address: "192.0.2." + strconv.Itoa(i+1)})
				if err != nil {
					t.Fatal(err)
				}
				jobs = append(jobs, model.Job{DeviceID: d, State: model.JobScheduled, StartAt: start, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.1.0"})
			}
			batch, ids, err := svc.Store.CreateBatch(ctx, "u", jobs)
			if err != nil {
				t.Fatal(err)
			}
			var last model.Job
			for i, e := range tc.ends {
				j := jobs[i]
				j.ID, j.BatchID, j.State, j.NotStarted = ids[i], batch, e.state, e.notStarted
				if err := svc.Store.SaveTransition(ctx, j, model.JobEvent{JobID: ids[i], From: model.JobScheduled, To: e.state, Event: "test"}); err != nil {
					t.Fatal(err)
				}
				stored, err := svc.Store.GetJob(ctx, ids[i]) // as the engine's callback gets it: StartAt read back from the store
				if err != nil {
					t.Fatal(err)
				}
				last = stored
			}
			a.notifyBatchDone(last)
			a.notifyBatchDone(last)
			got := sent()
			if len(got) != 1 {
				t.Fatalf("sent %d messages, want exactly one: %v", len(got), got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got[0], want) {
					t.Errorf("body lacks %s: %s", want, got[0])
				}
			}
			for _, not := range tc.not {
				if strings.Contains(got[0], not) {
					t.Errorf("body has %s: %s", not, got[0])
				}
			}
		})
	}
}

// Why (#610): Web Push is on from the first start (the VAPID key is made then), the web routes get the
// channel, and a restart keeps the key: a new key would silently break every browser's subscription.
func TestSetupNotifyTurnsWebPushOnWithAKeptKey(t *testing.T) {
	a, svc, _ := notifyRuntime(t, nil)
	if a.push == nil || a.push.PublicKey() == "" {
		t.Fatal("web push is off after the first start")
	}
	if a.webDeps(nil, nil).Push == nil {
		t.Fatal("the web routes do not get the push channel")
	}
	first := a.push.PublicKey()
	again := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	again.setupNotify(t.Context())
	if again.push == nil || again.push.PublicKey() != first {
		t.Fatal("a restart made a new VAPID key")
	}
}
