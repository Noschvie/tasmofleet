package web

// colTips are the header tooltips of every table column: exactly one short
// sentence saying what the column shows. No thresholds, no how-to, no hints
// about resizing; those belong in the page text. Keyed "<table>.<column>".
// TestEveryColumnHasAShortTip keeps the registry and this list in step.
var colTips = map[string]string{ //nolint:gosec // G101: header texts, no credentials
	"devices.name":       "The device name.",
	"devices.status":     "Health of the device and its policy deviations.",
	"devices.reach":      "Whether the device was heard from over MQTT and over HTTP.",
	"devices.firmware":   "Installed Tasmota version and build.",
	"devices.family":     "Chip of the device.",
	"devices.address":    "Network address the device is reached at.",
	"devices.uptime":     "Time since the last restart.",
	"devices.wifi":       "Wi-Fi signal quality as the device reports it.",
	"devices.restart":    "When the device last restarted.",
	"devices.otaurl":     "Where the device would update from (OtaUrl), as last read.",
	"devices.grouptopic": "Group topic the device listens on, as last read.",
	"devices.power":      "Relay state of the device.",
	"devices.actions":    "Actions for the device.",

	"audit.when":    "When the action happened.",
	"audit.actor":   "Who did it.",
	"audit.action":  "What was done.",
	"audit.target":  "What it was done to.",
	"audit.summary": "What the entry is about, in a few words.",
	"audit.result":  "How it ended.",
	"audit.details": "The recorded details of the entry.",

	"transaction_retry.device": "The device.",
	"transaction_retry.target": "The firmware being installed.",
	"transaction_retry.last":   "How the last attempt ended.",

	"device_settings.setting": "The setting that was changed.",
	"device_settings.command": "The command that was sent.",
	"device_settings.result":  "Whether the device accepted it.",
}
