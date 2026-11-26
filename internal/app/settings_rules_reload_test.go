package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// keysReader answers every read with the same values and remembers which settings were asked for.
type keysReader struct {
	mu    sync.Mutex
	vals  map[string]string
	asked [][]string
}

func (k *keysReader) ReadKeys(_ context.Context, _ model.DeviceID, keys []string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, slices.Clone(keys))
	return k.vals, nil
}

func (k *keysReader) askedFor(cmd string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.ContainsFunc(k.asked, func(keys []string) bool { return slices.Contains(keys, cmd) })
}

// importRules stores a rules file the way the CLI does: another process, the server's reporter is not told.
func importRules(t *testing.T, svc *Services, rulesJSON string) {
	t.Helper()
	file := `{"version":1,"settings":{},"policy_rules":` + rulesJSON + `}`
	if err := ImportSettings(t.Context(), svc, strings.NewReader(file), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
}

const (
	ruleSaveData    = `{"command":"SaveData","expected":"0","level":"required"}`
	ruleSetOption21 = `{"command":"SetOption21","expected":"0","level":"required"}`
)

// reloadEnv is the server's reporter over a real store, as setupSettings builds it, with one device, a fixed
// clock and rule A (SaveData) stored and loaded; the device was read for A a minute ago.
func reloadEnv(t *testing.T) (*policyReporter, *Services, *keysReader, time.Time) {
	t.Helper()
	svc := importServices(t)
	importRules(t, svc, `[`+ruleSaveData+`]`)
	now := time.Date(2026, 11, 26, 19, 15, 0, 0, time.UTC)
	rd := &keysReader{vals: map[string]string{"SaveData": "0", "SetOption21": "0"}}
	rules := newRulesService(svc, nil, nil)
	rep := &policyReporter{
		devices: fakeLister{{ID: 1, Name: "kitchen"}}, backups: fakeBackupSvc{}, reader: rd,
		now:  func() time.Time { return now },
		load: func(ctx context.Context) (ruleSet, error) { return storedRuleSet(ctx, rules) },
	}
	rep.reloadRules(t.Context()) // startup
	rep.vals.put(1, map[string]string{"SaveData": "0"}, now.Add(-time.Minute))
	return rep, svc, rd, now
}

// Why (#1309): a rule a person adds must be judged at the next Run now, whichever tool stored it. The CLI
// (tasmofleet settings import) stores a version in its own process; the running server kept the old rules,
// so Run now judged the cooldown with them: the device was held as "read recently", the new setting was never
// asked, and the page, which lists the stored rules, showed the new rule as "?" for every device.
func TestRunNowJudgesARuleImportedByTheCLIWhileTheServerRuns(t *testing.T) {
	rep, svc, rd, now := reloadEnv(t)
	importRules(t, svc, `[`+ruleSaveData+`,`+ruleSetOption21+`]`)

	r, err := rep.Report(t.Context(), "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.askedFor("SetOption21") {
		t.Errorf("the new rule's setting was never read: %v", rd.asked)
	}
	if _, held := r.Cooldown[1]; held {
		t.Errorf("a device with no value for a judged setting is not held back: %v", r.Cooldown)
	}
	if got := r.Provenance[1]["setoption21"]; got.Kind != "live" || !got.At.Equal(now) {
		t.Errorf("the new rule must be judged from the read just made: %+v", got)
	}
}

// Why (#1309): the background reads and the MQTT updates keep the live values the page judges. They must ask
// for the settings of the stored rules, or a rule stored by another process stays "?" until a restart.
func TestPolicyPollerReadsTheCommandsOfTheStoredRules(t *testing.T) {
	rep, svc, rd, now := reloadEnv(t)
	p := &policyPoller{rep: rep, interval: func() time.Duration { return time.Hour }, jitter: func() float64 { return 0 },
		offline: func(model.DeviceID) bool { return false }}
	p.once(t.Context()) // first sight: schedules the device
	importRules(t, svc, `[`+ruleSaveData+`,`+ruleSetOption21+`]`)
	rep.now = func() time.Time { return now.Add(time.Minute) }
	p.once(t.Context())
	if !rd.askedFor("SetOption21") {
		t.Fatalf("the poll read only what the old rules name: %v", rd.asked)
	}
}

// Why (#1309): a rule that is removed or disabled elsewhere must stop costing device reads, like it does after
// an edit in the editor.
func TestPolicyPollerStopsReadingARuleRemovedByTheCLI(t *testing.T) {
	rep, svc, rd, now := reloadEnv(t)
	importRules(t, svc, `[`+ruleSaveData+`,`+ruleSetOption21+`]`)
	rep.reloadRules(t.Context()) // the server holds both rules
	p := &policyPoller{rep: rep, interval: func() time.Duration { return time.Hour }, jitter: func() float64 { return 0 },
		offline: func(model.DeviceID) bool { return false }}
	p.once(t.Context())
	importRules(t, svc, `[`+ruleSaveData+`]`)
	rep.now = func() time.Time { return now.Add(time.Minute) }
	p.once(t.Context())
	if len(rd.asked) != 1 || slices.Contains(rd.asked[0], "SetOption21") {
		t.Fatalf("the removed rule is still read: %v", rd.asked)
	}
}

// Why (#1309): a failed load must never empty the rule set. No rules would make the fleet look compliant;
// the rules last loaded stay in use until a load succeeds again.
func TestReportKeepsTheLastLoadedRulesWhenTheStoreFails(t *testing.T) {
	rep, svc, rd, _ := reloadEnv(t)
	importRules(t, svc, `[`+ruleSaveData+`,`+ruleSetOption21+`]`)
	if _, err := rep.Report(t.Context(), "alice", false); err != nil { // any page view loads B
		t.Fatal(err)
	}
	inner := rep.load
	down := true
	rep.load = func(ctx context.Context) (ruleSet, error) {
		if down {
			return ruleSet{}, errors.New("database is locked")
		}
		return inner(ctx)
	}
	rep.vals.put(1, map[string]string{"SaveData": "0"}, time.Time{}) // no longer fresh: the device is read
	if _, err := rep.Report(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	if !rd.askedFor("SetOption21") {
		t.Fatalf("the rule loaded earlier was dropped by a failed load: %v", rd.asked)
	}
	down = false
	importRules(t, svc, `[`+ruleSaveData+`]`)
	if _, err := rep.Report(t.Context(), "alice", false); err != nil {
		t.Fatal(err)
	}
	if cmds := ruleCommands(rep.currentRules()); !slices.Equal(cmds, []string{"SaveData"}) {
		t.Fatalf("a load that works again must replace the rules: %v", cmds)
	}
}

// Why (#1309): one stored rule that no longer validates must not hide the others. The valid ones are judged
// after a reload, as they are at startup.
func TestReloadJudgesTheValidRulesOfAVersionWithAnInvalidOne(t *testing.T) {
	rep, svc, _, _ := reloadEnv(t)
	if _, err := svc.Store.AddPolicyRuleVersion(t.Context(), store.PolicyRuleVersion{Actor: "test",
		Rules: `[{"command":"SaveData","want":"0"},{"command":"MqttPassword","want":"x"},{"command":"SetOption21","want":"0"}]`}); err != nil {
		t.Fatal(err)
	}
	if _, err := rep.Report(t.Context(), "alice", false); err != nil {
		t.Fatal(err)
	}
	if cmds := ruleCommands(rep.currentRules()); !slices.Equal(cmds, []string{"SaveData", "SetOption21"}) {
		t.Fatalf("the valid rules must apply: %v", cmds)
	}
}

// Why (#1309): every other test here builds its own reporter and sets load itself, so none of them notices
// when setupSettings stops wiring the store into the real one. That line also carries the startup load: without
// it the server starts with no rules after every restart and the whole fleet looks compliant (fail open),
// and a version another process stores never reaches the running server again. The e2e specs seed their rules
// through the legacy import and the editor's OnChange, which never touch this path.
func TestSetupSettingsLoadsTheStoredRulesAtStartAndReloadsAVersionTheCLIStores(t *testing.T) {
	svc := importServices(t)
	importRules(t, svc, `[`+ruleSaveData+`]`) // stored before the server starts: the restart case
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context(), fw: newFirmwareState(nil),
		fwHolds: devlock.NewFirmwareHolds(time.Now, time.Hour)}
	if err := a.setupSettings(); err != nil {
		t.Fatal(err)
	}
	if got := a.policyLive.currentRules(); len(got) != 1 || got[0].Command != "SaveData" {
		t.Fatalf("the server must start with the stored rules, got %+v", got)
	}

	importRules(t, svc, `[`+ruleSaveData+`,`+ruleSetOption21+`]`) // the CLI path: the server is not told
	if _, err := a.policyLive.Report(t.Context(), "alice", false); err != nil {
		t.Fatal(err)
	}
	if got := a.policyLive.currentRules(); len(got) != 2 || got[1].Command != "SetOption21" {
		t.Errorf("a version stored by another process must reach the running server, got %+v", got)
	}
}
