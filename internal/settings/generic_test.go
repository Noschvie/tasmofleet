package settings

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Why: a generic policy rule makes tasmofleet send a bare command name to the whole fleet.
// Queryable is the gate for that, so it must open for plain settings and stay closed for
// Status, secrets, commands that act, and for another spelling of a curated setting (which
// would skip the curated validation and the restart knowledge).
func TestQueryableGenericCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"Sleep": true, "LedState": true, "WebLog": true,
		"Status0": false, "Status": false, "status": false, "Backlog": false, "Reset": false, "Power1": false,
		"MqttPassword": false, "Password": false, "mqttpassword": false,
		"SaveData": true, "TelePeriod": true, // curated: read through their Status block
		"hostname": false, "HOSTNAME": false, "setoption19": false, "SO19": false, "SetOption19": true,
		"Hostname": true, "": false, "Sleep 5": false,
	} {
		if got := Queryable(cmd); got != want {
			t.Errorf("Queryable(%q) = %v, want %v", cmd, got, want)
		}
	}
	if !Generic("Sleep") || Generic("Hostname") || Generic("SetOption19") || Generic("Status0") {
		t.Error("Generic must be true only for readable commands outside the catalog")
	}
}

// Why: Hostname, Topic and the MQTT settings restart the device. Make compliant orders and
// saves around that; a generic rule has no such knowledge, so it must never claim a restart
// (and must not turn a curated setting's restart off by another spelling).
func TestGenericCommandsDoNotRestartTheDevice(t *testing.T) {
	for _, k := range []string{"Sleep", "LedState", "hostname", "SetOption19"} {
		if RestartsDevice(k) {
			t.Errorf("%s restarts the device", k)
		}
	}
	if !RestartsDevice("Hostname") {
		t.Fatal("Hostname restarts the device")
	}
}

// Why: a generic command is read by its bare name, one at a time, and the reply must be exactly
// {"<Command>": scalar}. Any other shape is "unknown", never a guess, and a reply may carry
// credentials, which must not survive into the policy values.
func TestReadKeysGenericCommand(t *testing.T) {
	c := &fakeClient{replies: map[string]string{
		"Sleep":      `{"SLEEP":50}`,
		"LedState":   `{"LedState":"ON"}`,
		"WebLog":     `{"Command":"Unknown"}`,
		"Latitude":   `{"Latitude":{"a":1}}`,
		"Pulsetime1": `{"PulseTime1":{"Set":100}}`,
		"Interlock":  `{"Interlock":"ON","Other":1}`,
		"Baudrate":   `{"Baudrate":"http://user:s3cret@host/x"}`,
		"Longitude":  `{"Longitude":"password=hunter2"}`,
	}}
	m, _ := newSvc(c)
	got, err := m.ReadKeys(t.Context(), 1, []string{"Sleep", "LedState", "WebLog", "Latitude", "Pulsetime1", "Interlock", "Baudrate", "Longitude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["Sleep"] != "50" || got["LedState"] != "1" {
		t.Errorf("plain values: %v", got)
	}
	for _, k := range []string{"WebLog", "Latitude", "Pulsetime1", "Interlock"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q, want unknown", k, v)
		}
	}
	all := strings.Join([]string{got["Baudrate"], got["Longitude"]}, "|")
	if strings.Contains(all, "s3cret") || strings.Contains(all, "hunter2") {
		t.Errorf("a credential survived: %q", all)
	}
	if got["Baudrate"] == "" || got["Longitude"] == "" {
		t.Errorf("a scrubbed value is still a value: %v", got)
	}
	if want := []string{"Sleep", "LedState", "WebLog", "Latitude", "Pulsetime1", "Interlock", "Baudrate", "Longitude"}; !reflect.DeepEqual(c.sent, want) {
		t.Errorf("sent %v, want the bare names one at a time", c.sent)
	}
}

// Why: Status 0 builds a reply that crashes ESP8266 devices; it must never leave, however the
// caller got the name here, and neither may a secret or an acting command.
func TestReadKeysNeverSendsADeniedCommand(t *testing.T) {
	c := &fakeClient{}
	m, _ := newSvc(c)
	for _, k := range []string{"Status0", "Status 0", "status00", "Status", "Backlog", "Reset", "Restart", "Power", "MqttPassword", "Password", "hostname", "SO19", ""} {
		if _, err := m.ReadKeys(t.Context(), 1, []string{"Sleep", k}); !errors.Is(err, ErrNotQueryable) {
			t.Errorf("ReadKeys(%q): %v, want ErrNotQueryable", k, err)
		}
	}
	if len(c.sent) != 0 {
		t.Fatalf("sent %v although a key was refused", c.sent)
	}
}

// Why: a device announces setting changes on its own (stat/<topic>/RESULT); a generic rule's
// value must be kept from there, with credentials masked, and a secret's key must not become a
// value at all.
func TestParseReplyKeepsGenericKeys(t *testing.T) {
	got := ParseReply([]byte(`{"LedState":"ON"}`))
	if !reflect.DeepEqual(got, map[string]string{"LedState": "1"}) {
		t.Fatalf("got %v", got)
	}
	if got := ParseReply([]byte(`{"Baudrate":"https://u:pw@h/x"}`)); strings.Contains(got["Baudrate"], "pw") || got["Baudrate"] == "" {
		t.Fatalf("credentials in %v", got)
	}
	for _, p := range []string{`{"Password":"s3cret"}`, `{"MqttPassword":"****"}`, `{"Status0":1}`, `{"StatusSNS":{"x":1}}`, `{"Reset":"1"}`, `{"POWER":"ON"}`} {
		if got := ParseReply([]byte(p)); got != nil {
			t.Errorf("ParseReply(%s) = %v, want nil", p, got)
		}
	}
	if got := ParseReply([]byte(`{"SetOption19":"OFF"}`)); got["SetOption19"] != "0" {
		t.Errorf("a curated key still maps to its catalog name: %v", got)
	}
}
