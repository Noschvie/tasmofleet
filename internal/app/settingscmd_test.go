package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why: automated setups drive the settings with a file. It must round-trip, never
// export a secret, and refuse a file with any invalid or unknown entry as a whole.
func TestSettingsExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	a := importServices(t)
	if _, err := a.Sys.Apply(ctx, sysconfig.Request{Actor: "t", StepUp: sysconfig.StepUp{Done: true, Possible: true},
		Values: map[string]string{"BACKUP_KEEP": "20", "MQTT_URL": "tcp://broker.lan:1883", "MQTT_PASSWORD": "fake-cli-secret"}}); err != nil {
		t.Fatal(err)
	}
	rules := newRulesService(a, nil, nil)
	if _, err := rules.Save(ctx, "t", "", []sysconfig.RuleSpec{{Command: "SaveData", Want: "0", Name: "Flash"}}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ExportSettings(ctx, a, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "fake-cli-secret") || strings.Contains(out.String(), "MQTT_PASSWORD") {
		t.Fatalf("a secret was exported:\n%s", out.String())
	}

	b := importServices(t)
	var log bytes.Buffer
	if err := ImportSettings(ctx, b, strings.NewReader(out.String()), &log); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if c := b.Sys.Snapshot(); c.BackupKeep != 20 || c.MQTTURL != "tcp://broker.lan:1883" {
		t.Fatalf("keep=%d url=%s", c.BackupKeep, c.MQTTURL)
	}
	if st, _ := newRulesService(b, nil, nil).State(ctx); len(st.Specs) != 1 || st.Specs[0].Name != "Flash" {
		t.Fatalf("rules %+v", st.Specs)
	}

	for name, bad := range map[string]string{
		"unknown key":   `{"version":1,"settings":{"AUTH_MODE":"local"}}`,
		"bootstrap key": `{"version":1,"settings":{"LISTEN":":1"}}`,
		"invalid value": `{"version":1,"settings":{"BACKUP_KEEP":"20","OTA_PARALLEL":"0"}}`,
		"wrong version": `{"version":2,"settings":{}}`,
		"extra field":   `{"version":1,"settings":{},"admin":true}`,
		"bad rules":     `{"version":1,"settings":{"BACKUP_KEEP":"30"},"policy_rules":[{"command":"MqttPassword","want":"x"}]}`,
	} {
		c := importServices(t)
		if err := ImportSettings(ctx, c, strings.NewReader(bad), &log); err == nil {
			t.Errorf("%s accepted", name)
		}
		if c.Sys.Snapshot().BackupKeep != 10 {
			t.Errorf("%s: a refused file must change nothing", name)
		}
	}
}
