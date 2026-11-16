package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// An MQTT "Failed ..." report ends the wait for the accept window: the
// device told us the download died, so waiting 3 minutes for nothing is
// pointless. Without the hint the second Upgrade comes after UpgradeAccept
// (TestEngineDownloadNeverStarts); with it, right away.
func TestHintFailedReportRetriesBeforeAcceptWindow(t *testing.T) {
	dev := plugDevice()
	dev.ignore = true
	h := newHarness(t, 2, dev)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	first := true
	h.api.onUpdate = func(model.DeviceID) {
		if first {
			first = false
			h.e.Hint(1, "Failed File Not Found (404)")
		}
	}
	h.run(t, h.e)

	j := h.job(t, id)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	evs, _ := h.st.JobEvents(context.Background(), id)
	found := false
	for _, ev := range evs {
		if ev.From == model.JobUpgradeIssued && ev.To == model.JobSetOtaURL && ev.Event == string(EvUpgradeReport) &&
			strings.Contains(ev.Detail, "Failed File Not Found (404)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the device's own reason is missing from the history: %+v", evs)
	}
	times := dev.upgradeTimes
	if len(times) != 2 || times[1].Sub(times[0]) >= testCfg().UpgradeAccept {
		t.Fatalf("upgrade times %v: the hint should have skipped the accept window", times)
	}
}

// "Successful" and anything else is only an acceleration that the probe
// confirms anyway; it must not change the job.
func TestHintSuccessfulReportChangesNothing(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.api.onUpdate = func(model.DeviceID) { h.e.Hint(1, "Successful. Restarting") }
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := h.rec.count("upgrade"); n != 2 {
		t.Fatalf("Upgrade sent %d times, want 2 (one per stage)", n)
	}
}

// MQTT delivers reports for every device, also those without a job (a
// manual upgrade from the console). They must be ignored without effort.
func TestHintWithoutJobIsIgnored(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	h.e.Hint(1, "Failed File Not Found (404)")
	h.e.Hint(99, "Failed anything")
	if n := len(h.rec.entries()); n != 0 {
		t.Fatalf("hint without a job did something: %v", h.rec.entries())
	}
}

// ---- CancelBatch ----

func submitBatch(t *testing.T, h *harness, ids ...model.DeviceID) model.BatchID {
	t.Helper()
	var reqs []Request
	for _, id := range ids {
		reqs = append(reqs, Request{DeviceID: id, Release: "16.0.0", SkipBackup: true})
	}
	b, err := h.e.Submit(context.Background(), "alice", reqs)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return b
}

// A batch is cancelled as a whole (spec 9.6): queued jobs end cancelled and
// never touch their devices.
func TestCancelBatchCancelsEveryQueuedJob(t *testing.T) {
	h := newHarness(t, 1, plugDevice(), plugDevice(), plugDevice())
	b := submitBatch(t, h, 1, 2, 3)
	before := len(h.rec.entries()) // Submit's preview probes are read-only
	if err := h.e.CancelBatch(context.Background(), "bob", b); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: b})
	if len(jobs) != 3 {
		t.Fatalf("%d jobs in batch", len(jobs))
	}
	for _, j := range jobs {
		if j.State != model.JobCancelled {
			t.Fatalf("job %d is %s", j.ID, j.State)
		}
	}
	h.run(t, h.e)
	for _, e := range h.rec.entries()[before:] {
		if !strings.HasPrefix(e, "save ") {
			t.Fatalf("cancelled batch touched devices: %v", h.rec.entries()[before:])
		}
	}
}

// Jobs that already finished stay as they are; the rest of the batch is
// cancelled. A second batch is not affected.
func TestCancelBatchLeavesFinishedAndOtherBatches(t *testing.T) {
	h := newHarness(t, 2, plugDevice(), plugDevice(), plugDevice())
	b1 := submitBatch(t, h, 1, 2)
	b2 := submitBatch(t, h, 3)
	// job 1 already finished
	j1 := h.job(t, 1)
	j1.State = model.JobSucceeded
	h.st.put(j1)

	if err := h.e.CancelBatch(context.Background(), "bob", b1); err != nil {
		t.Fatal(err)
	}
	if s := h.job(t, 1).State; s != model.JobSucceeded {
		t.Fatalf("finished job changed to %s", s)
	}
	if s := h.job(t, 2).State; s != model.JobCancelled {
		t.Fatalf("job 2 is %s", s)
	}
	if s := h.job(t, 3).State; s != model.JobQueued {
		t.Fatalf("job of batch %d is %s, want queued", b2, s)
	}
}

// Nothing to cancel is an error the caller can show, like Cancel on a
// terminal job; an unknown batch is not found.
func TestCancelBatchErrors(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	b := submitBatch(t, h, 1)
	if err := h.e.CancelBatch(context.Background(), "bob", b); err != nil {
		t.Fatal(err)
	}
	if err := h.e.CancelBatch(context.Background(), "bob", b); !errors.Is(err, ErrNotActive) {
		t.Fatalf("second CancelBatch: %v, want ErrNotActive", err)
	}
	if err := h.e.CancelBatch(context.Background(), "bob", 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown batch: %v, want store.ErrNotFound", err)
	}
}

// A job whose device is flashing cannot be stopped; the batch cancel marks
// it (CancelRequested) like Cancel does.
func TestCancelBatchWhileFlashing(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	b := submitBatch(t, h, 1)
	h.api.onUpdate = func(model.DeviceID) {
		if err := h.e.CancelBatch(context.Background(), "bob", b); err != nil {
			t.Errorf("CancelBatch: %v", err)
		}
	}
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobCancelled || !j.CancelRequested {
		t.Fatalf("job %s cancelRequested=%v", j.State, j.CancelRequested)
	}
}

// ---- minimal prefetch ----

type ensureRecorder struct {
	Firmware
	mu        sync.Mutex
	names     []string
	failMinim bool
}

func (r *ensureRecorder) Ensure(ctx context.Context, rel, name string) (model.Artifact, error) {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	if r.failMinim && strings.Contains(name, "minimal") {
		return model.Artifact{}, errors.New("upstream down")
	}
	return r.Firmware.Ensure(ctx, rel, name)
}

func previewWith(t *testing.T, freeKB int, failMinimal bool) (PreviewItem, []string) {
	t.Helper()
	d := plugDevice()
	d.ident.FreeKB = freeKB
	h := newHarness(t, 2, d)
	rec := &ensureRecorder{Firmware: h.e.fw, failMinim: failMinimal}
	h.e.fw = rec
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	return items[0], rec.names
}

// Tasmota's own fallback (image too large for the flash) derives
// tasmota-minimal.bin.gz from the directory of OtaUrl. A direct ESP8266 plan
// must therefore have the minimal image cached as well, or the fallback
// hits a 404.
func TestPlanPrefetchesMinimalForDirectESP8266(t *testing.T) {
	it, names := previewWith(t, 2000, false)
	if it.Plan != model.PlanDirect || it.Decision != "upgrade" {
		t.Fatalf("item %+v", it)
	}
	if strings.Join(names, ",") != "tasmota.bin.gz,tasmota-minimal.bin.gz" {
		t.Fatalf("Ensure calls %v, want the target and the minimal image", names)
	}
}

// A direct plan fits the free flash, so a failing prefetch of the minimal
// image does not block it; a staged plan cannot work without it.
func TestPlanMinimalPrefetchFailure(t *testing.T) {
	if it, _ := previewWith(t, 2000, true); it.Decision != "upgrade" || it.Plan != model.PlanDirect {
		t.Fatalf("direct plan blocked by a failed minimal prefetch: %+v", it)
	}
	if it, _ := previewWith(t, 352, true); it.Decision != "refuse" || !strings.Contains(it.Reason, "tasmota-minimal.bin.gz") {
		t.Fatalf("staged plan without minimal must be refused with the reason: %+v", it)
	}
}

// Why: the cache refuses to delete (and prunes around) what an active job
// records. The minimal image a direct ESP8266 job may need for Tasmota's own
// fallback must therefore be recorded in the job row, not only for staged plans.
func TestDirectESP8266JobRecordsTheMinimalArtifact(t *testing.T) {
	d := plugDevice()
	d.ident.FreeKB = 2000
	h := newHarness(t, 2, d)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.Plan != model.PlanDirect || j.TargetArtifact == 0 || j.MinimalArtifact == 0 || j.MinimalArtifact == j.TargetArtifact {
		t.Fatalf("plan %s target %d minimal %d", j.Plan, j.TargetArtifact, j.MinimalArtifact)
	}
}

// realDecide is the fake selector with fwselect's own Decide, so the preview meets the real
// settings-migration stop (#892) while everything else stays fake.
type realDecide struct{ *fakeSel }

func (realDecide) Decide(id model.Identity, c fwselect.Choice) (fwselect.Decision, error) {
	return fwselect.Decide(id, c)
}

// Why (#892): a device older than 10.0.0.3 must not be flashed past 16.0.0, because later releases no longer
// migrate its settings. The preview must say so and offer 16.0.0 as the step (the page turns StepRelease into
// the choice), and it must refuse before it fetches the newer file that will never be sent. A custom image is
// refused the same way, but the upstream step is not offered for it (the request would mix custom and upstream).
func TestPreviewOffersTheSettingsMigrationStep(t *testing.T) {
	old := plugDevice()
	old.ident.Version = "9.5.0"
	h := newHarness(t, 2, old, plugDevice())
	h.e.sel = realDecide{h.sel}
	rec := &ensureRecorder{Firmware: h.e.fw}
	h.e.fw = rec
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.1.0"}, {DeviceID: 2, Release: "16.1.0"}, {DeviceID: 1, Release: "16.1.0", Custom: "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	if it.Decision != decisionRefuse || it.StepRelease != fwselect.StepRelease || !strings.Contains(it.Reason, "no longer migrates") {
		t.Errorf("old device: %+v, want a refusal that offers %s", it, fwselect.StepRelease)
	}
	if items[1].StepRelease != "" || items[1].Decision != "upgrade" {
		t.Errorf("current device: %+v, want a plain upgrade without a step", items[1])
	}
	if c := items[2]; c.Decision != decisionRefuse || c.StepRelease != "" {
		t.Errorf("custom image on the old device: %+v, want a refusal without the upstream step", c)
	}
	for _, n := range rec.names {
		if n != "tasmota.bin.gz" && n != "tasmota-minimal.bin.gz" {
			t.Fatalf("unexpected fetch %q", n)
		}
	}
	if len(rec.names) != 2 { // only the current device's target and minimal image
		t.Errorf("Ensure calls %v: the refused device must not fetch anything", rec.names)
	}
}

// Why (#892): 16.0.0 is no step for firmware older than 8.1.0. Tasmota stopped migrating it in 9.2.0.6, and
// 16.0.0 resets such a settings block to its defaults (only Wi-Fi and MQTT survive), so offering 16.0.0 would
// lead the person to wipe module, template, rules and names. A 7.2.0 device is refused with Tasmota's migration
// path and offered no step, for 16.1.0 and for 16.0.0 alike.
func TestPreviewOffersNoStepBelowTheMigrationPath(t *testing.T) {
	old := plugDevice()
	old.ident.Version = "7.2.0"
	h := newHarness(t, 1, old)
	h.e.sel = realDecide{h.sel}
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.1.0"}, {DeviceID: 1, Release: fwselect.StepRelease}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Decision != decisionRefuse || it.StepRelease != "" || !strings.Contains(it.Reason, "migration path") {
			t.Errorf("7.2.0 device: %+v, want a refusal with the migration path and no step", it)
		}
	}
}
