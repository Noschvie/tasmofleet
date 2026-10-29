package ota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// sbAPI adds the safeboot capability to the push fake.
type sbAPI struct {
	*pushAPI
	layout    tasmota.UpLayout
	layoutErr error
	reads     atomic.Int32
}

func (s *sbAPI) UpLayout(context.Context, model.Device) (tasmota.UpLayout, error) {
	s.reads.Add(1)
	return s.layout, s.layoutErr
}

func (s *sbAPI) SwitchBoot(context.Context, model.Device, tasmota.BootTarget) (tasmota.BootReply, error) {
	return "", errors.New("the route tests never switch")
}

// bigImageDevice is an ESP32 whose Free (400 KB) is smaller than the 469152-byte test image.
func bigImageDevice() *fakeDevice {
	d := esp32Device()
	d.ident.FreeKB = 400
	return d
}

func sbHarness(t *testing.T, fd *fakeDevice, layout tasmota.UpLayout, layoutErr error, open bool) (*harness, *sbAPI) {
	t.Helper()
	h, pa, pf := pushHarness(t, fd)
	api := &sbAPI{pushAPI: pa, layout: layout, layoutErr: layoutErr}
	e := NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Power: h.power}, h.st, h.st, api, pf, h.backups, h.clk, nil)
	e.sel = h.sel
	e.safebootPush = open
	h.sel.esp32Direct = true
	h.e = e
	return h, api
}

// Why: the route is chosen only for an ESP32 whose image does not fit its running partition AND whose
// /up asks for the safeboot detour, and only with the gate open. Every other case keeps the behaviour
// of before #30, with the reason in the note; the read of /up is not made when it cannot matter.
func TestPreviewSafebootRoute(t *testing.T) {
	for _, tc := range []struct {
		name      string
		free      int
		layout    tasmota.UpLayout
		layoutErr error
		open      bool
		wantVia   bool
		wantDeliv model.Delivery
		wantNote  string // substring
		wantReads int32
	}{
		{"safeboot layout, gate open", 400, tasmota.UpLayoutSafeboot, nil, true, true, model.DeliveryPush, "", 1},
		{"safeboot layout, gate closed", 400, tasmota.UpLayoutSafeboot, nil, false, false, model.DeliveryPull, "push into safeboot is not enabled yet", 1},
		{"dual layout does not fit", 400, tasmota.UpLayoutDirect, nil, true, false, model.DeliveryPull, "not enough free flash for an upload", 1},
		{"layout unreadable", 400, "", errors.New("timeout"), true, false, model.DeliveryPull, "not enough free flash for an upload", 1},
		{"image fits: plain push, /up not read", 1800, tasmota.UpLayoutSafeboot, nil, true, false, model.DeliveryPush, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := esp32Device()
			fd.ident.FreeKB = tc.free
			h, api := sbHarness(t, fd, tc.layout, tc.layoutErr, tc.open)
			items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush}})
			if err != nil || len(items) != 1 || items[0].Decision == decisionRefuse {
				t.Fatalf("preview %+v, %v", items, err)
			}
			it := items[0]
			if it.ViaSafeboot != tc.wantVia || it.Delivery != tc.wantDeliv || !strings.Contains(it.DeliveryNote, tc.wantNote) || (tc.wantNote == "" && it.DeliveryNote != "") {
				t.Errorf("item %+v", it)
			}
			if got := api.reads.Load(); got != tc.wantReads {
				t.Errorf("/up read %d times, want %d", got, tc.wantReads)
			}
		})
	}
}

// Why: a pull request never asks /up, and a device API without the capability (a fake, an old client)
// keeps today's fallback instead of failing.
func TestPreviewWithoutSafebootCapabilityKeepsTheFallback(t *testing.T) {
	h, _, _ := pushHarness(t, bigImageDevice())
	h.sel.esp32Direct = true
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush}})
	if err != nil || len(items) != 1 || items[0].ViaSafeboot || items[0].Delivery != model.DeliveryPull ||
		!strings.Contains(items[0].DeliveryNote, "not enough free flash") {
		t.Fatalf("preview %+v, %v", items, err)
	}
	h2, api := sbHarness(t, bigImageDevice(), tasmota.UpLayoutSafeboot, nil, true)
	if _, err := h2.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if api.reads.Load() != 0 {
		t.Errorf("a download request read /up")
	}
}

// Why: the route chosen in the preview is stored on the job, so a job resumed after a restart knows
// whether a restart into safeboot is part of it.
func TestSubmitStoresTheSafebootRoute(t *testing.T) {
	h, _ := sbHarness(t, bigImageDevice(), tasmota.UpLayoutSafeboot, nil, true)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	j := h.job(t, id)
	if !j.ViaSafeboot || j.Delivery != model.DeliveryPush || j.State != model.JobQueued {
		t.Fatalf("job %+v", j)
	}
}

// Why: if the gate is closed by the time the job plans (a new build), the job must not quietly turn
// into a download or run without the detour it was submitted with: it stops and says so.
func TestPlanRefusesWhenTheRouteChangedAfterSubmit(t *testing.T) {
	h, api := sbHarness(t, bigImageDevice(), tasmota.UpLayoutSafeboot, nil, true)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", Delivery: model.DeliveryPush})
	api.layout = tasmota.UpLayoutDirect // the page no longer asks for safeboot
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "planned as a push upload") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if len(api.uploads) != 0 {
		t.Fatalf("uploaded: %v", api.uploads)
	}
}

// Why: the browser's own script takes the safeboot route only for a plain image (first byte 0xE9);
// the check runs before the device is restarted, so an unusable file never costs a restart.
func TestPushCheckInsistsOnAPlainImageForSafeboot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first byte
		via   bool
		ok    bool
	}{
		{"plain image via safeboot", 0xE9, true, true},
		{"gzip via safeboot", 0x1F, true, false},
		{"gzip direct", 0x1F, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := pushHarness(t, esp32Device())
			e := h.e
			e.fw = firstByteFW{pushFW: pushFW{fakeFW: h.fw, rec: h.rec}, first: tc.first}
			err := e.pushCheck(context.Background(), model.Artifact{Filename: "tasmota32.bin", Size: 100}, tc.via)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !isRefusal(err) {
				t.Errorf("err %v must be a refusal", err)
			}
		})
	}
}

// firstByteFW hands out an image that starts with the given byte.
type firstByteFW struct {
	pushFW
	first byte
}

func (f firstByteFW) Open(_ context.Context, a model.Artifact) (io.ReadCloser, error) {
	b := make([]byte, a.Size)
	b[0] = f.first
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Why: an image that cannot be opened at upload time was never sent. That is certain, so a push
// into safeboot goes straight to its recovery instead of probing a device that never got a byte
// for the whole upgrade window.
func TestPushOpenFailureIsCertainNothingWasSent(t *testing.T) {
	h, pa, pf := pushHarness(t, esp32Device())
	pf.openErr = errors.New("cache file gone")
	h.e.fw = pf
	ev := h.e.push(context.Background(), model.Device{ID: 1}, model.Artifact{Filename: "tasmota32.bin", Size: 10})
	if ev.Kind != EvCommandFailed || !ev.NotFlashed || !strings.Contains(ev.Err, "cache file gone") {
		t.Fatalf("event %+v", ev)
	}
	if len(pa.uploads) != 0 {
		t.Fatalf("uploaded: %v", pa.uploads)
	}
	// and the machine recovers a safeboot job on it, at once
	j := sbJob(model.JobUpgradeIssued)
	if out, _ := Step(j, ev, t0, testCfg()); out.State != model.JobSafebootRecover {
		t.Fatalf("state %s", out.State)
	}
}
