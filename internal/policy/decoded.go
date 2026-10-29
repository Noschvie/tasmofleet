package policy

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/settings"
)

// decodedKeys maps a policy command to its place in the JSON that
// decode-config writes for a .dmp. "flag.save_state" is a key inside the
// "flag" object. Only commands whose location is known appear; every other
// command stays "unknown" instead of being guessed.
var decodedKeys = map[string][]string{
	"PowerOnState": {"poweronstate"},
	"SaveData":     {"save_data"}, // decode-config v16 (not "savedata")
	"TelePeriod":   {"tele_period"},
	"Hostname":     {"hostname"},
	"Topic":        {"mqtt_topic"},
	"FullTopic":    {"mqtt_fulltopic"},
	"MqttHost":     {"mqtt_host"},
	"MqttPort":     {"mqtt_port"},
	"MqttUser":     {"mqtt_user"},
	"OtaUrl":       {"ota_url"}, // decode-config v16; read with RedactURLCredentials
	"SetOption0":   {"flag", "save_state"},
	"SetOption3":   {"flag", "mqtt_enabled"},
	"SetOption4":   {"flag", "mqtt_response"},
	"SetOption19":  {"flag", "hass_discovery"},
}

// BackupReadable reports whether a decoded backup can tell the value of the
// command (it is in decodedKeys, or Timezone). Any other command is only known
// from a live read.
func BackupReadable(command string) bool {
	_, ok := decodedKeys[command]
	return ok || command == "Timezone"
}

// ValuesFromDecoded extracts the policy-relevant settings from a decoded
// backup (decode-config JSON). Missing or oddly typed entries are left out.
// Secrets are never extracted.
func ValuesFromDecoded(raw []byte) map[string]string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	out := map[string]string{}
	for cmd, path := range decodedKeys {
		cur, ok := root[path[0]]
		if !ok {
			continue
		}
		if len(path) == 2 {
			var inner map[string]json.RawMessage
			if json.Unmarshal(cur, &inner) != nil {
				continue
			}
			if cur, ok = inner[path[1]]; !ok {
				continue
			}
		}
		if v, ok := scalar(cur); ok {
			if cmd == "OtaUrl" {
				v = settings.RedactURLCredentials(v)
			}
			out[cmd] = v
		}
	}
	if tz, ok := timezone(root); ok {
		out["Timezone"] = tz
	}
	return out
}

// timezone renders the way the device reports it: "99" or "%+03d:%02d" from
// decode-config's "timezone" (hours) and "timezone_minutes".
func timezone(root map[string]json.RawMessage) (string, bool) {
	var hours, minutes int
	if json.Unmarshal(root["timezone"], &hours) != nil {
		return "", false
	}
	if hours == 99 {
		return "99", true
	}
	if raw, ok := root["timezone_minutes"]; ok && json.Unmarshal(raw, &minutes) != nil {
		return "", false
	}
	return fmt.Sprintf("%+03d:%02d", hours, minutes), true
}

func scalar(raw json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "1", true
		}
		return "0", true
	}
	return "", false
}
