package app

import (
	"context"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"log/slog"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
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
	fw := newFirmwareState(nil)
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

// Why (#558, M-1): the health poll's "minimal" finding is evidence with a time like an INFO1 or a
// probe. Without the time it won over everything: a device that was fixed while minimal (TasmoFleet
// restarted meanwhile, "Refresh identity" says regular) stayed skipped for ever, and the engine's
// pre-update backup refused it.
func TestHealthMinimalIsEvidenceWithATime(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	var polled time.Time
	fw := newFirmwareState(func(model.DeviceID) time.Time { return polled })
	dev := func(detected time.Time) model.Device {
		return model.Device{ID: 1, Identity: model.Identity{Variant: "tasmota", DetectedAt: detected}}
	}
	variant := func(d model.Device) string { return fw.effective(d).Identity.Variant }

	polled = t1
	if got := variant(dev(t0)); got != "minimal" {
		t.Errorf("a poll after the probe found minimal firmware: %q", got)
	}
	if got := variant(dev(t2)); got != "tasmota" {
		t.Errorf("a probe after the poll stored regular firmware and must win: %q", got)
	}
	fw.set(1, "", t2)
	if got := variant(dev(t0)); got != "tasmota" {
		t.Errorf("a regular INFO1 after the poll must win: %q", got)
	}
	polled = time.Time{} // the last poll found regular firmware
	fw2 := newFirmwareState(func(model.DeviceID) time.Time { return polled })
	if got := fw2.effective(dev(t0)).Identity.Variant; got != "tasmota" {
		t.Errorf("no minimal finding: %q", got)
	}
	// a limited INFO1 newer than the poll still counts
	polled = t1
	fw.set(1, "safeboot", t2.Add(time.Minute))
	if got := variant(dev(t0)); got != "safeboot" {
		t.Errorf("a newer limited INFO1: %q", got)
	}
}

// Why (#558, #560): the health poll cannot tell safeboot from minimal (both answer Status 11 with
// Unknown and know Upgrade). A poll newer than the INFO1 or probe that named safeboot must not rename
// the device: the policy row, the backup reason and the template refusal name the firmware.
func TestHealthMinimalKeepsTheSafebootName(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Minute), t0.Add(2*time.Minute)
	polled := t1
	fw := newFirmwareState(func(model.DeviceID) time.Time { return polled })
	dev := func(variant string, fam model.ChipFamily) model.Device {
		return model.Device{ID: 1, Identity: model.Identity{Family: fam, Variant: variant, DetectedAt: t0}}
	}
	variant := func(d model.Device) string { return fw.effective(d).Identity.Variant }
	if got := variant(dev("safeboot", model.ChipUnknown)); got != "safeboot" { // what a probe in safeboot stores
		t.Errorf("a probe named safeboot, a newer poll: %q", got)
	}
	if got := variant(dev("tasmota", model.ChipESP32)); got != "safeboot" {
		t.Errorf("an ESP32 on limited firmware runs safeboot: %q", got)
	}
	if got := variant(dev("tasmota", model.ChipESP8266)); got != "minimal" {
		t.Errorf("an ESP8266 on limited firmware runs minimal: %q", got)
	}
	fw.set(1, "safeboot", t1)
	polled = t2
	if got := variant(dev("tasmota", model.ChipUnknown)); got != "safeboot" {
		t.Errorf("an INFO1 named safeboot, a newer poll: %q", got)
	}
}

// Why (#558, M-1): the failure was real: a regular device that health calls minimal is refreshed once,
// is regular from then on, and is not asked again an hour later.
func TestFWRecheckFixesADeviceHealthCallsMinimal(t *testing.T) {
	p, st, _, _ := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	polled := time.Now() // after the stored identity
	fw := newFirmwareState(func(model.DeviceID) time.Time { return polled })
	now := time.Now()
	n := 0
	rc := newFWRecheck(fw.list(st.ListDevices), func(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} },
		func(ctx context.Context, id model.DeviceID) error { n++; return p.Refresh(ctx, id, "") }, slog.New(slog.DiscardHandler))
	rc.now = func() time.Time { return now }
	devs, _ := fw.list(st.ListDevices)(t.Context())
	if !policy.LimitedFirmware(devs[0].Identity.Variant) {
		t.Fatal("setup: health says minimal, so the device is limited")
	}
	rc.tick(t.Context())
	devs, _ = fw.list(st.ListDevices)(t.Context())
	if n != 1 || policy.LimitedFirmware(devs[0].Identity.Variant) {
		t.Fatalf("refreshed %d times, effective variant %q: the probe is newer than the poll and must win", n, devs[0].Identity.Variant)
	}
	now = now.Add(2 * time.Hour)
	rc.tick(t.Context())
	if n != 1 {
		t.Errorf("a recovered device is not asked again, %d refreshes", n)
	}
}

// Why (#558, M-2): during an update the device passes minimal (ESP8266) or safeboot (ESP32) on purpose.
// A refresh then would write identities mid-job and bind MACs the job does not expect. A busy device is
// skipped without a try being recorded, so it is looked at as soon as the job is over.
func TestFWRecheckLeavesABusyDeviceAlone(t *testing.T) {
	now := time.Now()
	devs := []model.Device{{ID: 1, Identity: model.Identity{Variant: "minimal"}}}
	busy := true
	var refreshed []model.DeviceID
	rc := newFWRecheck(func(context.Context) ([]model.Device, error) { return devs, nil },
		func(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} },
		func(_ context.Context, id model.DeviceID) error { refreshed = append(refreshed, id); return nil }, slog.New(slog.DiscardHandler))
	rc.now = func() time.Time { return now }
	rc.busy = func(context.Context, model.DeviceID) bool { return busy }
	rc.tick(t.Context())
	if len(refreshed) != 0 || len(rc.tried) != 0 {
		t.Fatalf("a device in an update was refreshed %v or marked tried %v", refreshed, rc.tried)
	}
	busy = false
	now = now.Add(time.Minute)
	rc.tick(t.Context())
	if len(refreshed) != 1 {
		t.Errorf("after the job the device must be looked at at once, refreshed %v", refreshed)
	}
}

// Why (#558): an INFO1 that names limited firmware after the last try means the device booted into it
// (or something forged the message). It makes the device due again once that try is 10 minutes old, not
// only after the hour; one without a newer INFO1 stays on the hourly rhythm.
func TestFWRecheckAsksSoonerAfterANewLimitedInfo1(t *testing.T) {
	now := time.Now()
	devs := []model.Device{{ID: 1, Identity: model.Identity{Variant: "minimal"}}}
	var sighted time.Time
	n := 0
	rc := newFWRecheck(func(context.Context) ([]model.Device, error) { return devs, nil },
		func(model.DeviceID) model.Health { return model.Health{State: model.HealthOnline} },
		func(context.Context, model.DeviceID) error { n++; return nil }, slog.New(slog.DiscardHandler))
	rc.now = func() time.Time { return now }
	rc.sighted = func(model.DeviceID) time.Time { return sighted }
	rc.tick(t.Context())
	now = now.Add(5 * time.Minute)
	sighted = now
	rc.tick(t.Context())
	if n != 1 {
		t.Fatalf("a new INFO1 5 minutes after the try must not make it due yet, %d refreshes", n)
	}
	now = now.Add(6 * time.Minute)
	rc.tick(t.Context())
	if n != 2 {
		t.Errorf("a new INFO1 and a try 11 minutes old: due, %d refreshes", n)
	}
	now = now.Add(20 * time.Minute) // no INFO1 since the last try
	rc.tick(t.Context())
	if n != 2 {
		t.Errorf("without a newer INFO1 it stays hourly, %d refreshes", n)
	}
}

// Why (#558): the wiring is where this feature silently disappears (a reader that is built but never
// asked, a store that is not wrapped). The recheck must be kept away from a device a typed firmware
// command or an update job is working on, and must see live INFO1 sightings; the policy report must
// get its devices through the effective view.
func TestRuntimeWiresTheFirmwareState(t *testing.T) {
	svc := importServices(t)
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context(), fw: newFirmwareState(nil),
		fwHolds: devlock.NewFirmwareHolds(time.Now, time.Hour)}
	rc := a.newFWRecheck()
	if rc.busy == nil || rc.sighted == nil {
		t.Fatal("the recheck must know about busy devices and INFO1 sightings")
	}
	if rc.busy(t.Context(), 99) {
		t.Error("a device without a job or hold is not busy")
	}
	a.fwHolds.Start(7, 5)
	if !rc.busy(t.Context(), 7) {
		t.Error("a device a typed firmware command still updates is busy")
	}
	withJob := func(name, addr string, to model.JobState) model.DeviceID { // a device whose job is in state to
		id, err := svc.Store.CreateDevice(t.Context(), model.Device{Name: name, Address: addr})
		if err != nil {
			t.Fatal(err)
		}
		j := model.Job{DeviceID: id, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
		_, ids, err := svc.Store.CreateBatch(t.Context(), "u", []model.Job{j})
		if err != nil {
			t.Fatal(err)
		}
		j.ID, j.State = ids[0], to
		if err := svc.Store.SaveTransition(t.Context(), j, model.JobEvent{JobID: ids[0], From: model.JobQueued, To: to, Event: "test"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if !rc.busy(t.Context(), withJob("updating", "192.0.2.1", model.JobAwaitReboot)) {
		t.Error("a device an update job is working on is busy")
	}
	if rc.busy(t.Context(), withJob("updated", "192.0.2.2", model.JobSucceeded)) {
		t.Error("a device whose update job has ended is not busy")
	}
	a.fw.set(5, "safeboot", time.Now())
	if rc.sighted(5).IsZero() {
		t.Error("the recheck must see the INFO1 sightings of the firmware state")
	}
	if err := a.setupSettings(); err != nil {
		t.Fatal(err)
	}
	if v, ok := a.policyLive.devices.(fwDevices); !ok || v.fw != a.fw {
		t.Errorf("the policy report reads devices through %T, want the effective view of the runtime's state", a.policyLive.devices)
	}
}

type fakeHealth struct {
	minimal map[model.DeviceID]bool
	seen    time.Time
}

func (f fakeHealth) Minimal(id model.DeviceID) bool  { return f.minimal[id] }
func (f fakeHealth) Get(model.DeviceID) model.Health { return model.Health{SeenHTTP: f.seen} }

// Why (#558, M-1): the firmware state takes the time of the poll only when that poll found minimal
// firmware; a poll that found regular firmware must give no evidence at all, or a recovered device
// would be skipped for ever.
func TestMinimalSeenAtIsTheTimeOfAPollThatFoundMinimal(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := minimalSeenAt(fakeHealth{minimal: map[model.DeviceID]bool{1: true}, seen: at})
	if got := f(1); !got.Equal(at) {
		t.Errorf("minimal: %v, want the time of the poll %v", got, at)
	}
	if got := f(2); !got.IsZero() {
		t.Errorf("regular: %v, want zero", got)
	}
}
