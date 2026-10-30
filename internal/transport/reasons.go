package transport

import "fmt"

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
	ActCommands Action = "commands" // console, settings, policy
	ActBackup   Action = "backup"
	ActRestore  Action = "restore"
	ActUpdate   Action = "update"
)

// Reason is one English sentence for one action, shown verbatim in the UI and in errors.
type Reason struct {
	Action Action
	Path   Path
	Text   string
}

// Describe is the base sentence of a path, used when a decision has nothing more specific to say.
func (p Path) Describe() string {
	switch p {
	case HTTP:
		return "over the device's HTTP API"
	case MQTT:
		return "over MQTT"
	case DownloadHTTP:
		return "the device downloads the firmware from TasmoFleet over HTTP"
	case DownloadMQTT:
		return "TasmoFleet starts the download over MQTT and the device fetches the firmware itself"
	case PushHTTP:
		return "TasmoFleet uploads the firmware to the device over HTTP"
	case PushSafeboot:
		return "TasmoFleet uploads the firmware over HTTP through the safeboot partition"
	case PushMQTT:
		return "TasmoFleet uploads the firmware over MQTT (experimental)"
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
		return "the device refuses the web API (SetOption128)"
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
