package web

import (
	"fmt"
	"strings"
	"time"
)

// Tooltips. The UI uses the native title attribute: it needs no script, works
// with the strict CSP, and is read out as the accessible description by the
// browsers. But a title is only ever an addition (#1073): no hover on a phone,
// not read out by default, so what a state means is also on screen. hints is
// the one place that explains the status words and chips; templates call
// h.hint(word) for the title and h.why(word) for the short reason shown as
// text, and neither is repeated by hand. It holds exactly the words a call site
// can pass: the health state and "mqtt-down" (cells.templ), the job state
// (updates.go) and "no-ota" (cells.templ);
// TestHintsAreExactlyTheWordsACallSiteCanPass keeps it so. "stale" is built by
// lookupHint from the configured threshold. Plain data cells get no title (the
// cut-off ones get one from tables.js).
var hints = map[string]hint{
	// device health: the Status cell shows Why under the badge of every state but Online
	"online":      {Title: "Online: reports to the broker or answered the last HTTP poll."},
	"offline":     {Title: "Offline: reported offline, or no answer to HTTP polls.", Why: "no answer"},
	"auth_failed": {Title: "Auth failed: the device refused the stored web login.", Why: "login refused"},
	"unknown":     {Title: "Unknown: nothing has been heard from this device yet.", Why: "not heard from yet"},
	// an online device that nothing reaches over MQTT
	"mqtt-down": {Title: "The device answers over HTTP, but nothing arrives over MQTT", Why: "HTTP only"},
	// OTA job states
	"queued":           {Title: "Waiting: no update slot is free yet."},
	"preflight":        {Title: "Checking: reading the device and the firmware file."},
	"backup":           {Title: "Backing up: saving the configuration first."},
	"set_ota_url":      {Title: "Preparing: pointing the device at the firmware server."},
	"upgrade_issued":   {Title: "Flashing: the device is downloading and installing the firmware."},
	"await_reboot":     {Title: "Rebooting: waiting for the device to come back."},
	"safeboot_switch":  {Title: "Restarting into safeboot: the image does not fit the running firmware, so the device restarts into its safeboot partition and takes the image there."},
	"safeboot_recover": {Title: "Switching back: the upload into safeboot did not work out; the device is sent back to its firmware."},
	"applying":         {Title: "Applying: sending the policy commands and saving them to the device once."},
	"verify":           {Title: "Verifying: checking that the device runs the new firmware."},
	"restore_ota_url":  {Title: "Cleaning up: putting the device's original update address back."},
	"succeeded":        {Title: "Succeeded: the device runs the target firmware."},
	"failed":           {Title: "Failed: the update did not finish; the device keeps its old state."},
	"cancelled":        {Title: "Cancelled: stopped before it finished."},
	// the device list's tag
	"no-ota": {Title: "Excluded from OTA updates: never offered firmware."},
}

// hint is what a status word or chip means: Title is the tooltip, Why the short reason shown beside the word
// ("" for a word that needs none).
type hint struct{ Title, Why string }

// hintText is the tooltip of a status word or chip; staleAfter is the configured
// silence after which an MQTT device counts as stale.
func hintText(v any, staleAfter func() time.Duration) string { return lookupHint(v, staleAfter).Title }

// hintWhy is the short reason of a status word shown as text ("no message for over 11m"), "" when it has none.
func hintWhy(v any, staleAfter func() time.Duration) string { return lookupHint(v, staleAfter).Why }

func lookupHint(v any, staleAfter func() time.Duration) hint {
	s := asString(v)
	if s == "stale" {
		d := 11 * time.Minute // health.DefaultStaleAfter
		if staleAfter != nil {
			d = staleAfter()
		}
		silent := humanDuration(d)
		return hint{Title: fmt.Sprintf("Stale: the device is online at the broker, but sent no message for over %s.", silent), Why: "no message for over " + silent}
	}
	return hints[s]
}

// familyHint explains a chip family chip: the safety rule behind it.
func familyHint(family string) string {
	if family == "" {
		return "Unknown chip: not updated until the chip is detected."
	}
	name := strings.ToUpper(family)
	if len(name) > 5 && strings.HasPrefix(name, "ESP32") {
		name = "ESP32-" + name[5:] // esp32s3 -> ESP32-S3
	}
	return fmt.Sprintf("%s devices get only firmware built for this chip.", name)
}
