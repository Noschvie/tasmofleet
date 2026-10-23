package cmdpolicy

import (
	"strings"
	"testing"
)

// TestScrubReply: device replies can carry credentials (Status 6 shows the
// MQTT user, Rule bodies and WebSend carry user:pass@host, URLs carry
// userinfo). Whatever is shown or stored from a reply goes through here.
func TestScrubReply(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"password key", `{"WebPassword":"hunter2"}`, `{"WebPassword":"***"}`},
		{"mqtt password", `{"MqttPassword":"hunter2","MqttHost":"broker"}`, `{"MqttPassword":"***","MqttHost":"broker"}`},
		{"mqtt user", `{"StatusMQT":{"MqttHost":"b","MqttUser":"bob","MqttPort":1883}}`, `{"StatusMQT":{"MqttHost":"b","MqttUser":"***","MqttPort":1883}}`},
		{"token and secret keys", `{"Token":"abc","client_secret":"x","ApiKey":"k"}`, `{"Token":"***","client_secret":"***","ApiKey":"***"}`},
		{"nested object under a secret key", `{"Password1":{"a":"b"}}`, `{"Password1":"***"}`},
		{"order is kept", `{"z":1,"a":2,"Password":"p","m":3}`, `{"z":1,"a":2,"Password":"***","m":3}`},
		{"array of objects", `{"x":[{"password":"p"},{"ok":"fine"}]}`, `{"x":[{"password":"***"},{"ok":"fine"}]}`},
		{"url userinfo", `{"Url":"http://bob:hunter2@example.org/x"}`, `{"Url":"http://***@example.org/x"}`},
		{"query secret", `{"Rule1":"ON x DO WebSend http://h/cm?user=a&password=hunter2 ENDON"}`, `{"Rule1":"ON x DO WebSend http://h/cm?user=a&password=*** ENDON"}`},
		{"websend bracket form", `{"Rule1":"WebSend [bob:hunter2@10.0.0.1:80] Power ON"}`, `{"Rule1":"WebSend [***@10.0.0.1:80] Power ON"}`},
		{"token equals", `{"x":"auth token=abc123 done"}`, `{"x":"auth token=*** done"}`},
		{"userinfo without a scheme", `{"MqttHost":"alice:hunter2@broker"}`, `{"MqttHost":"***@broker"}`},
		{"ssid is not shown", `{"Wifi":{"SSId":"Home","RSSI":70}}`, `{"Wifi":{"SSId":"***","RSSI":70}}`},
		{"harmless stays", `{"StatusFWR":{"Version":"16.0.0","Hardware":"ESP8266EX"}}`, `{"StatusFWR":{"Version":"16.0.0","Hardware":"ESP8266EX"}}`},
		{"numbers stay exact", `{"Big":12345678901234567890,"F":1.50}`, `{"Big":12345678901234567890,"F":1.50}`},
		{"not json", `plain password=hunter2 text`, `plain password=*** text`},
		{"not json userinfo", `ftp://u:p@h/f`, `ftp://***@h/f`},
		{"escaped quote in value", `{"a":"say \"hi\" token=zzz"}`, `{"a":"say \"hi\" token=***"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(ScrubReply([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("\n in:   %s\n got:  %s\n want: %s", tc.in, got, tc.want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret survived: %s", got)
			}
		})
	}
}

func TestScrubReplyKeepsEmptyAndGarbage(t *testing.T) {
	if got := ScrubReply(nil); len(got) != 0 {
		t.Errorf("nil -> %q", got)
	}
	if got := string(ScrubReply([]byte(`{"a":`))); got != `{"a":` {
		t.Errorf("truncated JSON is scrubbed as text, got %q", got)
	}
}

// Redacted also scrubs the arguments of commands we do not know to be secret:
// the audit log and history keep the text for a long time.
func TestRedactedScrubsArguments(t *testing.T) {
	tests := []struct{ cmd, want string }{
		{"FriendlyName1 x password=hunter2", "FriendlyName1 x password=***"},
		{"FriendlyName1 bob:hunter2@x", "FriendlyName1 ***@x"},
		{"Template {\"NAME\":\"x\"}", "Template {\"NAME\":\"x\"}"},
	}
	for _, tc := range tests {
		if got := Redacted(tc.cmd); got != tc.want {
			t.Errorf("Redacted(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
