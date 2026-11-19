package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
)

// answerReader answers every read with the same values and counts the devices it was asked for.
type answerReader struct {
	mu   sync.Mutex
	vals map[string]string
	read []model.DeviceID
}

func (a *answerReader) ReadKeys(_ context.Context, id model.DeviceID, _ []string) (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.read = append(a.read, id)
	return a.vals, nil
}

func (a *answerReader) reads() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.read)
}

// cooldownEnv is a reporter with two rules (SaveData, PowerOnState) on one device and a fixed clock.
func cooldownEnv(devs ...model.Device) (*policyReporter, *answerReader, time.Time) {
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	rd := &answerReader{vals: map[string]string{"SaveData": "0", "PowerOnState": "3"}}
	if len(devs) == 0 {
		devs = []model.Device{{ID: 1, Name: "kitchen"}}
	}
	return &policyReporter{
		rules:   []policy.Rule{{ID: "r-save", Command: "SaveData", Want: "0"}, {ID: "r-pos", Command: "PowerOnState", Want: "3"}},
		devices: fakeLister(devs), backups: fakeBackupSvc{}, reader: rd,
		now: func() time.Time { return now },
	}, rd, now
}

// Why: Run now has to read what a rule judges. An MQTT stat (or a Make compliant verify) of one setting
// is a read of that setting only; counting it as the device's read left SaveData "Not judged" for five minutes
// and "Run now" gave no reason (#989).
func TestRunNowReadsADeviceAnUnrelatedReadLeftWithoutAJudgedValue(t *testing.T) {
	p, rd, now := cooldownEnv()
	p.vals.put(1, map[string]string{"PowerOnState": "3"}, now.Add(-time.Minute))
	rep, err := p.Report(t.Context(), "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if rd.reads() != 1 {
		t.Fatalf("the device must be read: %d reads", rd.reads())
	}
	if _, held := rep.Cooldown[1]; held {
		t.Error("a device that was read is not held back")
	}
	if rep.Provenance[1]["savedata"].Kind != "live" {
		t.Errorf("SaveData must be judged from the read: %+v", rep.Provenance[1]["savedata"])
	}
}

// Why: a check that other reads can push back again and again never arrives. A read that gave only one of
// the judged settings later must not renew the cooldown of the rest (#989).
func TestRunNowPartialReadsDoNotPushTheCheckBack(t *testing.T) {
	p, rd, now := cooldownEnv()
	p.vals.put(1, map[string]string{"SaveData": "0", "PowerOnState": "3"}, now.Add(-6*time.Minute))
	p.vals.put(1, map[string]string{"PowerOnState": "3"}, now.Add(-time.Minute))
	rep, _ := p.Report(t.Context(), "alice", true)
	if rd.reads() != 1 {
		t.Fatalf("SaveData is 6 min old: the device is read, %d reads", rd.reads())
	}
	if len(rep.Cooldown) != 0 {
		t.Errorf("nothing held back: %v", rep.Cooldown)
	}
}

// Why: the cooldown protects the devices from being polled by a button; it stays for a read that is complete.
// The result must say when the next read is possible, so "Not judged" or an unchanged value is not a mystery (#989).
func TestRunNowHoldsBackACompleteRecentReadAndSaysUntilWhen(t *testing.T) {
	p, rd, now := cooldownEnv()
	p.vals.put(1, map[string]string{"SaveData": "0", "PowerOnState": "3"}, now.Add(-2*time.Minute))
	// scoped to another device, disabled and not readable live: none of them is judged on device 1, so none
	// of them (no value for them) may count as a missing value.
	p.rules = append(p.rules,
		policy.Rule{ID: "r-other", Command: "SetOption1", Want: "0", Devices: []string{"attic"}},
		policy.Rule{ID: "r-off", Command: "SetOption2", Want: "0", Disabled: true},
		policy.Rule{ID: "r-reset", Command: "Reset", Want: "0"},
	)
	rep, _ := p.Report(t.Context(), "alice", true)
	if rd.reads() != 0 {
		t.Fatalf("a complete read 2 min ago holds the check back: %d reads", rd.reads())
	}
	if got, want := rep.Cooldown[1], now.Add(3*time.Minute); !got.Equal(want) {
		t.Fatalf("the next read is possible at %v, want %v", got, want)
	}
	if rep.LiveThrottled {
		t.Error("the per-device hold is not the rate limit")
	}
}

// Why: the anchor is the OLDEST judged setting, so a rule added later (no value yet) is read on the next
// Run now instead of waiting out the cooldown of the other settings.
func TestRunNowReadsAgainWhenAJudgedSettingHasNoValue(t *testing.T) {
	p, rd, now := cooldownEnv()
	p.vals.put(1, map[string]string{"SaveData": "0"}, now.Add(-time.Minute))
	rep, _ := p.Report(t.Context(), "alice", true)
	if rd.reads() != 1 || len(rep.Cooldown) != 0 {
		t.Fatalf("PowerOnState has no value: reads %d, held %v", rd.reads(), rep.Cooldown)
	}
}

// Why: a device outside every rule's scope judges nothing; it keeps the old anchor (its newest read), so it
// is not read more often than before.
func TestRunNowDeviceNoRuleJudgesKeepsTheNewestReadAsAnchor(t *testing.T) {
	p, rd, now := cooldownEnv(model.Device{ID: 1, Name: "kitchen"})
	p.rules = []policy.Rule{{ID: "r-other", Command: "SaveData", Want: "0", Devices: []string{"attic"}}}
	p.vals.put(1, map[string]string{"Other": "x"}, now.Add(-time.Minute))
	rep, _ := p.Report(t.Context(), "alice", true)
	if rd.reads() != 0 || !rep.Cooldown[1].Equal(now.Add(4*time.Minute)) {
		t.Fatalf("reads %d, held %v", rd.reads(), rep.Cooldown)
	}
}

// Why: a device on limited firmware is never judged and nothing is sent to it; a device whose API is off is
// left alone on purpose. Saying "read recently" about either would be wrong.
func TestRunNowNeverListsLimitedOrSkippedDevicesAsHeld(t *testing.T) {
	lim := model.Device{ID: 1, Name: "lim", Identity: model.Identity{Variant: "safeboot"}}
	off := model.Device{ID: 2, Name: "off"}
	p, _, now := cooldownEnv(lim, off)
	p.skip = func(id model.DeviceID) bool { return id == 2 }
	for _, id := range []model.DeviceID{1, 2} {
		p.vals.put(id, map[string]string{"SaveData": "0", "PowerOnState": "3"}, now.Add(-time.Minute))
	}
	rep, _ := p.Report(t.Context(), "alice", true)
	if len(rep.Cooldown) != 0 {
		t.Errorf("held %v", rep.Cooldown)
	}
}
