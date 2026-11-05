package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

func onFirmware(id model.DeviceID, name, variant string) model.Device {
	return model.Device{ID: id, Name: name, Identity: model.Identity{Family: model.ChipESP32, Variant: variant}}
}

// Why (#558): a device on the safeboot image or the minimal firmware answers "Unknown" to the rule
// commands; asking it fills its log and its MQTT topic with errors every hour. The policy reads
// (hourly poller and on-demand check) must send nothing to it, and the device must read as "not
// judged" with the firmware named, even when an older live value or backup would have judged it.
// Once its identity reports regular firmware it is read again.
func TestPolicyReadsSkipSafebootAndMinimalAndResume(t *testing.T) {
	now := time.Now()
	rd := &fakeReader{asked: map[model.DeviceID][]string{}}
	devs := fakeLister{onFirmware(1, "regular", "tasmota"), onFirmware(2, "safe", "safeboot"), onFirmware(4, "mini", "minimal")}
	rep := &policyReporter{
		rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, devices: devs, reader: rd,
		backups: fakeBackupSvc{}, now: func() time.Time { return now },
	}
	// a value read before the device fell back to safeboot must not judge it now
	rep.vals.put(2, map[string]string{"SaveData": "1"}, now.Add(-time.Hour))

	if rep.refresh(t.Context(), "x", devs) { // throttled
		t.Fatal("the first on-demand run must not be throttled")
	}
	if _, asked := rd.asked[1]; !asked {
		t.Fatal("the regular device must be read")
	}
	for _, id := range []model.DeviceID{2, 4} {
		if keys, asked := rd.asked[id]; asked {
			t.Errorf("device %d runs safeboot/minimal firmware and must receive nothing, got %v", id, keys)
		}
	}

	r, err := rep.Report(t.Context(), "x", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.DeviceID]policy.OutcomeKind{}
	for _, o := range r.Outcomes {
		got[o.DeviceID] = o.Kind
	}
	if got[1] != policy.OutcomeMatch || got[2] != policy.OutcomeUnknown || got[4] != policy.OutcomeUnknown {
		t.Errorf("outcomes %v: regular must match, safeboot and minimal must be not judged", got)
	}

	// the poller skips them too
	clock := now
	rep.now = func() time.Time { return clock }
	rd.asked = map[model.DeviceID][]string{}
	p := &policyPoller{rep: rep, interval: func() time.Duration { return time.Hour }, jitter: func() float64 { return 0 }, offline: func(model.DeviceID) bool { return false }}
	p.once(t.Context())
	clock = clock.Add(11 * time.Minute)
	p.once(t.Context())
	if _, asked := rd.asked[2]; asked {
		t.Error("the poller must not read a safeboot device")
	}
	if _, asked := rd.asked[1]; !asked {
		t.Error("the poller must still read the regular device")
	}
	if due := p.due[2].Sub(clock); due > pollRetryAfter {
		t.Errorf("a skipped device is looked at again in %v, want at most %v so it resumes soon after a flash", due, pollRetryAfter)
	}

	// the device reports regular firmware: the next read includes it
	devs[1].Identity.Variant = "tasmota"
	clock = clock.Add(pollRetryAfter + time.Minute)
	p.once(t.Context())
	if keys := rd.asked[2]; len(keys) == 0 {
		t.Error("a device that reports regular firmware must be read again")
	}
	r, _ = rep.Report(t.Context(), "x", false)
	for _, o := range r.Outcomes {
		if o.DeviceID == 2 && o.Kind != policy.OutcomeMatch {
			t.Errorf("after the read the device is judged, got kind %v", o.Kind)
		}
	}
}

// Why (#558): "Copy from a device" is a person's click, but the answer of a safeboot device is
// "Unknown": refuse up front with the firmware named instead of an error from the device.
func TestReadValueRefusesSafebootAndMinimal(t *testing.T) {
	for _, variant := range []string{"safeboot", "minimal"} {
		rd := &fakeReader{asked: map[model.DeviceID][]string{}}
		rep := &policyReporter{devices: fakeLister{onFirmware(2, "d", variant)}, reader: rd, backups: fakeBackupSvc{}}
		_, _, err := rep.ReadValue(t.Context(), "x", 2, "SaveData")
		if err == nil || !strings.Contains(err.Error(), variant) {
			t.Errorf("%s: want a refusal naming the firmware, got %v", variant, err)
		}
		if len(rd.asked) != 0 {
			t.Errorf("%s: nothing may be sent, got %v", variant, rd.asked)
		}
	}
}

// Why (#558): the background Status 1 (GroupTopic) and Status 5/11 (network facts) reads are as
// unanswerable on safeboot and minimal firmware as the policy reads; nothing may be sent, and the
// group topic reader must not keep a stuck device in its retry loop.
func TestBackgroundReadsSendNothingToSafebootAndMinimal(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{
		{ID: 1, Topic: "a", Address: "192.0.2.1", Identity: model.Identity{Variant: "safeboot"}},
		{ID: 2, Topic: "b", Address: "192.0.2.2", Identity: model.Identity{Variant: "minimal"}},
	}}
	c := &gtClient{}
	r := newGroupTopicReader(st.list, online, c, st, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	for range 4 {
		r.tick(t.Context())
	}
	if len(c.asked) != 0 {
		t.Errorf("group topic reader asked %v", c.asked)
	}
	p := &Prober{Devices: getOnly{g: st}, Client: c}
	for _, id := range []model.DeviceID{1, 2} {
		if err := p.ReadNet(t.Context(), id); err != nil {
			t.Errorf("device %d: %v", id, err)
		}
	}
	if len(c.asked) != 0 {
		t.Errorf("ReadNet asked %v", c.asked)
	}
	// regular firmware: read again
	st.devs[0].Identity.Variant = "tasmota"
	now = now.Add(24 * time.Hour) // past the reader's idle pause
	r.tick(t.Context())
	if len(c.asked) != 1 || c.asked[0] != 1 {
		t.Errorf("a device on regular firmware must be read again, asked %v", c.asked)
	}
}

// getOnly serves GetDevice from a gtStore; any other store call panics on the nil interface
// (ReadNet must return before it writes).
type getOnly struct {
	identityStore
	g *gtStore
}

func (s getOnly) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	return s.g.GetDevice(ctx, id)
}
