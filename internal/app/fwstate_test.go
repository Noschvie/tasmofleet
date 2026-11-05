package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why (#558): the reporter's device announces "16.0.0(safeboot)single-core". The tag decides, not
// the version, the chip or what precedes it ("release-", a commit hash), so a new Tasmota release
// or a custom build is still recognised and a regular build is never mistaken for one.
func TestInfo1BuildNamesSafebootAndMinimalWhateverTheVersion(t *testing.T) {
	for _, tc := range []struct {
		version, variant string
		known            bool
	}{
		{"16.0.0(safeboot)single-core", "safeboot", true}, // the bare string of #558
		{"16.0.0(safeboot)", "safeboot", true},
		{"15.0.1(release-minimal)", "minimal", true},
		{"17.2.0(a1b2c3d-safeboot)dual-core", "safeboot", true},
		{"15.0.1(tasmota-minimal)", "minimal", true},
		{"16.0.0(tasmota32)", "", true},
		{"15.0.1(release-tasmota)", "", true},
		{"15.0.1(release-sensors)", "", true},
		{"15.0.1", "", false},
		{"", "", false},
	} {
		got, known := info1Build([]byte(`{"Info1":{"Module":"x","Version":"` + tc.version + `","FallbackTopic":"cmnd/x_fb/"}}`))
		if got != tc.variant || known != tc.known {
			t.Errorf("%q: variant %q known %v, want %q %v", tc.version, got, known, tc.variant, tc.known)
		}
	}
	if _, known := info1Build([]byte(`{"StatusFWR":{}}`)); known {
		t.Error("a message without Info1 says nothing")
	}
}

// Why (#558): the stored variant changes only on a probe, so what an INFO1 or the health poll saw
// is kept next to it, and the newest of them decides. A device that boots into safeboot is limited
// at once; one that leaves it is read again at once; a probe that ran later beats an older sighting.
func TestFirmwareStateNewestEvidenceWins(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	minimal := false
	fw := newFirmwareState(func(model.DeviceID) bool { return minimal })
	dev := func(variant string) model.Device {
		return model.Device{ID: 1, Identity: model.Identity{Variant: variant, DetectedAt: t0}}
	}
	variant := func(d model.Device) string { return fw.effective(d).Identity.Variant }

	if variant(dev("tasmota")) != "tasmota" || variant(dev("minimal")) != "minimal" {
		t.Fatal("with nothing seen the stored variant stands")
	}
	fw.set(1, "safeboot", t0.Add(time.Minute))
	if variant(dev("tasmota")) != "safeboot" {
		t.Error("an INFO1 after the probe: the device booted into safeboot (the reporter's case)")
	}
	fw.set(1, "", t0.Add(2*time.Minute))
	if variant(dev("tasmota")) != "tasmota" || variant(dev("minimal")) != "tasmota" || variant(dev("safeboot")) != "tasmota" {
		t.Error("a regular INFO1 after the probe ends the limited state, whatever was stored")
	}
	fw.set(1, "safeboot", t0.Add(time.Minute)) // a late, older sighting must not undo the newer one
	if variant(dev("minimal")) != "tasmota" {
		t.Error("an older sighting overwrote a newer one")
	}
	later := dev("minimal")
	later.Identity.DetectedAt = t0.Add(time.Hour) // a probe after every sighting
	if variant(later) != "minimal" {
		t.Error("an identity read after the last sighting decides")
	}
	minimal = true
	if variant(dev("tasmota")) != "tasmota" { // the regular INFO1 at +2m is newer than t0, so it stands
		t.Error("the health poll must not override a newer sighting")
	}
	if v := (*firmwareState)(nil).effective(dev("minimal")).Identity.Variant; v != "minimal" {
		t.Error("no state: the device as stored")
	}
	fresh := newFirmwareState(func(model.DeviceID) bool { return true })
	if fresh.effective(dev("tasmota")).Identity.Variant != "minimal" {
		t.Error("the health poll found minimal firmware over HTTP")
	}
}

// Why (#558): INFO1 is how a device that was identified as regular tells that it runs safeboot now.
// It must be noticed even though the hardware is known (applyInfo1 ignores INFO1 then), and a
// retained message is no reading of now.
func TestLiveInfo1RecordsSafebootAndRegular(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	now := t0.Add(time.Hour)
	d := model.Device{ID: 4, Topic: "plug", Identity: model.Identity{Hardware: "ESP32-D0WD", Variant: "tasmota32", DetectedAt: t0}}
	fw := newFirmwareState(nil)
	m := &mqttIdentity{list: func(context.Context) ([]model.Device, error) { return []model.Device{d}, nil }, fw: fw,
		now: func() time.Time { return now }, log: slog.New(slog.DiscardHandler)}
	info := func(version string, retained bool) mqttbus.Message {
		return mqttbus.Message{Kind: mqttbus.KindInfo, Topic: "plug", Suffix: "INFO1", Retained: retained,
			Payload: []byte(`{"Info1":{"Module":"x","Version":"` + version + `"}}`)}
	}
	m.handle(t.Context(), info("16.0.0(safeboot)single-core", true))
	if got := fw.effective(d).Identity.Variant; got != "tasmota32" {
		t.Errorf("a retained INFO1 is no reading of now, variant %q", got)
	}
	m.handle(t.Context(), info("16.0.0(safeboot)single-core", false))
	if got := fw.effective(d).Identity.Variant; got != "safeboot" {
		t.Errorf("variant %q after the INFO1 of a device that booted into safeboot", got)
	}
	now = now.Add(time.Minute)
	m.handle(t.Context(), info("16.0.0(tasmota32)", false))
	if got := fw.effective(d).Identity.Variant; got != "tasmota32" {
		t.Errorf("variant %q after the INFO1 of regular firmware", got)
	}
}

// Why (#558): once a device is on limited firmware nothing reads it, and the stored identity changes
// only on a probe. This reader is the way back: it runs the ordinary identity refresh about once an
// hour for such a device, so a device that left minimal firmware outside an update job (Tasmota's
// own fallback, a manual upload) is read again; one that is still on it costs one read per hour; a
// device on regular firmware is never touched.
func TestFWRecheckBringsADeviceBackAndLeavesTheRestAlone(t *testing.T) {
	p, st, id, _ := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	d, _ := st.GetDevice(t.Context(), id)
	ident := d.Identity
	ident.Variant, ident.DetectedAt = "minimal", time.Now() // stored as minimal, but the device runs regular firmware
	if err := st.UpdateProbe(t.Context(), id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, store.ProbeResult{Identity: ident}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var refreshed []model.DeviceID
	rc := newFWRecheck(st.ListDevices, func(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} },
		func(ctx context.Context, id model.DeviceID) error {
			refreshed = append(refreshed, id)
			return p.Refresh(ctx, id, "")
		}, slog.New(slog.DiscardHandler))
	rc.now = func() time.Time { return now }

	rc.tick(t.Context())
	got, _ := st.GetDevice(t.Context(), id)
	if len(refreshed) != 1 || got.Identity.Variant == "minimal" || policy.LimitedFirmware(got.Identity.Variant) {
		t.Fatalf("refreshed %v, variant now %q: the device runs regular firmware and must be recognised", refreshed, got.Identity.Variant)
	}
	// and it is read for the policy again
	rd := &fakeReader{asked: map[model.DeviceID][]string{}}
	rep := &policyReporter{rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, reader: rd, devices: fakeLister{got}, backups: fakeBackupSvc{}}
	rep.readDevice(t.Context(), got)
	if len(rd.asked[id]) == 0 {
		t.Error("a device back on regular firmware must be read by the policy again")
	}
	for range 3 {
		now = now.Add(2 * time.Hour)
		rc.tick(t.Context())
	}
	if len(refreshed) != 1 {
		t.Errorf("a device on regular firmware must never be touched again, refreshed %v", refreshed)
	}
}

func TestFWRecheckAsksADeviceStillOnMinimalOncePerHour(t *testing.T) {
	p, st, id, _ := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266Min})
	d, _ := st.GetDevice(t.Context(), id)
	if d.Identity.Variant != "minimal" {
		t.Fatalf("setup: variant %q", d.Identity.Variant)
	}
	now := time.Now()
	n := 0
	rc := newFWRecheck(st.ListDevices, func(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} },
		func(ctx context.Context, id model.DeviceID) error { n++; return p.Refresh(ctx, id, "") }, slog.New(slog.DiscardHandler))
	rc.now = func() time.Time { return now }
	rc.tick(t.Context())
	now = now.Add(30 * time.Minute)
	rc.tick(t.Context())
	if n != 1 {
		t.Errorf("%d refreshes within the hour, want 1", n)
	}
	now = now.Add(31 * time.Minute)
	rc.tick(t.Context())
	if n != 2 {
		t.Errorf("%d refreshes after the hour, want 2", n)
	}
	if got, _ := st.GetDevice(t.Context(), id); got.Identity.Variant != "minimal" {
		t.Errorf("a device still on minimal firmware stays minimal, got %q", got.Identity.Variant)
	}
	// offline devices are not asked
	rc.health = func(model.DeviceID) model.Health { return model.Health{State: model.HealthOffline} }
	now = now.Add(3 * time.Hour)
	rc.tick(t.Context())
	if n != 2 {
		t.Error("an offline device must not be asked")
	}
}

// Why (#558): the consumers get the devices through the effective view, so a device that booted
// into safeboot after its last probe is skipped by the policy reads and shows as not judged, with no
// change to the stored identity.
func TestPolicyReadsFollowTheLiveFirmwareState(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stored := fakeLister{{ID: 1, Name: "booted-into-safeboot", Identity: model.Identity{Family: model.ChipESP32, Variant: "tasmota32", DetectedAt: t0}}}
	fw := newFirmwareState(nil)
	rd := &fakeReader{asked: map[model.DeviceID][]string{}}
	rep := &policyReporter{rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, reader: rd, backups: fakeBackupSvc{},
		devices: fwDevices{inner: storeOf{stored}, fw: fw}}
	fw.set(1, "safeboot", t0.Add(time.Minute))
	devs, _ := rep.devices.ListDevices(t.Context())
	rep.refresh(t.Context(), "x", devs)
	if len(rd.asked) != 0 {
		t.Fatalf("sent to a device whose INFO1 says safeboot: %v", rd.asked)
	}
	r, _ := rep.Report(t.Context(), "x", false)
	if len(r.Outcomes) != 1 || r.Outcomes[0].Kind != policy.OutcomeUnknown {
		t.Errorf("outcomes %+v, want not judged", r.Outcomes)
	}
	fw.set(1, "", t0.Add(2*time.Minute))
	rep.lastRun = time.Time{}
	devs, _ = rep.devices.ListDevices(t.Context())
	rep.refresh(t.Context(), "x", devs)
	if len(rd.asked[1]) == 0 {
		t.Error("read again as soon as an INFO1 shows regular firmware")
	}
}

type storeOf struct{ fakeLister }

func (s storeOf) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	for _, d := range s.fakeLister {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, store.ErrNotFound
}
