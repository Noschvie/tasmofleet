package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/store"
)

// withoutFirmwareURL builds the transaction engine the way a start without FIRMWARE_BASE_URL does (no
// firmware cache) and returns the firmware the engine was handed.
func withoutFirmwareURL(t *testing.T, o anotherDevice) (*runtime, ota.Firmware) {
	t.Helper()
	var handed ota.Firmware
	prev := newOTAEngine
	newOTAEngine = func(op ota.Options, j store.JobStore, d store.DeviceStore, api ota.DeviceAPI, fw ota.Firmware, b ota.Backups, c clock.Clock, n ota.Notifier) *ota.Engine {
		handed = fw
		return prev(op, j, d, api, fw, b, c, n)
	}
	t.Cleanup(func() { newOTAEngine = prev })
	bg, stop := context.WithCancel(context.Background())
	a := &runtime{bgContext: bg, log: slog.Default(), svc: &Services{Client: o.cl, Store: o.st}} // a.cache stays nil
	t.Cleanup(func() { stop(); a.bgWG.Wait() })
	a.setupOTA()
	return a, handed
}

// Why (#881): the engine runs every transaction (bulk backup, Send command, Restart, Make compliant), and
// only an update needs a firmware file. Without FIRMWARE_BASE_URL there is no cache; the engine must still
// exist (the web layer registers the bulk backup from it), and its firmware must refuse every file, so
// that no OtaUrl or Upgrade can be planned without a server the devices could download from.
func TestTransactionEngineRunsWithoutFirmwareBaseURL(t *testing.T) {
	a, fw := withoutFirmwareURL(t, newAnotherDevice(t))
	if a.engine == nil {
		t.Fatal("no engine without a firmware cache: bulk backup, commands and Make compliant have nothing to run on")
	}
	if _, err := fw.Ensure(t.Context(), "15.1.0", "tasmota.bin.gz"); !errors.Is(err, errFirmwareOff) {
		t.Errorf("Ensure without a firmware server: %v, want %v", err, errFirmwareOff)
	}
	if _, ok := fw.(ota.CustomFirmware); ok {
		t.Error("an uploaded image must not be served without a firmware server")
	}
	if _, ok := fw.(ota.FirmwareFiles); ok {
		t.Error("a push upload must not open an image without a firmware cache")
	}
}

// Why (#881): the web layer builds the bulk backup, the command batches and the transaction pages from
// web.Deps.OTA and BackupBatch, and tells "updates exist" by Deps.Firmware. A start without the firmware
// URL must hand over the first and leave the second nil (a typed nil in the interface would switch the
// update wizard on again).
func TestWebDepsWithoutFirmwareBaseURLHaveTransactionsButNoUpdates(t *testing.T) {
	a, _ := withoutFirmwareURL(t, newAnotherDevice(t))
	deps := a.webDeps(nil, nil)
	if deps.BackupBatch == nil || deps.OTA == nil {
		t.Errorf("without a firmware base URL: BackupBatch %v, OTA %v; the bulk backup route and the transaction pages need both", deps.BackupBatch, deps.OTA)
	}
	if deps.Firmware != nil {
		t.Errorf("Firmware %T is set without a firmware cache: the UI would offer updates that cannot run", deps.Firmware)
	}
}

// Why (#881): an update must never start without a firmware server. With firmwareOff the plan fails before
// the guard and before any command: the request is refused with the reason, no job is created, and the
// device saw only the read-only probes (no OtaUrl, no Upgrade, safety rule 1).
func TestUpdateIsRefusedWithoutFirmwareBaseURLAndSendsNothing(t *testing.T) {
	o := newAnotherDevice(t)
	a, _ := withoutFirmwareURL(t, o)
	if a.engine == nil {
		t.Fatal("no engine without a firmware cache")
	}
	_, err := a.engine.Submit(t.Context(), "test", []ota.Request{{DeviceID: o.id, Release: "99.0.0", Delivery: model.DeliveryPull}})
	var refused *ota.RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), errFirmwareOff.Error()) {
		t.Fatalf("Submit: %v, want a refusal naming %q", err, errFirmwareOff)
	}
	if len(o.sim.Commands()) == 0 {
		t.Fatal("the device saw no probe at all: the rig would pass for any code")
	}
	for _, c := range o.sim.Commands() {
		if l := strings.ToLower(c); strings.HasPrefix(l, "otaurl ") || strings.HasPrefix(l, "upgrade") {
			t.Errorf("the device received %q", c)
		}
	}
	if jobs, _ := o.st.ListJobs(t.Context(), store.JobFilter{DeviceID: o.id}); len(jobs) != 0 {
		t.Errorf("%d jobs were created for a refused update", len(jobs))
	}
}
