//go:build integration

package ota

import (
	"context"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// A crash mid-flash: the minimal hop succeeds, the second hop never gets the
// image, the job fails and the device sits on minimal. The retry starts from
// that state (fresh preflight, minimal device, explicit build) and finishes
// the update, with the OtaUrl back where it was.
func TestITRetryAfterCrashMidFlashDeviceOnMinimal(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	orig := e.ctrl[dev].OtaURL()
	upgrades := 0
	e.beforeUpgrade = func(id model.DeviceID) {
		if upgrades++; upgrades == 2 { // the second hop (and its retry) fails to download
			e.ctrl[id].AddFault(simulator.Fault{After: "Upgrade 1", Kind: simulator.FaultDownloadFail})
		}
	}
	eng := e.start(2)
	first := e.submit(eng, Request{DeviceID: dev})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("original job %s %q", j.State, j.LastError)
	}
	e.settle(dev)
	if !e.ctrl[dev].Minimal() {
		t.Fatalf("device should sit on minimal, runs %s", e.ctrl[dev].Image())
	}
	e.ctrl[dev].ClearFaults()
	e.beforeUpgrade = nil

	res, err := eng.SubmitRetry(context.Background(), "it", first, nil)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := e.st.GetBatch(context.Background(), res.Batch)
	if err != nil || rb.RetryOf != first {
		t.Fatalf("retry batch %+v %v", rb, err)
	}
	jobs, _ := e.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	rj := e.waitJob(dev, func(j model.Job) bool { return j.ID == jobs[0].ID && j.State.Terminal() }, "terminal")
	if rj.State != model.JobSucceeded || rj.LastError != "" {
		t.Fatalf("retry job %s %q%s", rj.State, rj.LastError, e.history())
	}
	if rj.IdentityBefore.Variant != "minimal" {
		t.Fatalf("the retry must have planned from the minimal state, saw %+v", rj.IdentityBefore)
	}
	e.settle(dev)
	c := e.ctrl[dev]
	if c.Version() != itRelease || c.Image() != "tasmota" || c.OtaURL() != orig {
		t.Fatalf("device ends as %s %s with OtaUrl %q (want %s tasmota %q)", c.Image(), c.Version(), c.OtaURL(), itRelease, orig)
	}
	e.assertNoViolations()
}

// Two devices fail, one is retried: the other keeps its failed job and gets
// no command; the retry belongs to the original batch.
func TestITRetryOfASelection(t *testing.T) {
	e := newItEnv(t)
	d1 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{After: "Upgrade 1", Kind: simulator.FaultDownloadFail}}})
	d2 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{After: "Upgrade 1", Kind: simulator.FaultDownloadFail}}})
	o1, o2 := e.ctrl[d1].OtaURL(), e.ctrl[d2].OtaURL()
	_ = o2
	eng := e.start(2)
	first := e.submit(eng, Request{DeviceID: d1}, Request{DeviceID: d2})
	e.waitTerminal(d1)
	e.waitTerminal(d2)
	for _, d := range []model.DeviceID{d1, d2} {
		if j := e.jobOf(d); j.State != model.JobFailed {
			t.Fatalf("device %d job %s", d, j.State)
		}
		e.settle(d)
	}
	before2 := len(e.ctrl[d2].Commands())
	e.ctrl[d1].ClearFaults()

	res, err := eng.SubmitRetry(context.Background(), "it", first, []model.DeviceID{d1})
	if err != nil || len(res.Devices) != 1 || res.Devices[0] != d1 {
		t.Fatalf("%+v %v", res, err)
	}
	rj := e.waitJob(d1, func(j model.Job) bool { return j.BatchID == res.Batch && j.State.Terminal() }, "terminal")
	if rj.State != model.JobSucceeded {
		t.Fatalf("retry job %s %q", rj.State, rj.LastError)
	}
	e.settle(d1)
	if c := e.ctrl[d1]; c.Version() != itRelease || c.OtaURL() != o1 {
		t.Fatalf("device 1 runs %s with OtaUrl %q", c.Version(), c.OtaURL())
	}
	if j := e.jobOf(d2); j.State != model.JobFailed || j.BatchID != first {
		t.Fatalf("device 2's job changed: %+v", j)
	}
	if after := len(e.ctrl[d2].Commands()); after != before2 {
		t.Fatalf("the unselected device received %d more commands", after-before2)
	}
	// a second selection of a device whose retry succeeded: the original job
	// is still failed, the device is now at target and is skipped
	if _, err := eng.SubmitRetry(context.Background(), "it", first, []model.DeviceID{d1}); err == nil {
		t.Fatal("device 1 is at target now; the retry should report nothing to do")
	}
}
