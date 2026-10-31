package tasmotacmd

import (
	"bufio"
	"embed"
	"strings"
	"sync"
)

// Help for a command: a one-line description of our own for the common ones, and a link to the
// matching anchor of the Tasmota docs. The docs' text is not copied: the Tasmota docs state no
// licence for it (maintainer's check, #269). The descriptions below are written for this app.

// DocsBase is where the Commands page of the Tasmota docs lives.
const DocsBase = "https://tasmota.github.io/docs/Commands/"

//go:embed docs-anchors.txt
var docsAnchorsFile embed.FS

var (
	anchorsOnce sync.Once
	anchors     map[string]string // lower case -> the id as the docs spell it
)

func docsAnchors() map[string]string {
	anchorsOnce.Do(func() {
		anchors = map[string]string{}
		raw, err := docsAnchorsFile.ReadFile("docs-anchors.txt")
		if err != nil {
			return
		}
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
				anchors[strings.ToLower(l)] = l
			}
		}
	})
	return anchors
}

// DocsURL is the link to the docs entry of a typed command ("SetOption19", "Power1", "OtaUrl"),
// or false when the pinned list of anchors (docs-anchors.txt) has none for it: a link is never
// built from a guess. An anchor is the name with its index, the name alone, the name in plural (the
// SetOptions table) or the name with 1 (the docs write NtpServer1 for NtpServer<x>), in that order.
func DocsURL(cmd string) (string, bool) {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return "", false
	}
	typed := strings.ToLower(f[0])
	base := strings.TrimRight(typed, "0123456789")
	for _, k := range []string{typed, base, base + "s", base + "1"} {
		if k == "" {
			continue
		}
		if id, ok := docsAnchors()[k]; ok {
			return DocsBase + "#" + id, true
		}
	}
	return "", false
}

// Describe is our own one-line description of a typed command; "SetOption19" is described apart
// from "SetOption", every other index shares the description of its command. Empty when we have none.
func Describe(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	typed := strings.ToLower(f[0])
	if d, ok := descriptions[typed]; ok {
		return d
	}
	return descriptions[strings.TrimRight(typed, "0123456789")]
}

// descriptions are keyed by lower case command name. Every key must be a command in the newest
// catalogue and have a docs anchor (describe_test.go).
var descriptions = map[string]string{
	"power":          "Switch a relay on or off; Power1 to Power8 pick the relay.",
	"poweronstate":   "What the relay does after the device gets power: off, on, toggle, its last state or a pulse.",
	"sleep":          "Pause in milliseconds between processing cycles: longer saves power, shorter reacts faster.",
	"setoption":      "Switch one firmware behaviour on or off, chosen by index.",
	"setoption0":     "Remember the relay state and restore it after a restart.",
	"setoption3":     "Turn the MQTT connection on or off.",
	"setoption4":     "Answer commands on the RESULT topic or on a topic named after the command.",
	"setoption8":     "Show temperatures in Celsius or Fahrenheit.",
	"setoption19":    "Home Assistant discovery, classic Tasmota integration.",
	"teleperiod":     "Seconds between telemetry messages; 0 turns it off.",
	"timezone":       "Offset from UTC in hours, or 99 to use the TimeStd and TimeDst rules.",
	"timestd":        "Rule for the switch back to standard time at the end of daylight saving.",
	"timedst":        "Rule for the switch to daylight saving time.",
	"latitude":       "Latitude of the device, used for sunrise and sunset.",
	"longitude":      "Longitude of the device, used for sunrise and sunset.",
	"altitude":       "Height of the device above sea level, used to correct pressure readings.",
	"mqtthost":       "Host name or IP address of the MQTT broker.",
	"mqttport":       "TCP port of the MQTT broker.",
	"mqttclient":     "Client id the device presents to the MQTT broker.",
	"topic":          "The MQTT topic that names this device.",
	"fulltopic":      "Pattern that builds the MQTT topics from prefix and device topic.",
	"grouptopic":     "Topic shared by a group of devices, for commands to all of them at once.",
	"prefix":         "The prefixes of the command, reply and telemetry topics.",
	"friendlyname":   "Name shown in the web interface and to home automation, one per relay.",
	"devicename":     "Name of the device in the web interface header and in discovery.",
	"hostname":       "Network host name of the device.",
	"module":         "The device type that sets the layout of the GPIO functions.",
	"template":       "Custom device template: name, GPIO functions and base module.",
	"gpio":           "Function assigned to a GPIO pin.",
	"otaurl":         "Web address the device loads firmware from when it is told to upgrade.",
	"pulsetime":      "Switch a relay back after a delay, in steps of 0.1 or 1 second.",
	"blinktime":      "How long a relay stays on or off while it blinks, in 0.1 second steps.",
	"blinkcount":     "How many times a relay toggles when it blinks; 0 blinks until stopped.",
	"buttondebounce": "Time in milliseconds a button must be stable before a press counts.",
	"switchdebounce": "Time in milliseconds a switch must be stable before a change counts.",
	"switchmode":     "How a switch input drives its relay: follow, toggle, push button and others.",
	"ledstate":       "What the status LED shows: power state, network and MQTT activity.",
	"ledpower":       "Power state of the link LED, one per LED.",
	"dimmer":         "Brightness of a light in percent.",
	"color":          "Colour of a light, as red, green, blue and white values or a hex code.",
	"ct":             "Colour temperature of a white light, in mired.",
	"fade":           "Smooth transitions when a light changes brightness or colour.",
	"speed":          "How fast a light fades between settings.",
	"ntpserver":      "The NTP servers the device asks for the time, up to three.",
	"savedata":       "How often settings are written to flash.",
	"webserver":      "Web interface mode: off, user or admin.",
	"weblog":         "Log level shown in the console of the web interface.",
	"syslog":         "Log level sent to a syslog server.",
	"loghost":        "Host name or IP address of the syslog server.",
	"logport":        "UDP port of the syslog server.",
	"ipaddress":      "Static network settings: address, gateway, subnet mask and DNS servers.",
	"ssid":           "Name of a Wi-Fi network the device connects to.",
	"statetext":      "The words used for off, on, toggle and hold in messages.",
	"powerretain":    "Whether power state messages are retained by the MQTT broker.",
}
