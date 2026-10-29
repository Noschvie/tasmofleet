package web

import (
	"net/url"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// ruleTemplate is one starting point of the New rule page.
type ruleTemplate struct {
	ID    string
	Group string // the heading it is listed under (templateGroups)
	Title string // shown on the card and used as the rule name
	Hint  string // the card's one-line description
	Text  string // longer description (the empty state of the overview)
	Spec  sysconfig.RuleSpec
}

// kv is the card's "command = value" line; a template without a value shows the command only.
func (t ruleTemplate) kv() string {
	if t.Spec.Want == "" {
		return t.Spec.Command
	}
	return t.Spec.Command + " = " + t.Spec.Want
}

// templateGroups is the order of the headings on the New rule page; a group without a template
// is not shown.
var templateGroups = []string{"Time and location", "Updates", "MQTT and telemetry", "Behaviour"}

// policyTemplates are the templates offered on the New rule page. A template opens an unsaved
// Advisory draft for the whole fleet (OtaUrl: for one chip family): a hint first, the admin makes
// it Required. A blank Want is a value only the admin knows (a server, a place, a firmware URL);
// the draft page asks for it. The broker template needs the configured broker host and is left
// out without one. Every template is checked against the Tasmota v16 source, see
// docs/research/tasmota-policy-templates.md; TestTemplatesReadableAndComparable keeps each of
// them readable and comparable.
func policyTemplates(brokerHost string) []ruleTemplate {
	t := []ruleTemplate{
		{ID: "timezone", Group: "Time and location", Title: "Local timezone", Hint: "Daylight saving from the location",
			Text: "Daylight-saving rules from the device location (Timezone 99).",
			Spec: sysconfig.RuleSpec{Command: "Timezone", Want: "99"}},
		{ID: "ntp", Group: "Time and location", Title: "NTP server", Hint: "Every device takes its time from one server",
			Text: "Every device takes its time from the same NTP server (first slot).",
			Spec: sysconfig.RuleSpec{Command: "NtpServer1"}},
		{ID: "latitude", Group: "Time and location", Title: "Latitude", Hint: "Any decimals; compared to 4 decimals (about 11 m)",
			Text: "Every device knows where it is, for sunrise and sunset. Any decimals; compared to 4 decimals (about 11 m), e.g. 52.52.",
			Spec: sysconfig.RuleSpec{Command: "Latitude"}},
		{ID: "longitude", Group: "Time and location", Title: "Longitude", Hint: "Any decimals; compared to 4 decimals (about 11 m)",
			Text: "Every device knows where it is, for sunrise and sunset. Any decimals; compared to 4 decimals (about 11 m), e.g. 13.40.",
			Spec: sysconfig.RuleSpec{Command: "Longitude"}},
		{ID: "otaurl-esp8266", Group: "Updates", Title: "OTA URL for ESP8266", Hint: "Where ESP8266 devices fetch firmware",
			Text: "ESP8266 devices fetch firmware from one address.",
			Spec: sysconfig.RuleSpec{Command: "OtaUrl", Families: []string{string(model.ChipESP8266)}}},
		{ID: "otaurl-esp32", Group: "Updates", Title: "OTA URL for ESP32", Hint: "Where ESP32 devices fetch firmware",
			Text: "ESP32 devices fetch firmware from one address.",
			Spec: sysconfig.RuleSpec{Command: "OtaUrl", Families: []string{policy.AllESP32}}},
		{ID: "telemetry", Group: "MQTT and telemetry", Title: "Telemetry every 5 minutes", Hint: "Devices publish every 5 minutes",
			Text: "Devices publish their state every 5 minutes.",
			Spec: sysconfig.RuleSpec{Command: "TelePeriod", Want: "300"}},
	}
	if brokerHost != "" {
		t = append(t, ruleTemplate{ID: "broker", Group: "MQTT and telemetry", Title: "Use one MQTT broker", Hint: "All devices on one broker",
			Text: "Every device talks to the same broker.",
			Spec: sysconfig.RuleSpec{Command: "MqttHost", Want: brokerHost}})
	}
	t = append(t,
		ruleTemplate{ID: "powerretain", Group: "MQTT and telemetry", Title: "Retain power state", Hint: "The broker keeps the last relay state",
			Text: "Relay state messages carry the retain flag, so the broker keeps the last state.",
			Spec: sysconfig.RuleSpec{Command: "PowerRetain", Want: "1"}},
		ruleTemplate{ID: "poweron", Group: "Behaviour", Title: "Restore power after an outage", Hint: "Relays come back as they were",
			Text: "Relays come back the way they were.",
			Spec: sysconfig.RuleSpec{Command: "PowerOnState", Want: "3"}},
		ruleTemplate{ID: "ledstate", Group: "Behaviour", Title: "Status LED", Hint: "The LED shows the relay state",
			Text: "The status LED follows the relay (LedState 1).",
			Spec: sysconfig.RuleSpec{Command: "LedState", Want: "1"}},
		ruleTemplate{ID: "sleep", Group: "Behaviour", Title: "Dynamic sleep", Hint: "Less heat and power use",
			Text: "The processor sleeps between cycles (Sleep 50).",
			Spec: sysconfig.RuleSpec{Command: "Sleep", Want: "50"}},
	)
	for i := range t {
		t[i].Spec.Name, t[i].Spec.Severity = t[i].Title, "info"
	}
	return t
}

// brokerHost is the host of the configured MQTT broker, "" without one.
func (h *handler) brokerHost() string {
	if h.d.SysConfig == nil {
		return ""
	}
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "MQTT_URL" && f.Value != "" {
			if u, err := url.Parse(f.Value); err == nil {
				return u.Hostname()
			}
		}
	}
	return ""
}
