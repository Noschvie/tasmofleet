package ota

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// The audit trail of a planned update (#824): the person who plans it is in the entry of the plan, the start
// belongs to the scheduler, and every cancel names who asked for it.

// unitAudit keeps audit events in memory.
type unitAudit struct {
	mu  sync.Mutex
	evs []model.AuditEvent
}

func (a *unitAudit) Record(_ context.Context, ev model.AuditEvent) {
	a.mu.Lock()
	a.evs = append(a.evs, ev)
	a.mu.Unlock()
}

// of returns the recorded events with this action.
func (a *unitAudit) of(action string) []model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []model.AuditEvent
	for _, e := range a.evs {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (a *unitAudit) all() []model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.AuditEvent(nil), a.evs...)
}

func detailOf(t *testing.T, e model.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
		t.Fatalf("detail %q of %s is not JSON: %v", e.Detail, e.Action, err)
	}
	return d
}

// auditedEngine is an engine for h that records audit events and the actor of every backup.
func (h *harness) auditedEngine(audits *unitAudit, actors *[]string) *Engine {
	h.backups = &fakeBackups{rec: h.rec, actors: actors}
	e := NewEngine(Options{Machine: testCfg(), MaxParallel: 2, HoldFlashing: h.hold, Audit: audits}, h.st, h.st, h.api, h.fw, h.backups, h.clk, nil)
	e.sel = h.sel
	return e
}

// Why: Thomas planned an update, and the audit log then showed his name on the backup and on the log line of
// the run that started hours later without him. The plan is his action ("transaction.start", written when he
// plans it); the start is the scheduler's, with the time that was planned, so the log can tell a start on time
// from one that came late after a restart.
func TestScheduledStartIsAuditedAsTheSchedulerWithThePlannedTime(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	start := t0.Add(time.Minute)
	var actors []string
	audits := &unitAudit{}
	e := h.auditedEngine(audits, &actors)
	if _, err := e.Submit(context.Background(), "local:admin", []Request{{DeviceID: 1, Release: "16.0.0", StartAt: start}}); err != nil {
		t.Fatal(err)
	}
	h.clk.now = start.Add(4 * time.Minute) // TasmoFleet was busy or down: the start comes late
	h.run(t, e)

	got := audits.of("transaction.scheduled_start")
	if len(got) != 1 {
		t.Fatalf("%d scheduled-start entries, want 1: %+v", len(got), audits.all())
	}
	ev := got[0]
	if ev.Actor != "system" || ev.Result != "ok" {
		t.Errorf("actor %q result %q, want system and ok", ev.Actor, ev.Result)
	}
	d := detailOf(t, ev)
	if d["planned_start"] != start.UTC().Format(time.RFC3339) || d["late_seconds"] != float64(240) || d["device"] != "plug-1" {
		t.Errorf("detail %v lacks the planned start, the lateness or the device", d)
	}
	if want := "transaction:" + strconv.FormatInt(int64(h.job(t, 1).BatchID), 10); ev.Target != want {
		t.Errorf("target %q, want %q", ev.Target, want)
	}
	if len(actors) != 1 || actors[0] != "system" {
		t.Errorf("the update's backup was booked to %v, want the scheduler: the person planned it, they did not start it", actors)
	}
	for _, a := range audits.all() {
		if a.Actor == "local:admin" {
			t.Errorf("the scheduler's step %q is booked to the person who planned the update", a.Action)
		}
	}
}

// Why: only a planned start belongs to the scheduler. An update the person starts at once is their action
// from the first step to the backup, and nothing writes a scheduled-start entry for it.
func TestUpdateStartedAtOnceKeepsThePersonAsActor(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	var actors []string
	audits := &unitAudit{}
	e := h.auditedEngine(audits, &actors)
	if _, err := e.Submit(context.Background(), "local:admin", []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
		t.Fatal(err)
	}
	h.run(t, e)
	if len(actors) != 1 || actors[0] != "local:admin" {
		t.Errorf("backup booked to %v, want the person who started the update", actors)
	}
	if got := audits.of("transaction.scheduled_start"); len(got) != 0 {
		t.Errorf("an update that was not planned has a scheduled-start entry: %+v", got)
	}
}

// Why: a plan that was cancelled used to vanish from the audit log without a trace, so nobody could say who
// stopped the update that did not run. Every cancel is one entry naming the person, whether the update had not
// started (planned, queued) or was already running.
func TestEveryCancelIsAuditedNamingWhoCancelled(t *testing.T) {
	ctx := context.Background()
	t.Run("a planned job", func(t *testing.T) {
		h := newHarness(t, 2, plugDevice())
		audits := &unitAudit{}
		e := h.auditedEngine(audits, nil)
		if _, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0", StartAt: t0.Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		if err := e.Cancel(ctx, "local:bob", 1); err != nil {
			t.Fatal(err)
		}
		got := audits.of("transaction.cancel")
		if len(got) != 1 || got[0].Actor != "local:bob" || got[0].Target != "transaction:1" {
			t.Fatalf("cancel entries %+v, want one by local:bob on transaction:1", got)
		}
		d := detailOf(t, got[0])
		if d["devices"] != float64(1) || d["waiting"] != float64(1) || d["planned_start"] != t0.Add(time.Hour).UTC().Format(time.RFC3339) {
			t.Errorf("detail %v: want one device, waiting, with the planned start", d)
		}
		// Why: the cancel of one job of a transaction must not read as the whole transaction stopped; the entry
		// names the job and its device.
		if d["job"] != float64(1) || d["device_id"] != float64(1) {
			t.Errorf("detail %v: a cancel of one job names the job and the device", d)
		}
	})
	t.Run("a whole transaction is one entry", func(t *testing.T) {
		h := newHarness(t, 2, plugDevice(), plugDevice())
		audits := &unitAudit{}
		e := h.auditedEngine(audits, nil)
		id, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0", StartAt: t0.Add(time.Hour)}, {DeviceID: 2, Release: "16.0.0", StartAt: t0.Add(time.Hour)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.CancelBatch(ctx, "local:bob", id); err != nil {
			t.Fatal(err)
		}
		got := audits.of("transaction.cancel")
		if len(got) != 1 || got[0].Actor != "local:bob" || detailOf(t, got[0])["devices"] != float64(2) {
			t.Fatalf("cancel entries %+v, want one entry by local:bob for 2 devices", got)
		}
		if _, one := detailOf(t, got[0])["job"]; one {
			t.Errorf("a whole-transaction cancel names one job: %s", got[0].Detail)
		}
		// A second cancel finds nothing active: it changes nothing and leaves no entry.
		if err := e.CancelBatch(ctx, "local:bob", id); err == nil {
			t.Fatal("cancelling a finished transaction did not fail")
		}
		if n := len(audits.of("transaction.cancel")); n != 1 {
			t.Errorf("%d cancel entries after a cancel that did nothing, want 1", n)
		}
	})
	t.Run("a job that is not planned", func(t *testing.T) {
		h := newHarness(t, 2, plugDevice())
		audits := &unitAudit{}
		e := h.auditedEngine(audits, nil)
		if _, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
			t.Fatal(err)
		}
		if err := e.Cancel(ctx, "local:bob", 1); err != nil {
			t.Fatal(err)
		}
		got := audits.of("transaction.cancel")
		if len(got) != 1 || got[0].Actor != "local:bob" {
			t.Fatalf("cancel entries %+v, want one by local:bob", got)
		}
		if _, planned := detailOf(t, got[0])["planned_start"]; planned {
			t.Errorf("a job that was never planned has a planned start: %s", got[0].Detail)
		}
	})
	t.Run("a job that is running", func(t *testing.T) {
		h := newHarness(t, 2, plugDevice())
		audits := &unitAudit{}
		e := h.auditedEngine(audits, nil)
		if _, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
			t.Fatal(err)
		}
		h.api.onUpdate = func(model.DeviceID) { // the device is flashing: the cancel is a request
			if err := e.Cancel(ctx, "local:bob", 1); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
		h.run(t, e)
		got := audits.of("transaction.cancel")
		if len(got) != 1 || got[0].Actor != "local:bob" || got[0].Target != "transaction:1" {
			t.Fatalf("cancel entries %+v, want one by local:bob on transaction:1", got)
		}
		if d := detailOf(t, got[0]); d["running"] != float64(1) || d["waiting"] != float64(0) {
			t.Errorf("detail %v: a cancel of a running job is a request that is still running", d)
		} else if d["job"] != float64(1) || d["device_id"] != float64(1) {
			t.Errorf("detail %v: a cancel of a running job names the job and the device", d)
		}
	})
}

// Why: the store has one connection, so a read made while the engine's lock is held stalls Hint (called from
// MQTT) and every worker start behind a busy database. The cancel of a running job reaches its worker without a
// read; the transaction for the audit entry is read after the lock is released.
func TestCancelOfARunningJobReadsNothingWhileTheEngineLockIsHeld(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 2, plugDevice())
	audits := &unitAudit{}
	e := h.auditedEngine(audits, nil)
	if _, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
		t.Fatal(err)
	}
	inbox := make(chan Event, 1)
	e.workers[1] = inbox // a worker is running the job
	heldDuringRead := 0
	h.st.onGet = func() {
		if e.mu.TryLock() {
			e.mu.Unlock()
		} else {
			heldDuringRead++
		}
	}
	if err := e.Cancel(ctx, "local:bob", 1); err != nil {
		t.Fatal(err)
	}
	if heldDuringRead != 0 {
		t.Errorf("the job was read %d times while the engine's lock was held", heldDuringRead)
	}
	if len(inbox) != 1 {
		t.Errorf("the worker did not get the cancel request")
	}
	if got := audits.of("transaction.cancel"); len(got) != 1 || got[0].Target != "transaction:1" {
		t.Errorf("cancel entries %+v, want one on transaction:1", got)
	}
}

// Why: a cancel that did not happen must not be in the audit log as one. A job that cannot be read, a cancel
// that cannot be stored and a running job that cannot take the request leave no entry, and the person is told.
func TestACancelThatDidNotHappenIsNotAudited(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		setup func(fs *flakyStore, e *Engine)
	}{
		{"the job cannot be read", func(fs *flakyStore, _ *Engine) { fs.failGet = true }},
		{"the cancel cannot be stored", func(fs *flakyStore, _ *Engine) { fs.failSave = true }},
		{"the running job is busy", func(_ *flakyStore, e *Engine) { e.workers[1] = make(chan Event) }}, // nobody reads: the inbox is full
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, 2, plugDevice())
			audits := &unitAudit{}
			fs := &flakyStore{fakeStore: h.st}
			e := NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Audit: audits}, fs, fs, h.api, h.fw, h.backups, h.clk, nil)
			e.sel = h.sel
			if _, err := e.Submit(ctx, "alice", []Request{{DeviceID: 1, Release: "16.0.0", StartAt: t0.Add(time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			c.setup(fs, e)
			if err := e.Cancel(ctx, "local:bob", 1); err == nil {
				t.Fatal("the cancel did not fail")
			}
			if got := audits.of("transaction.cancel"); len(got) != 0 {
				t.Errorf("a cancel that did not happen is audited: %+v", got)
			}
		})
	}
}

// Why: a whole-transaction cancel is one entry about that transaction. When the first of its jobs is a running
// one whose read failed, the entry used to name that single job ("job:7") and read as the cancel of one device.
func TestCancelOfATransactionIsAuditedAgainstTheTransactionEvenWhenItsFirstJobCouldNotBeRead(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	audits := &unitAudit{}
	e := h.auditedEngine(audits, nil)
	e.auditCancel(context.Background(), "local:bob", []cancelDone{{job: 7}, {job: 8, batch: 3, waiting: true}}, 3)
	got := audits.of("transaction.cancel")
	if len(got) != 1 || got[0].Target != "transaction:3" {
		t.Fatalf("cancel entries %+v, want one on transaction:3", got)
	}
}
