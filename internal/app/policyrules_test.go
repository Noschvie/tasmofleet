package app

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why: the editor's live preview must tell how many devices a rule would flag
// before it is saved, from backups and cached reads only. A preview that read
// devices would let a page view cause fleet traffic.
func TestPolicyPreviewCountsFlaggedDevicesWithoutReadingDevices(t *testing.T) {
	now := time.Now()
	rd := &fakeReader{asked: map[model.DeviceID][]string{}}
	rep := &policyReporter{
		devices: fakeLister{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}, {ID: 3, Name: "c", Tags: []string{"kitchen"}}},
		backups: fakeBackupSvc{
			list: map[model.DeviceID][]model.Backup{
				1: {{ID: 10, Kind: model.BackupDecoded, TakenAt: now}},
				3: {{ID: 11, Kind: model.BackupDecoded, TakenAt: now}},
			},
			data: map[model.BackupID]string{10: `{"save_data":5}`, 11: `{"save_data":0}`},
		},
		reader: rd,
	}
	res, err := rep.Preview(t.Context(), []policy.Rule{
		{Command: "SaveData", Want: "0"},
		{Command: "SaveData", Want: "0", Tags: []string{"kitchen"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.asked) != 0 {
		t.Fatal("a preview must not read devices")
	}
	if res.Devices != 3 || res.NoData != 1 {
		t.Fatalf("devices=%d nodata=%d", res.Devices, res.NoData)
	}
	if res.Rules[0].Flagged != 1 || res.Rules[0].Unknown != 1 { // a deviates; b has no data
		t.Errorf("rule 0: %+v", res.Rules[0])
	}
	if res.Rules[1].Flagged != 0 || res.Rules[1].Unknown != 0 { // only c is in scope and complies
		t.Errorf("rule 1: %+v", res.Rules[1])
	}
}

// Why: the counts on the rule cards must match what the policy page reports.
// With precedence (N3) a device judged by a higher rule is not counted for a
// lower one, and the overlap itself is returned so the page can show it.
func TestPolicyPreviewCountsWithPrecedence(t *testing.T) {
	now := time.Now()
	rep := &policyReporter{
		devices: fakeLister{{ID: 1, Name: "a"}, {ID: 3, Name: "c", Tags: []string{"kitchen"}}},
		backups: fakeBackupSvc{
			list: map[model.DeviceID][]model.Backup{
				1: {{ID: 10, Kind: model.BackupDecoded, TakenAt: now}},
				3: {{ID: 11, Kind: model.BackupDecoded, TakenAt: now}},
			},
			data: map[model.BackupID]string{10: `{"save_data":5}`, 11: `{"save_data":5}`},
		},
		reader: &fakeReader{asked: map[model.DeviceID][]string{}},
	}
	res, err := rep.Preview(t.Context(), []policy.Rule{
		{Command: "SaveData", Want: "5", Tags: []string{"kitchen"}}, // c complies here
		{Command: "SaveData", Want: "0"},                            // a deviates; c is shadowed
		{Command: "SaveData", Want: "1", Disabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rules[0] != (sysconfig.RulePreview{}) || res.Rules[1].Flagged != 1 || res.Rules[2] != (sysconfig.RulePreview{}) {
		t.Fatalf("counts %+v", res.Rules)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Higher != 0 || res.Conflicts[0].Lower != 1 || res.Conflicts[0].Devices != 1 {
		t.Fatalf("conflicts %+v", res.Conflicts)
	}
}

// Why: saved rules must reach the running report without a restart.
func TestPolicyReporterFollowsNewRules(t *testing.T) {
	rep := &policyReporter{devices: fakeLister{{ID: 1, Name: "a"}}, backups: fakeBackupSvc{}, reader: &fakeReader{asked: map[model.DeviceID][]string{}}}
	r, _ := rep.Report(t.Context(), "x", false)
	if len(r.Rules) != 0 {
		t.Fatal("no rules yet")
	}
	rep.setRules([]policy.Rule{{Command: "SaveData", Want: "0"}})
	r, _ = rep.Report(t.Context(), "x", false)
	if len(r.Rules) != 1 || len(r.Findings) != 1 {
		t.Fatalf("%+v", r)
	}
}

// Why (N3): a disabled rule flags nothing, so the live check must not spend
// device reads on a setting only a disabled rule names.
func TestRuleCommandsSkipDisabledRules(t *testing.T) {
	got := ruleCommands([]policy.Rule{
		{Command: "SaveData", Want: "0"},
		{Command: "TelePeriod", Want: "300", Disabled: true},
	})
	if len(got) != 1 || got[0] != "SaveData" {
		t.Fatalf("commands %v", got)
	}
}
