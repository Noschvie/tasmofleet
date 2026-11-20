package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/tasmotacmd"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#606): a policy is stored, exported and shown in the clear (the rule editor, the report, the preview), and
// Make compliant sends its lines unasked, now over MQTT first, where the broker hands them to every client
// subscribed to the device's topics. Masking is no transport rule (a secret is no reason for HTTP, the product
// owner's rule of 2026-11-14): Make compliant must never build a secret line in the first place. A policy line
// that sets a password, a Wi-Fi name, a token, a key or a URL with a login, or that smuggles one in behind a "; ",
// is refused before a single command is sent. Every line of the table is one cmdpolicy keeps secret (it redacts it,
// or the audit does not show it in full), so the table cannot go stale into lines that prove nothing. Beyond the
// table, no line a policy can send (makeCompliantLines) is one cmdpolicy redacts or that carries a credential the
// reply scrub would mask, and every secret setting of the form is refused.
func TestMakeCompliantNeverSendsASecret(t *testing.T) {
	for _, line := range []string{
		"MqttPassword x", "WebPassword x", "WebPassword2 x", "Password1 x", "SSId1 net", "TmToken x",
		"RgxPassword x", "RgxSSId net", "IfxPassword x", "LoRaWanAppKey 00", "WiFiTest1 net",
		"MI32Key 0123456789abcdef0123456789abcdef", "MI32Keys 0123456789abcdef", "ZbConfig 1",
		"OtaUrl http://u:p@h/fw.bin", "OtaUrl https://tok@h/fw.bin",
		"LedState 1; WebPassword x", "TelePeriod 60; OtaUrl http://u:p@h/fw.bin",
	} {
		v, err := cmdpolicy.Classify(line)
		secret := err != nil || v.Redact || !cmdpolicy.ShownInFull(line)
		if !secret {
			t.Fatalf("fixture: %q is no secret for cmdpolicy", line)
		}
		c := &fakeCmd{}
		err = newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, []string{"PowerOnState 0", line})
		if err == nil || len(c.sent) != 0 {
			t.Errorf("%q: err %v, sent %q", line, err, c.sent)
		}
	}
	for _, l := range makeCompliantLines(t) {
		// The one exception: the broker login. It is secret by the one rule since #809 (its value never reaches a
		// log, a reply or a diff), but a fleet policy may set it (#606, the security check of PR #816; #947 reads it
		// back): the device hands it to the broker on every connect. Every other secret stays refused.
		if l.key == "MqttUser" && !carriesCredential(t, l.text) {
			continue
		}
		if v, err := cmdpolicy.Classify(l.text); err != nil || v.Redact || carriesCredential(t, l.text) {
			t.Errorf("a policy can send %q, which cmdpolicy keeps secret", l.text)
		}
	}
	for _, f := range settings.Fields {
		if !f.Secret {
			continue
		}
		if txt, err := CommandText(f.Key, "x"); err == nil {
			t.Errorf("secret setting %s: a policy can send %q", f.Key, txt)
		}
		if commandProblem(f.Key, "x") == nil {
			t.Errorf("secret setting %s: a policy rule may name it", f.Key)
		}
	}
}

// carriesCredential reports whether text holds a credential the reply scrub masks in free text (a URL's userinfo,
// password=, a WebSend login): the one rule cmdpolicy keeps for credentials inside a command's text.
func carriesCredential(t *testing.T, text string) bool {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"Line": text})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(cmdpolicy.ScrubReply(raw), &got); err != nil {
		t.Fatal(err)
	}
	return got["Line"] != text
}

// Why (#606): Tasmota reads the digits at the end of a command name as its index and runs the command without
// them, so "Topic1 plug" and "MqttPort1 plug" set the topic and the port like "Topic plug" and "MqttPort plug".
// Taken as a generic rule, such a spelling would skip the setting's validation (MqttPort1 plug would be sent) and
// its restart (settings.RestartsDevice knows Topic, not Topic1): Make compliant would put a line that restarts the
// device, or cuts it off from MQTT, among the first chunks and send the save after it, over MQTT, to a device that
// is restarting on another topic. A curated setting without an index of its own is therefore written only under
// its own name, in a rule as in a line.
func TestAnIndexedSpellingOfASettingIsThatSetting(t *testing.T) {
	checked := 0
	for _, f := range settings.Fields {
		if strings.TrimRight(f.Key, "0123456789") != f.Key {
			continue // FriendlyName1, NtpServer2: the index is the setting's own
		}
		for _, name := range indexed(f.Key) {
			if name == f.Key {
				continue
			}
			checked++
			for _, v := range sampleValues {
				if txt, err := CommandText(name, v); err == nil {
					t.Errorf("%s: a policy can send %q as a generic rule", f.Key, txt)
					break
				}
			}
			if _, err := ParseRules(rulesJSON(name, "1"), settings.Queryable); err == nil || !strings.Contains(err.Error(), "write the setting as "+f.Key) {
				t.Errorf("rule %s: %v, want a refusal naming %s", name, err, f.Key)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no indexed spelling was checked")
	}
}

// Why (#606): Tasmota runs a SetOption synonym as its SetOption (MqttTLS is SetOption103, MqttTLSFingerprint is
// SetOption132, kMqttSynonyms), with or without an index and in any letter case. Taken as a generic rule, such a name
// would skip what policy knows about the SetOption: that it restarts the device (settings.RestartsDevice) and cuts
// it off from MQTT (cmdpolicy names SetOption103 and 132, not MqttTLS). Make compliant would publish "MqttTLS 1" over
// MQTT first, among the first chunks with a save after it, to a device restarting into TLS, and over MQTT alone
// without the consent T10 demands. A synonym is therefore written only as its SetOption, in a rule as in a line;
// every synonym of every pinned catalogue, not a list picked by hand.
func TestASetOptionSynonymIsWrittenAsItsSetOption(t *testing.T) {
	set, err := tasmotacmd.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, v := range set.Versions() {
		cat, _, _ := set.For(v)
		for _, c := range cat.Commands {
			if !strings.HasPrefix(c.SynonymOf, "SetOption") {
				continue
			}
			want := "write the setting as " + c.SynonymOf
			for _, name := range []string{c.Command, strings.ToLower(c.Command), c.Command + "1"} {
				for _, val := range []string{"0", "1"} {
					checked++
					if txt, err := CommandText(name, val); err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("CommandText(%s, %s) = %q, %v; want a refusal naming %s", name, val, txt, err, c.SynonymOf)
					}
					if _, err := ParseRules(rulesJSON(name, val), settings.Queryable); err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("rule %s = %s: %v; want a refusal naming %s", name, val, err, c.SynonymOf)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the pinned catalogues hold no SetOption synonym")
	}
}

// Why (#606): Tasmota reads a SetOption's value with strtol base 0 (support_command.ino:463), so a leading 0 means
// octal: "SetOption103 08" sets 0 and "SetOption103 018" sets 1 (both restart the device and SetOption103 cuts it
// off from MQTT). It reads the index as a plain decimal (atoi, :424), so "SetOption003" is SetOption3 (MQTT off).
// Read with Atoi, policy saw 8 and 18 (no restart, a value the device refuses) and missed the curated SetOption3
// with its 0/1 check, so Make compliant would send such a line among the first chunks, a save after it, and again
// on every run (Verify never reads "08" back). A zero-padded index or value is refused, in a line and in a rule: a
// stored rule whose line can never be built would be skipped on every run with a reason that blames its name.
// The plain spelling still goes.
func TestAZeroPaddedSetOptionIsRefused(t *testing.T) {
	for _, c := range [][2]string{
		{"SetOption103", "08"}, {"SetOption103", "018"}, {"SetOption132", "09"}, {"SetOption55", "08"},
		{"SetOption003", "08"}, {"SetOption003", "0"}, {"SetOption0103", "1"}, {"SetOption20", "01"},
	} {
		if txt, err := CommandText(c[0], c[1]); err == nil {
			t.Errorf("CommandText(%s, %s) = %q, want a refusal", c[0], c[1], txt)
		}
	}
	if _, err := ParseRules(rulesJSON("SetOption003", "0"), settings.Queryable); err == nil || !strings.Contains(err.Error(), "write the setting as SetOption3") {
		t.Errorf("rule SetOption003: %v, want a refusal naming SetOption3", err)
	}
	for _, c := range [][2]string{{"SetOption103", "08"}, {"SetOption20", "01"}, {"SetOption3", "00"}, {"SetOption55", "010"}, {"SetOption103", " 08"}} {
		if _, err := ParseRules(rulesJSON(c[0], c[1]), settings.Queryable); err == nil || !strings.Contains(err.Error(), "plain decimal") {
			t.Errorf("rule %s = %q: %v, want a refusal of the value", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{{"SetOption103", "0"}, {"SetOption20", "1"}, {"SetOption0", "0"}, {"SetOption55", "10"}} {
		if txt, err := CommandText(c[0], c[1]); err != nil || txt != c[0]+" "+c[1] {
			t.Errorf("CommandText(%s, %s) = %q, %v; the plain spelling must still go", c[0], c[1], txt, err)
		}
		if _, err := ParseRules(rulesJSON(c[0], c[1]), settings.Queryable); err != nil {
			t.Errorf("rule %s = %s: %v; the plain spelling must still be accepted", c[0], c[1], err)
		}
	}
}

// Why (#606, the product owner's transport rule of 2026-11-14): a line that changes the device's MQTT connection
// goes over MQTT like any other line while HTTP answers the router's Status 11 right before: no lockout, since the
// router then carries the device's next commands over HTTP. Over MQTT alone (HTTP silent) it needs the lockout
// handling: the router refuses it without a person's consent, which Make compliant never has, and the line is
// skipped. Which line is such a change is the router's call (cmdpolicy.CutsMQTTOff on the text sent, read the way
// the device reads the name: cmdpolicy.ResolveName, which policy uses too). The run around such a line must not send
// its later chunks and a save to a device that is restarting into another topic or broker: every such line either
// restarts the device for policy (chunks() puts it in the last chunks, and a run with a restart adds no save) or
// changes nothing on it: a stored rule may hold a SetOption that restarts at 0 and 1 with a value above 1, which
// Tasmota v16 answers with Command Error and does not apply (settings/fields.go, restartSetOptions). A SetOption's
// value is a plain decimal, as policy reads it: the device reads a leading 0 as octal. The lines are every one a
// policy can send (makeCompliantLines), not a list picked by hand.
func TestEveryCutOffLineMakeCompliantCanSendRestartsOrIsRefusedByTheDevice(t *testing.T) {
	checked := map[string]bool{}
	for _, l := range makeCompliantLines(t) {
		if strings.HasPrefix(l.key, "SetOption") && strconv.Itoa(atoi(l.value)) != l.value {
			t.Errorf("%q: the device reads a leading 0 in the value as octal; policy must not send it", l.text)
		}
		if !cmdpolicy.CutsMQTTOff(l.text) {
			continue
		}
		checked[l.key] = true
		// the restart policy knows of (OrderCommands puts the line last by it), the refusal the device gives
		if !settings.RestartsDevice(l.key, l.value) && !refusedByTheDevice(l.key, l.value) {
			t.Errorf("%q cuts MQTT off, does not restart the device for policy and is applied by it", l.text)
		}
	}
	// Not empty by accident: every setting of the form that cuts MQTT off and is no secret was among them.
	for _, f := range settings.Fields {
		if f.Secret {
			continue
		}
		cuts := false
		for _, v := range sampleValues {
			cuts = cuts || cmdpolicy.CutsMQTTOff(f.Key+" "+v)
		}
		if cuts && !checked[f.Key] {
			t.Errorf("%s cuts MQTT off but no line of it was checked: add a value it accepts to sampleValues", f.Key)
		}
	}
	if !checked["SetOption3"] || !checked["SetOption103"] || !checked["SetOption132"] {
		t.Errorf("the SetOptions that cut MQTT off were not all checked: %v", checked)
	}
}

// refusedByTheDevice reports whether key is a SetOption that restarts the device at 1 and value is a plain decimal
// above 1: Tasmota v16 answers that with Command Error and applies nothing (settings/fields.go, restartSetOptions).
// A value with a leading 0 is octal on the device ("08" is 0, "018" is 1) and is not refused there.
func refusedByTheDevice(key, value string) bool {
	v, err := strconv.Atoi(value)
	return strings.HasPrefix(key, "SetOption") && settings.RestartsDevice(key, "1") && err == nil && strconv.Itoa(v) == value && v > 1
}

// atoi is value as a number, -1 when it is none.
func atoi(value string) int {
	v, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return v
}

// sampleValues are values of every shape a setting of the form accepts: numbers for the switches, the ports and
// the SetOptions (also with a leading 0, which the device reads as octal), a topic, a full topic, a host, a login
// name and a URL.
var sampleValues = []string{"0", "1", "2", "3", "00", "08", "018", "255", "1883", "plug", "%prefix%/%topic%/", "broker.lan", "user", "http://h/fw.bin"}

// indexed is name as it is written alone and with each index Tasmota parses from it (0 to 9, and 01).
func indexed(name string) []string {
	out := []string{name}
	for _, i := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "01"} {
		out = append(out, name+i)
	}
	return out
}

// makeCompliantLines is every line a policy can make Make compliant send, as parseLines accepts it: each name
// below, alone and with an index, with each of sampleValues, kept where CommandText gives exactly that line. The
// names come from the code, not from a list: every setting of the form and its lower-case spelling, every
// SetOption the form takes (SetOption0 to SetOption999, written with leading zeros too), and every command of every
// pinned Tasmota catalogue (a generic rule may name any of them, a SetOption synonym and every command that changes
// the MQTT connection among them).
func makeCompliantLines(t *testing.T) []fixLine {
	t.Helper()
	set, err := tasmotacmd.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range set.Versions() {
		cat, _, _ := set.For(v)
		for _, c := range cat.Commands {
			names = append(names, indexed(c.Command)...)
		}
	}
	for _, f := range settings.Fields {
		names = append(names, indexed(f.Key)...)
		names = append(names, indexed(strings.ToLower(f.Key))...)
	}
	for n := range 1000 {
		for _, f := range []string{"%d", "%02d", "%03d"} {
			names = append(names, fmt.Sprintf("SetOption"+f, n))
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	var out []fixLine
	for _, name := range names {
		for _, v := range sampleValues {
			if txt, err := CommandText(name, v); err == nil && txt == name+" "+v {
				out = append(out, fixLine{name, v, txt})
			}
		}
	}
	return out
}

// Why (#606, security check of PR #816): the router proves HTTP for a change of the MQTT connection with one
// Status 11 right before the Backlog that carries it (T10); it cannot prove HTTP after a line of that same Backlog
// ran. A plain line that closes HTTP to TasmoFleet (SetOption128 0 on a device added by another name than its own)
// packed into one Backlog with a broker or topic change would leave the device restarting onto a broker it may not
// rejoin, with HTTP shut: the lockout T10 rules out while HTTP answers. So the restarting lines of a run that holds
// a change of the MQTT connection never join the last plain chunk: every plain line has run when the router asks
// its Status 11. A run without such a change keeps the one Backlog fewer.
func TestAChangeOfTheMQTTConnectionNeverSharesABacklogWithAPlainLine(t *testing.T) {
	for _, tc := range []struct{ lines, want []string }{
		{[]string{"SetOption128 0", "MqttHost 10.0.0.9"}, []string{"Backlog SetOption128 0", "Backlog MqttHost 10.0.0.9"}},
		{[]string{"PowerOnState 0", "Hostname plug", "Topic other"}, []string{"Backlog PowerOnState 0", "Backlog Hostname plug; Topic other"}},
		{[]string{"SetOption128 0", "SetOption103 1"}, []string{"Backlog SetOption128 0", "Backlog SetOption103 1"}},
		{[]string{"PowerOnState 0", "Hostname plug"}, []string{"Backlog PowerOnState 0; Hostname plug"}},
	} {
		c := &fakeCmd{}
		if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, tc.lines); err != nil {
			t.Fatalf("%q: %v", tc.lines, err)
		}
		if !slices.Equal(c.sent, tc.want) {
			t.Errorf("%q: sent %q, want %q", tc.lines, c.sent, tc.want)
		}
	}
}

// Why (#606, review and security check of PR #816): Make compliant goes over MQTT first, and the MQTT channel only
// publishes what cmdpolicy accepts, a Backlog of at most 255 bytes. A longer Backlog is refused there and
// carried by HTTP, although MQTT could carry it (Tasmota takes about 1200 bytes): an HTTP case of TasmoFleet's own
// making that the product owner's rule does not allow. Worse, cmdpolicy cannot read a Backlog it refuses, so
// CutsMQTTOff says false for a broker or topic change hidden in it: the router asks no Status 11 and marks nothing
// after HTTP carried it, and a device reached over MQTT only fails the run instead of skipping that line. So every
// Backlog Make compliant sends is one cmdpolicy reads, and one that carries a change of the MQTT connection is seen
// as one. Both lists are realistic: a plain plan of firmware and time settings, and a move to another broker.
func TestEveryBacklogMakeCompliantSendsIsOneTheMQTTChannelTakes(t *testing.T) {
	plain := []string{
		"OtaUrl http://firmware.example.lan:8080/tasmota/release/16.0.0/tasmota32-lite-de.bin.gz",
		"NtpServer1 ntp1.example.lan", "NtpServer2 ntp2.example.lan", "NtpServer3 de.pool.ntp.org",
		"TimeStd 0,0,10,1,3,60", "TimeDst 0,0,3,1,2,120", "PowerOnState 3", "SetOption1 1",
	}
	move := []string{
		"FullTopic %prefix%/%topic%/" + strings.Repeat("a", 40) + "/", "MqttHost " + strings.Repeat("a", 30) + ".example.lan",
		"MqttUser " + strings.Repeat("a", 40), "Hostname " + strings.Repeat("a", 30), "Topic " + strings.Repeat("a", 30), "MqttPort 8883",
	}
	for _, lines := range [][]string{plain, move} {
		if _, err := cmdpolicy.Classify("Backlog " + strings.Join(lines, "; ")); err == nil {
			t.Fatalf("fixture: cmdpolicy takes %q in one Backlog, the test would prove nothing", lines)
		}
		c := &fakeCmd{}
		if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
			t.Fatalf("%q: %v", lines, err)
		}
		for _, s := range c.sent {
			if _, err := cmdpolicy.Classify(s); err != nil {
				t.Errorf("sent %q (%d bytes): the MQTT channel refuses it: %v", s, len(s), err)
			}
			parts := strings.Split(strings.TrimPrefix(s, "Backlog "), "; ")
			if slices.ContainsFunc(parts, cmdpolicy.CutsMQTTOff) && !cmdpolicy.CutsMQTTOff(s) {
				t.Errorf("sent %q: it changes the MQTT connection, but the router cannot see it", s)
			}
		}
	}
}

// Why (#606, review and security check of PR #816): a rule may write a SetOption's value as ON, on or " 1"; the
// device gets the canonical "SetOption103 1", which restarts it into TLS and cuts MQTT off. The plan must judge the
// restart by that value, the one Apply judges and the device runs: judged by the rule's raw value, the line stayed
// among the plain ones in the rules' order, and a plain SetOption128 0 after it shared its Backlog, past the
// router's Status 11. On a device added by another name than its own, HTTP is then closed while it restarts onto
// TLS: the lockout T10 rules out while HTTP answers.
func TestARestartIsJudgedByTheValueTheDeviceGets(t *testing.T) {
	live := func(model.DeviceID, string) bool { return true }
	for _, want := range []string{"ON", "on", " 1", "1"} {
		ps := PlanFix([]Finding{
			{DeviceID: 1, Rule: Rule{Command: "SetOption103", Want: want}, Have: "0"},
			{DeviceID: 1, Rule: Rule{Command: "SetOption128", Want: "0"}, Have: "1"},
		}, live)
		if len(ps) != 1 {
			t.Fatalf("want %q: plans %+v", want, ps)
		}
		lines := commandLines(ps[0])
		if !slices.Equal(lines, []string{"SetOption128 0", "SetOption103 1"}) {
			t.Errorf("want %q: lines %q, want the plain line first", want, lines)
			continue
		}
		c := &fakeCmd{}
		if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
			t.Fatalf("want %q: %v", want, err)
		}
		if !slices.Equal(c.sent, []string{"Backlog SetOption128 0", "Backlog SetOption103 1"}) {
			t.Errorf("want %q: sent %q", want, c.sent)
		}
	}
}

// Why (#606, security check of PR #816): cmdpolicy keeps the value of MqttUser out of logs and the audit (Redacted
// shows "MqttUser ***"), and Make compliant may send it. The error of a failed send lands in the job's detail and
// the audit's result, so it names the settings, never the lines with their values. That holds for a send that
// failed and for a refusal of the router that no cut-off line explains (the run stops there, it is not retried).
func TestAFailedSendNamesTheSettingsNotTheirValues(t *testing.T) {
	for _, tc := range []struct {
		line, value string
		err         error
	}{
		{"MqttUser bob", "bob", errors.New("boom")},
		{"NtpServer1 ntp.example.lan", "ntp.example.lan", transport.ErrCutOff},
	} {
		c := &fakeCmd{err: func(string) error { return tc.err }}
		err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, []string{tc.line})
		key, _, _ := strings.Cut(tc.line, " ")
		if !errors.Is(err, tc.err) || strings.Contains(err.Error(), tc.value) || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: err %v, want the setting named without its value", tc.line, err)
		}
		if len(c.sent) != 1 {
			t.Errorf("%q: sent %q, want one try", tc.line, c.sent)
		}
	}
}
