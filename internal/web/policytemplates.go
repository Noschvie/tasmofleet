package web

import (
	"net/url"

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

// templateGroups is the order of the headings on the New rule page; a group without a template
// is not shown.
var templateGroups = []string{"Time and location", "Updates", "MQTT and telemetry", "Behaviour"}

// policyTemplates are the templates offered on the New rule page. A template opens an unsaved
// Advisory draft for the whole fleet: a hint first, the admin makes it Required. The broker
// template needs the configured broker host and is left out without one.
func policyTemplates(brokerHost string) []ruleTemplate {
	t := []ruleTemplate{
		{ID: "telemetry", Group: "MQTT and telemetry", Title: "Telemetry every 5 minutes", Hint: "Devices publish every 5 minutes",
			Text: "Devices publish their state every 5 minutes.",
			Spec: sysconfig.RuleSpec{Command: "TelePeriod", Want: "300"}},
		{ID: "poweron", Group: "Behaviour", Title: "Restore power after an outage", Hint: "Relays come back as they were",
			Text: "Relays come back the way they were.",
			Spec: sysconfig.RuleSpec{Command: "PowerOnState", Want: "3"}},
	}
	if brokerHost != "" {
		t = append(t, ruleTemplate{ID: "broker", Group: "MQTT and telemetry", Title: "Use one MQTT broker", Hint: "All devices on one broker",
			Text: "Every device talks to the same broker.",
			Spec: sysconfig.RuleSpec{Command: "MqttHost", Want: brokerHost}})
	}
	t = append(t, ruleTemplate{ID: "timezone", Group: "Time and location", Title: "Local timezone", Hint: "Daylight saving from the location",
		Text: "Daylight-saving rules from the device location (Timezone 99).",
		Spec: sysconfig.RuleSpec{Command: "Timezone", Want: "99"}})
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
