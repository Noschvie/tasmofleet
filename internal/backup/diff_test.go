package backup

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadDecoded(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/synthetic/decoded.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// replaceOnce edits the real decoded output so tests change one setting
// without hand-writing a 300-key document.
func replaceOnce(t *testing.T, doc []byte, old, repl string) []byte {
	t.Helper()
	s := string(doc)
	if strings.Count(s, old) != 1 {
		t.Fatalf("fixture drifted: %q occurs %d times", old, strings.Count(s, old))
	}
	return []byte(strings.Replace(s, old, repl, 1))
}

// Why: the diff page must never become a second place where secrets leak
// (spec 12.3). Every secret in the real decoder output is changed here and
// none of the old or new values may appear anywhere in the result.
func TestDiffNeverCarriesSecretValues(t *testing.T) {
	a := loadDecoded(t)
	b := a
	for old, repl := range map[string]string{
		"synthetic-wifi-pass-0001": "NEW-wifi-secret-A",
		"synthetic-mqtt-pass-0002": "NEW-mqtt-secret-B",
		"synthetic-web-pass-0003":  "NEW-web-secret-C",
	} {
		b = replaceOnce(t, b, old, repl)
	}
	changes, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sta_pwd[0]": false, "mqtt_pwd": false, "web_password": false}
	for _, c := range changes {
		if !c.Secret {
			t.Errorf("unexpected non-secret change %s", c.Path)
			continue
		}
		if c.Old != "" || c.New != "" {
			t.Errorf("%s: secret row carries values %q -> %q", c.Path, c.Old, c.New)
		}
		want[c.Path] = true
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("secret change at %s not reported", p)
		}
	}
	flat := fmtChanges(changes)
	for _, leak := range []string{"synthetic-wifi-pass", "synthetic-mqtt-pass", "synthetic-web-pass", "NEW-"} {
		if strings.Contains(flat, leak) {
			t.Errorf("diff output contains %q", leak)
		}
	}
}

func fmtChanges(cs []Change) string {
	var sb strings.Builder
	for _, c := range cs {
		sb.WriteString(c.Path + "|" + c.Old + "|" + c.New + "\n")
	}
	return sb.String()
}

// Why: identical backups must give an empty diff, otherwise "nothing
// changed" can never be shown and restore verification could not pass.
func TestDiffIdenticalIsEmpty(t *testing.T) {
	a := loadDecoded(t)
	changes, err := Diff(a, a)
	if err != nil || len(changes) != 0 {
		t.Fatalf("got %v, %v", changes, err)
	}
}

// Why: the diff is for humans reviewing a drift; plain settings show old and
// new, arrays are addressed by index and the order is stable so two renders
// of the same pair look the same.
func TestDiffPlainValuesArraysAndOrder(t *testing.T) {
	a := loadDecoded(t)
	b := replaceOnce(t, a, `"tele_period": 300`, `"tele_period": 60`)
	b = replaceOnce(t, b, `"Synthetic Plug"`, `"Kitchen Plug"`)
	changes, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes: %v", len(changes), changes)
	}
	if changes[0].Path != "friendlyname[0]" || changes[0].Old != `"Synthetic Plug"` || changes[0].New != `"Kitchen Plug"` {
		t.Errorf("first change: %+v", changes[0])
	}
	if changes[1].Path != "tele_period" || changes[1].Old != "300" || changes[1].New != "60" || changes[1].Secret {
		t.Errorf("second change: %+v", changes[1])
	}
}

// Why: a key that exists on one side only (a setting new in a newer
// firmware) must show up, and an absent value must differ from an empty one.
func TestDiffAddedRemovedAndEmptyString(t *testing.T) {
	changes, err := Diff([]byte(`{"a":"","b":1}`), []byte(`{"b":1,"c":2,"a":null}`))
	if err != nil {
		t.Fatal(err)
	}
	got := fmtChanges(changes)
	if got != "a|\"\"|null\nc||2\n" {
		t.Errorf("got %q", got)
	}
}

// Why: a removed or added secret still must not reveal its value, and the
// secret rule applies to the whole subtree below a secret key.
func TestDiffSecretSubtreeAndOneSidedSecret(t *testing.T) {
	changes, err := Diff([]byte(`{"api_key":{"x":"s1"},"n":1}`), []byte(`{"token":"t","n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %v", changes)
	}
	for _, c := range changes {
		if !c.Secret || c.Old != "" || c.New != "" {
			t.Errorf("not redacted: %+v", c)
		}
	}
}

// Why: URL-shaped values can carry credentials (OtaUrl, webhook URLs); the
// userinfo must not reach the page even though the key name looks harmless,
// and a changed URL password must still be reported as a change.
func TestDiffRedactsURLCredentials(t *testing.T) {
	changes, err := Diff([]byte(`{"ota_url":"http://u:p1@h/a"}`), []byte(`{"ota_url":"http://u:p2@h/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Secret {
		t.Fatalf("got %+v", changes)
	}
	if flat := fmtChanges(changes); strings.Contains(flat, "p1") || strings.Contains(flat, "p2") || strings.Contains(flat, "http") {
		t.Errorf("got %q", flat)
	}
}

// Why: the secret rule is the spec 12.3 regex plus the audit redaction keys;
// "pwd" is added because decode-config names Wi-Fi/MQTT passwords sta_pwd and
// mqtt_pwd, which the spec regex alone would miss.
func TestSecretKeyRule(t *testing.T) {
	for _, k := range []string{"sta_pwd", "mqtt_pwd", "web_password", "Password", "passwd", "influxdb_token", "api_key", "client_secret", "zb_precfgkey_h", "http_password", "ssid_password"} {
		if !secretKey(k) {
			t.Errorf("%q should be secret", k)
		}
	}
	for _, k := range []string{"friendlyname", "sta_ssid", "mqtt_host", "tele_period", "mqtt_user"} {
		if secretKey(k) {
			t.Errorf("%q should not be secret", k)
		}
	}
}

func TestDiffRejectsInvalidJSON(t *testing.T) {
	if _, err := Diff([]byte(`{`), []byte(`{}`)); err == nil {
		t.Error("expected an error for invalid first document")
	}
	if _, err := Diff([]byte(`{}`), []byte(`nope`)); err == nil {
		t.Error("expected an error for invalid second document")
	}
}

// Why: after a restore the device rewrites counters and checksums; these must
// not make a successful restore look like a failure, but a real setting must.
func TestFilterVolatile(t *testing.T) {
	in := []Change{
		{Path: "header.data.crc"}, {Path: "cfg_crc"}, {Path: "bootcount"}, {Path: "save_flag"},
		{Path: "friendlyname[0]"}, {Path: "tele_period"},
	}
	out := FilterVolatile(in)
	if len(out) != 2 || out[0].Path != "friendlyname[0]" || out[1].Path != "tele_period" {
		t.Errorf("got %v", out)
	}
}

// Why: drift compares the stored snapshot with a live one; clocks, uptime and
// memory change every second and would drown real drift.
func TestSnapshotDrift(t *testing.T) {
	a := []byte(`{"Status1":{"StatusPRM":{"Uptime":"1T01:00:00","BootCount":3}},"Status2":{"StatusFWR":{"Version":"15.0.0"}},"Status11":{"StatusSTS":{"UptimeSec":5}},"Status6":{"StatusMQT":{"MqttHost":"a","MqttPassword":"x"}}}`)
	b := []byte(`{"Status1":{"StatusPRM":{"Uptime":"2T01:00:00","BootCount":4}},"Status2":{"StatusFWR":{"Version":"16.0.0"}},"Status11":{"StatusSTS":{"UptimeSec":9}},"Status6":{"StatusMQT":{"MqttHost":"b","MqttPassword":"y"}}}`)
	changes, err := SnapshotDrift(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got := fmtChanges(changes)
	want := "Status2.StatusFWR.Version|\"15.0.0\"|\"16.0.0\"\nStatus6.StatusMQT.MqttHost|\"a\"|\"b\"\nStatus6.StatusMQT.MqttPassword||\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if !changes[2].Secret {
		t.Error("MqttPassword must be flagged secret")
	}
}

// The drift view's "Show all" lists every compared setting, the unchanged ones marked Same, so
// a reader sees what was checked and not only what differs. Volatile readings stay out (they
// change by themselves) and a secret stays blank even when it did not change.
func TestSnapshotCompareListsUnchangedSettings(t *testing.T) {
	a := []byte(`{"Status1":{"StatusPRM":{"Uptime":"1T01:00:00"}},"Status3":{"StatusLOG":{"SerialLog":0}},"Status6":{"StatusMQT":{"MqttHost":"a","MqttPassword":"x"}}}`)
	b := []byte(`{"Status1":{"StatusPRM":{"Uptime":"2T01:00:00"}},"Status3":{"StatusLOG":{"SerialLog":0}},"Status6":{"StatusMQT":{"MqttHost":"b","MqttPassword":"x"}}}`)
	all, err := SnapshotCompare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range all {
		got[c.Path] = c
	}
	if len(all) != 3 {
		t.Fatalf("want MqttHost, MqttPassword and SerialLog, got %+v", all)
	}
	if c := got["Status3.StatusLOG.SerialLog"]; !c.Same || c.Old != "0" || c.New != "0" {
		t.Errorf("an unchanged setting is listed with its value and Same: %+v", c)
	}
	if c := got["Status6.StatusMQT.MqttHost"]; c.Same {
		t.Errorf("a changed setting is not Same: %+v", c)
	}
	if c := got["Status6.StatusMQT.MqttPassword"]; !c.Secret || c.Old != "" || c.New != "" {
		t.Errorf("an unchanged secret is still never shown: %+v", c)
	}
	drift, err := SnapshotDrift(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || drift[0].Path != "Status6.StatusMQT.MqttHost" {
		t.Errorf("SnapshotDrift keeps listing the changes only: %+v", drift)
	}
}

// Every credential shape the security review probed (M1), each with a fake
// secret. Policy: a value that carries a credential is never shown, not even
// scrubbed, and a change of the value is reported as a secret row.
var credentialProbes = []string{
	"http://user:p@ss@host/x",
	"http://user:pa/ss@host/x",
	"http://user:pa ss@host/x",
	"WebSend [admin:p@ss@10.0.0.5] Power",
	"WebSend [fakeuser:FAKE-PW-1@192.0.2.1] Power ON",
	`password="SECRET 16 with space"`,
	"/api?auth_token=FAKE-PW-2",
	"mqtt_password=FAKE-PW-3",
	"api_token=FAKE-PW-4",
	"private_key=FAKE-PW-5",
	"/api?sig=FAKE-PW-6",
	"X-API-Key: FAKE-PW-7",
	`{"access_token":"FAKE-PW-8"}`,
	`{"api-key":"FAKE-PW-9"}`,
	`{"password": 12345}`,
	"RgxPassword FAKE-PW-10",
	"TelegramToken FAKE-PW-11",
	"InfluxDbPassword FAKE-PW-12",
	"WebQuery https://api.telegram.org/bot123456:ABCdefGhIJKlmNoPQRsTUVwxyZ/sendMessage",
	"https://hooks.slack.com/services/T0001/B0002/FAKEPW13",
	"https://discord.com/api/webhooks/123456789/FAKE-PW-14",
	"Bearer abc12",
	"curl -H 'Authorization: Basic RkFLRS1QVy00OnNlY3JldA==' http://h.invalid",
	"Backlog Password1 FAKE-PW-15; Restart 1",
	"WebPassword FAKE-PW-16",
	"http://h.invalid/?apikey=FAKE-PW-17&x=1",
	"http://h.invalid/?access_token=FAKE-PW-18",
	"http://fakeuser:FAKE-PW-19@host.invalid/x",
}

// Why: M1. Operators see every non-body string, so each probe shape must turn
// the row into a secret row with no value, whichever key it sits under.
func TestDiffNeverShowsValuesThatCarryCredentials(t *testing.T) {
	for i, f := range credentialProbes {
		enc := func(suffix string) []byte {
			b, _ := json.Marshal(map[string][]string{"friendlyname": {f + suffix}})
			return b
		}
		changes, err := Diff(enc(" old"), enc(" new"))
		if err != nil || len(changes) != 1 {
			t.Fatalf("probe %d: %v %v", i, changes, err)
		}
		c := changes[0]
		if !c.Secret || c.Old != "" || c.New != "" {
			t.Errorf("probe %d %q is shown: %+v", i, f, c)
		}
	}
}

// Why: a harmless text must stay readable; the net must not swallow everything.
func TestDiffKeepsHarmlessText(t *testing.T) {
	changes, _ := Diff([]byte(`{"friendlyname":["Kitchen Plug"],"ntp_server":["pool.ntp.org"]}`), []byte(`{"friendlyname":["Hall Plug"],"ntp_server":["pool.ntp.org"]}`))
	if len(changes) != 1 || changes[0].Secret || changes[0].Old != `"Kitchen Plug"` || changes[0].New != `"Hall Plug"` {
		t.Fatalf("got %+v", changes)
	}
}

// Why: M2 and L6. Mem1..16 hold the API keys rules use and rf_code holds
// replayable RF codes: admin-only like rules, hidden from operators.
func TestMemsAndRFCodesAreBodies(t *testing.T) {
	changes, _ := Diff([]byte(`{"mems":["APIKEY_OLD",""],"rf_code":[["0x01"]],"tele_period":1}`), []byte(`{"mems":["APIKEY_NEW",""],"rf_code":[["0x02"]],"tele_period":2}`))
	if len(changes) != 3 {
		t.Fatalf("got %+v", changes)
	}
	hidden := HideBodies(changes)
	for _, c := range hidden {
		if strings.Contains(c.Old+c.New, "APIKEY") || strings.Contains(c.Old+c.New, "0x0") {
			t.Errorf("operator view leaks: %+v", c)
		}
	}
	for _, c := range changes {
		if c.Path != "tele_period" && !c.Body {
			t.Errorf("%s is not a body: %+v", c.Path, c)
		}
	}
}

// Why: when two URLs differ only in the credential, the diff must not show
// "which part changed" even indirectly.
func TestDiffCredentialOnlyChangeIsSecretRow(t *testing.T) {
	changes, _ := Diff([]byte(`{"x":"http://u:FAKE-A@h/p?token=FAKE-C"}`), []byte(`{"x":"http://u:FAKE-B@h/p?token=FAKE-D"}`))
	if len(changes) != 1 || !changes[0].Secret || changes[0].Old != "" || changes[0].New != "" {
		t.Fatalf("got %+v", changes)
	}
}

// Why: rules and scripts may hold secrets in shapes no pattern knows; the
// diff keeps them for admins and shows operators only that the text changed.
func TestHideBodies(t *testing.T) {
	changes, _ := Diff([]byte(`{"rules":["on power1#state do x endon",""],"tele_period":300}`), []byte(`{"rules":["on power1#state do y endon",""],"tele_period":60}`))
	if len(changes) != 2 || !changes[0].Body || changes[1].Body {
		t.Fatalf("got %+v", changes)
	}
	if changes[0].Old == "" {
		t.Error("admins get the body")
	}
	hidden := HideBodies(changes)
	if hidden[0].Old != "" || hidden[0].New != "" || !hidden[0].Hidden || hidden[1].New != "60" || hidden[1].Hidden {
		t.Errorf("hidden: %+v", hidden)
	}
	if changes[0].Old == "" {
		t.Error("HideBodies must not modify its input")
	}
}

// Why: the explicit list is the primary defence; a decode-config key that the
// fallback regex would not catch (telegram_chatid, keeloq_master_*) must still
// be redacted, and the Tasmota typo "pwassword" must be caught by the regex
// even without the list.
func TestExplicitSecretFieldsAndFallback(t *testing.T) {
	for _, k := range SecretFields() {
		changes, err := Diff([]byte(`{"`+k+`":"FAKE-OLD"}`), []byte(`{"`+k+`":"FAKE-NEW"}`))
		if err != nil || len(changes) != 1 || !changes[0].Secret || changes[0].Old != "" {
			t.Errorf("%s: %+v %v", k, changes, err)
		}
	}
	for _, k := range []string{"rgx_pwassword", "rgx_pwassword_esp32", "mqtt_fingerprint1", "some_auth_blob", "cloud_credential"} {
		if !secretKey(k) {
			t.Errorf("fallback regex misses %q", k)
		}
	}
}

// Why: this is the leak test the review asked for. Every secret field of
// decode-config carries a fake value in the synthetic dump; none of those
// values (text, numbers, hex keys, fingerprints, credentials inside a URL or
// a rule) may appear in any change row, for operators or admins.
func TestDiffAgainstDumpWithEverySecretFieldSet(t *testing.T) {
	base := loadDecoded(t)
	sec, err := os.ReadFile("testdata/synthetic/decoded-secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Diff(base, sec)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Change{}
	for _, c := range changes {
		seen[c.Path] = c
	}
	for _, f := range SecretFields() {
		top := false
		for p := range seen {
			if p == f || strings.HasPrefix(p, f+"[") {
				top = true
				if !seen[p].Secret {
					t.Errorf("%s changed but is not a secret row", p)
				}
			}
		}
		// rgx_pwassword_esp32 does not exist in this ESP8266 dump
		if !top && !strings.HasSuffix(f, "_esp32") {
			t.Errorf("secret field %s not reported as changed", f)
		}
	}
	for _, view := range [][]Change{changes, HideBodies(changes)} {
		flat := fmtChanges(view)
		for _, leak := range []string{"FAKE-SECRET", "4242", "4343", "777001", "777002", "0x5ec7", "0x6d6d", "0x7a7a", `"0xaa"`, `"0xbb"`, "ouser", "otapw"} {
			if strings.Contains(flat, leak) {
				t.Errorf("diff output contains %q", leak)
			}
		}
	}
	if r := seen["rules[0]"]; !r.Body || !r.Secret || r.New != "" {
		t.Errorf("a rule that carries a credential is a secret row: %+v", r)
	}
	if r := seen["ota_url"]; !r.Secret || r.New != "" {
		t.Errorf("a URL that carries a credential is a secret row: %+v", r)
	}
}

// Why: "Show all N settings" on the comparison lists unchanged settings too,
// and it must not become the place where an unchanged secret is shown in
// full (an unchanged Wi-Fi password is still a password). The changed rows
// must be exactly Diff's, so the "4 of 17 settings differ" count and the
// tinted rows agree.
func TestCompareListsEverySettingButNoSecretValue(t *testing.T) {
	a := loadDecoded(t)
	b := replaceOnce(t, a, "synthetic-mqtt-pass-0002", "NEW-mqtt-secret-B")
	all, err := Compare(a, b)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	var diff []Change
	same := 0
	for _, c := range all {
		if c.Same {
			same++
			if c.Old != c.New {
				t.Errorf("%s: marked same but %q != %q", c.Path, c.Old, c.New)
			}
			continue
		}
		diff = append(diff, c)
	}
	if fmtChanges(diff) != fmtChanges(changed) {
		t.Errorf("changed rows of Compare differ from Diff:\n%s\nwant\n%s", fmtChanges(diff), fmtChanges(changed))
	}
	if same == 0 || len(all) != same+len(changed) {
		t.Errorf("Compare returned %d rows, %d same, %d changed", len(all), same, len(changed))
	}
	flat := fmtChanges(all)
	for _, leak := range []string{"synthetic-wifi-pass", "synthetic-mqtt-pass", "synthetic-web-pass", "NEW-"} {
		if strings.Contains(flat, leak) {
			t.Errorf("full comparison contains %q", leak)
		}
	}
}
