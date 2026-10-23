package settings

import (
	"strconv"
	"strings"
)

type setOption struct{ name, desc string }

// label renders "SetOption0 (save_state): restore relay state after power
// loss". SetOption numbers are never shown bare (plan rule 6).
func (s setOption) label(n int) string {
	return "SetOption" + strconv.Itoa(n) + " (" + s.name + "): " + s.desc
}

var setOptions = map[int]setOption{
	0:  {"save_state", "restore relay state after power loss"},
	3:  {"mqtt_enabled", "enable MQTT"},
	4:  {"mqtt_response", "MQTT replies as RESULT instead of %COMMAND%"},
	19: {"hass_discovery", "Home Assistant discovery (classic Tasmota integration)"},
}

// Label returns the display text of a command: SetOption numbers carry
// their internal name and description, everything else is the command name
// itself (or the field label when it is a curated field).
func Label(cmd string) string {
	if rest, ok := strings.CutPrefix(cmd, "SetOption"); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			if s, known := setOptions[n]; known {
				return s.label(n)
			}
		}
		return cmd
	}
	if f, ok := Lookup(cmd); ok {
		return f.Label
	}
	return cmd
}
