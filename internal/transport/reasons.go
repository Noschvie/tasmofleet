package transport

import (
	"fmt"

	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Path is one way to do one thing with a device.
type Path string

// Paths. None means no way exists; its Reason says why.
const (
	None         Path = "none"
	HTTP         Path = "http"
	MQTT         Path = "mqtt"
	DownloadHTTP Path = "download_http"
	DownloadMQTT Path = "download_mqtt"
	PushHTTP     Path = "push_http"
	PushSafeboot Path = "push_safeboot"
	PushMQTT     Path = "push_mqtt"
)

// AllPaths lists every Path value.
var AllPaths = []Path{None, HTTP, MQTT, DownloadHTTP, DownloadMQTT, PushHTTP, PushSafeboot, PushMQTT}

// Action is what the person wants to do with a device.
type Action string

// Actions Choose answers.
const (
	ActCommands Action = "commands" // reads: Status N, identity refresh, read-only console commands
	ActWrites   Action = "writes"   // console, settings, Make compliant, backlog: commands that change the device
	ActBackup   Action = "backup"
	ActRestore  Action = "restore"
	ActUpdate   Action = "update"
	// ActJob is not answered by Choose: a command that starts a firmware upgrade or a restore (Upgrade or
	// WebGetConfig with an argument) is never sent over MQTT (the router's job rule), so it needs HTTP and
	// has no path while HTTP fails.
	ActJob Action = "job"
)

// Reason is one English sentence for one action, shown verbatim in the UI and in errors.
type Reason struct {
	Action Action
	Path   Path
	Text   string
}

// Describe is the base sentence of a path. For an update path it is the one sentence every screen and log line uses
// (#987): it names both legs, the transport of the commands (OtaUrl, Upgrade) and how the image reaches the device.
// The image never crosses the broker: the device fetches it over HTTP from TasmoFleet's firmware server, or
// TasmoFleet pushes it. Every update Reason starts with it and only adds the why.
func (p Path) Describe() string {
	switch p {
	case HTTP:
		return "over the device's HTTP API"
	case MQTT:
		return "over MQTT"
	case DownloadHTTP:
		return "commands over HTTP, the image over HTTP from TasmoFleet's firmware server"
	case DownloadMQTT:
		return "commands over MQTT, the image over HTTP from TasmoFleet's firmware server"
	case PushHTTP:
		return "commands over HTTP, the image pushed by TasmoFleet over HTTP"
	case PushSafeboot:
		return "commands over HTTP, the image pushed by TasmoFleet over HTTP through the safeboot partition"
	case PushMQTT:
		return "commands over MQTT, the image pushed by TasmoFleet over MQTT (experimental)"
	case None:
		return "no path is available"
	default:
		return "an unknown path"
	}
}

func (f HTTPState) failText() string {
	switch f.Kind {
	case HTTPFailOutside:
		return "the device's address is outside DEVICE_NETWORKS"
	case HTTPFailAPIRefused:
		return tasmota.APIOffName
	case HTTPFailAuth:
		return "the device refused the web password"
	case HTTPFailUnreachable:
		return "TasmoFleet cannot reach the device over HTTP"
	default:
		return "HTTP to the device fails"
	}
}

// httpFailText says what is wrong with HTTP, with the date when known.
func httpFailText(h HTTPState) string {
	if h.Since.IsZero() {
		return h.failText()
	}
	return fmt.Sprintf("%s (since %s)", h.failText(), h.Since.UTC().Format("2006-01-02 15:04 UTC"))
}
