package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Retrying the failed devices of a transaction: a NEW batch linked to the
// original, same target and options, planned afresh from what the devices
// look like now.

type auditRec struct {
	mu  sync.Mutex
	evs []model.AuditEvent
}

func (a *auditRec) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	a.evs = append(a.evs, e)
	a.mu.Unlock()
}

func failingDevices(n int) []*fakeDevice {
	var out []*fakeDevice
	for i := 0; i < n; i++ {
		d := plugDevice()
		d.ignore = true // the download never starts: the job fails
		out = append(out, d)
	}
	return out
}

func runFailed(t *testing.T, h *harness, reqs ...Request) model.BatchID {
	t.Helper()
	b, err := h.e.Submit(context.Background(), "alice", reqs)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	return b
}

func TestRetryCreatesLinkedBatchWithTheSameOptions(t *testing.T) {
	h := newHarness(t, 2, failingDevices(1)...)
	aud := &auditRec{}
	h.e.audit = aud
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", Policy: model.PolicyReinstall, SkipBackup: true, SkipPowerRestore: true, RestoreRelays: true})
	if j := h.job(t, 1); j.State != model.JobFailed {
		t.Fatalf("original job %s", j.State)
	}
	h.api.dev(1).ignore = false // the device is fine now

	res, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batch == 0 || res.Batch == orig || len(res.Devices) != 1 || res.Devices[0] != 1 {
		t.Fatalf("result %+v", res)
	}
	b, _ := h.st.GetBatch(context.Background(), res.Batch)
	if b.RetryOf != orig {
		t.Fatalf("batch %+v does not point at the original %d", b, orig)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 1 {
		t.Fatalf("%d jobs in the retry batch", len(jobs))
	}
	j := jobs[0]
	if j.OriginalOtaURL != origURL {
		t.Fatalf("the retry must restore the original's OtaUrl %q, not what the failed update left: %q", origURL, j.OriginalOtaURL)
	}
	if j.TargetRelease != "16.0.0" || j.Policy != model.PolicyReinstall || !j.SkipBackup || !j.SkipPowerRestore || !j.RestoreRelays || j.CreatedBy != "bob" {
		t.Fatalf("retry job lost the original's options: %+v", j)
	}
	h.run(t, h.e)
	if got := h.job(t, j.ID); got.State != model.JobSucceeded {
		t.Fatalf("retry job %s %q", got.State, got.LastError)
	}
	if got := h.job(t, 1); got.State != model.JobFailed {
		t.Fatalf("the original job changed to %s: history must stay", got.State)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.evs) != 1 || aud.evs[0].Action != "transaction.retry" || aud.evs[0].Actor != "bob" || aud.evs[0].Result != "ok" || !strings.Contains(aud.evs[0].Detail, `"retry_of":1`) {
		t.Fatalf("audit %+v", aud.evs)
	}
}

func TestRetryOfASelection(t *testing.T) {
	h := newHarness(t, 2, failingDevices(3)...)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true}, Request{DeviceID: 2, Release: "16.0.0", SkipBackup: true}, Request{DeviceID: 3, Release: "16.0.0", SkipBackup: true})
	for i := model.DeviceID(1); i <= 3; i++ {
		h.api.dev(i).ignore = false
	}
	res, err := h.e.SubmitRetry(context.Background(), "bob", orig, []model.DeviceID{2, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 1 || res.Devices[0] != 2 {
		t.Fatalf("only the selected device is retried: %+v", res)
	}
	if act, _ := h.st.ListJobs(context.Background(), store.JobFilter{DeviceID: 1, States: activeStates}); len(act) != 0 {
		t.Fatal("an unselected device got a job")
	}
	// all failed devices when nothing is selected, after the first retry finished
	h.run(t, h.e)
	res2, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil)
	if err != nil || len(res2.Devices) != 2 || res2.Devices[0] != 1 || res2.Devices[1] != 3 || len(res2.Skipped) != 1 || res2.Skipped[0].DeviceID != 2 {
		t.Fatalf("retry of everything: device 2 was fixed by the first retry and is skipped: %+v %v", res2, err)
	}
	rs, _ := h.st.BatchRetries(context.Background(), orig)
	if len(rs) != 2 {
		t.Fatalf("%d retries of the original, want 2 (one-to-many)", len(rs))
	}
}

func TestRetryRefusals(t *testing.T) {
	ok := plugDevice()
	bad := plugDevice()
	bad.ignore = true
	h := newHarness(t, 2, ok, bad)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true}, Request{DeviceID: 2, Release: "16.0.0", SkipBackup: true})
	if h.job(t, 1).State != model.JobSucceeded || h.job(t, 2).State != model.JobFailed {
		t.Fatalf("setup: %s %s", h.job(t, 1).State, h.job(t, 2).State)
	}
	var re *RefusedError
	if _, err := h.e.SubmitRetry(context.Background(), "bob", orig, []model.DeviceID{1}); !errors.As(err, &re) || !strings.Contains(re.Refused[0].Reason, "only failed or cancelled") {
		t.Fatalf("a succeeded job must not be retried: %v", err)
	}
	if _, err := h.e.SubmitRetry(context.Background(), "bob", orig, []model.DeviceID{99}); !errors.As(err, &re) || !strings.Contains(re.Refused[0].Reason, "not part of batch") {
		t.Fatalf("a device of another batch: %v", err)
	}
	if _, err := h.e.SubmitRetry(context.Background(), "bob", 999, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown batch: %v", err)
	}
	// nothing failed: nothing to retry
	h2 := newHarness(t, 2, plugDevice())
	o2 := runFailed(t, h2, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	if _, err := h2.e.SubmitRetry(context.Background(), "bob", o2, nil); !errors.Is(err, ErrNothingToRetry) {
		t.Fatalf("all succeeded: %v", err)
	}
}

func TestRetryRefusedWhileTheBatchRuns(t *testing.T) {
	h := newHarness(t, 2, failingDevices(2)...)
	b, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0"}, {DeviceID: 2, Release: "16.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	// jobs are queued, not terminal
	if _, err := h.e.SubmitRetry(context.Background(), "bob", b, nil); !errors.Is(err, ErrBatchRunning) {
		t.Fatalf("running batch: %v", err)
	}
}

// Another transaction has meanwhile started on the device: the retry is
// refused for it and nothing is created.
func TestRetryRefusedForADeviceWithAnActiveJob(t *testing.T) {
	h := newHarness(t, 2, failingDevices(2)...)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true}, Request{DeviceID: 2, Release: "16.0.0", SkipBackup: true})
	h.api.dev(1).ignore, h.api.dev(2).ignore = false, false
	if _, err := h.e.Submit(context.Background(), "carol", []Request{{DeviceID: 2, Release: "16.0.0"}}); err != nil {
		t.Fatal(err)
	}
	var re *RefusedError
	if _, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil); !errors.As(err, &re) || re.Refused[0].DeviceID != 2 || !strings.Contains(re.Refused[0].Reason, "already has active job") {
		t.Fatalf("got %v", err)
	}
	rs, _ := h.st.BatchRetries(context.Background(), orig)
	if len(rs) != 0 {
		t.Fatal("a refused retry created a batch")
	}
}

// A device that was fixed by hand since is "already at target": normal
// planning says skip, which is reported but does not block the others.
func TestRetrySkipsDevicesThatNeedNothingNow(t *testing.T) {
	h := newHarness(t, 2, failingDevices(2)...)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true}, Request{DeviceID: 2, Release: "16.0.0", SkipBackup: true})
	h.api.dev(1).ident = finalIdent() // fixed by hand
	h.api.dev(2).ignore = false
	res, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 1 || res.Devices[0] != 2 || len(res.Skipped) != 1 || res.Skipped[0].DeviceID != 1 {
		t.Fatalf("result %+v", res)
	}
}

// When every failed device has been fixed by hand, there is nothing to retry,
// and the caller learns which devices were skipped and why.
func TestRetryWhenEverythingIsFixedAlready(t *testing.T) {
	h := newHarness(t, 2, failingDevices(1)...)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.api.dev(1).ident = finalIdent()
	res, err := h.e.SubmitRetry(context.Background(), "bob", orig, nil)
	if !errors.Is(err, ErrNothingToRetry) || res.Batch != 0 || len(res.Skipped) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if rs, _ := h.st.BatchRetries(context.Background(), orig); len(rs) != 0 {
		t.Fatal("a batch was created for nothing")
	}
}

// A crash mid-flash leaves the device on minimal. The retry plans from that
// state: the device gets the job's target build as an explicit choice, like
// the update dialog does for devices on minimal, and finishes.
func TestRetryAfterACrashMidFlash(t *testing.T) {
	d := plugDevice()
	h := newHarness(t, 2, d)
	orig := runFailed(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	_ = orig
	// make the original a failure that left the device on minimal
	j := h.job(t, 1)
	j.State, j.LastError = model.JobFailed, "device did not come back"
	h.st.put(j)
	d.ident, d.otaURL, d.uptime = minimalIdent("16.0.0"), minimalURL, 0
	h.sel.mu.Lock()
	h.sel.choices = nil
	h.sel.mu.Unlock()

	res, err := h.e.SubmitRetry(context.Background(), "bob", j.BatchID, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.sel.mu.Lock()
	choices := append([]fwselect.Choice(nil), h.sel.choices...)
	h.sel.mu.Unlock()
	if len(choices) == 0 || choices[0].Variant != "tasmota" || !choices[0].AllowVariantChange {
		t.Fatalf("a device on minimal needs the build chosen explicitly: %+v", choices)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 1 || jobs[0].State != model.JobSucceeded || jobs[0].IdentityBefore.Variant != variantMinimal {
		t.Fatalf("retry job %+v", jobs)
	}
	if d.ident.Variant != "tasmota" {
		t.Fatalf("device ends on %+v", d.ident)
	}
}
