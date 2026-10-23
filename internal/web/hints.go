package web

import (
	"fmt"
	"strings"
	"time"
)

// Tooltips. The UI uses the native title attribute: it needs no script, works
// with the strict CSP, and is read out as the accessible description by the
// browsers. hints is the one place that explains the status words, chips and
// badges; templates call {{hint .}} and the text is never repeated by hand.
// Plain data cells get no title (the cut-off ones get one from tables.js).
var hints = map[string]string{
	// device health
	"online":      "Online: reports to the broker or answered the last HTTP poll.",
	"offline":     "Offline: reported offline, or no answer to HTTP polls.",
	"auth_failed": "Auth failed: the device refused the stored web login.",
	"unknown":     "Unknown: nothing has been heard from this device yet.",
	// OTA job states
	"queued":          "Queued: waiting for a free update slot.",
	"preflight":       "Checking: reading the device and the firmware file.",
	"backup":          "Backing up: saving the configuration first.",
	"set_ota_url":     "Preparing: pointing the device at the firmware server.",
	"upgrade_issued":  "Flashing: the device is downloading and installing the firmware.",
	"await_reboot":    "Rebooting: waiting for the device to come back.",
	"applying":        "Applying: sending the policy commands and saving them to the device once.",
	"verify":          "Verifying: checking that the device runs the new firmware.",
	"restore_ota_url": "Cleaning up: putting the device's original update address back.",
	"restore_power":   "Restoring power: relays that were on go back on.",
	"succeeded":       "Succeeded: the device runs the target firmware.",
	"failed":          "Failed: the update did not finish; the device keeps its old state.",
	"cancelled":       "Cancelled: stopped before it finished.",
	// restore states ("failed" is shared with the update states)
	"sending":   "Sending: the device is fetching the configuration from this server.",
	"waiting":   "Waiting: the device restarts with the restored configuration.",
	"verifying": "Verifying: comparing the device with the restored backup.",
	"done":      "Done: the restore finished and the check passed.",
	// update decisions
	"upgrade":   "Upgrade: the release is newer than the installed firmware.",
	"reinstall": "Reinstall: the same version is flashed again.",
	"downgrade": "Downgrade: the release is older than the installed firmware.",
	"skip":      "Skip: nothing to do for this device.",
	// discovery proposals and import actions
	"new":            "New device: not in the inventory yet.",
	"address-change": "A known device (same MAC) answers from another address than the one stored.",
	"conflict":       "Conflict: name, address or MAC clashes with a device already in the inventory.",
	"create":         "Create: adds a device to the inventory.",
	"merge":          "Merge: updates the device that is already in the inventory.",
	"created":        "Created: the device was added to the inventory.",
	"updated":        "Updated: the device that was already in the inventory was changed.",
	"skipped":        "Skipped: nothing was written for this row.",
	// transaction outcomes (by tone)
	"outcome-ok":      "Every device in this transaction was updated.",
	"outcome-warn":    "Some devices were updated, others failed or were cancelled.",
	"outcome-bad":     "No device in this transaction was updated.",
	"outcome-info":    "Devices are still being updated.",
	"outcome-neutral": "Cancelled before any device was updated.",
	// misc chips
	"updating":    "An update is running for this device.",
	"no-ota":      "Excluded from OTA updates: never offered firmware.",
	"recommended": "Recommended: the newest release the firmware source offers.",
	"current":     "Current: this version of the rules is in effect.",
	"sev-warn":    "Warn: a deviation is shown as a problem.",
	"sev-info":    "Info: a deviation is listed for your information only.",
	"all-chips":   "The rule applies to every chip family.",
	"compliant":   "Compliant: the device has the expected value.",
}

// hintText explains a status word or chip; staleAfter is the configured
// silence after which an MQTT device counts as stale.
func hintText(v any, staleAfter func() time.Duration) string {
	s := asString(v)
	if s == "stale" {
		d := 11 * time.Minute // health.DefaultStaleAfter
		if staleAfter != nil {
			d = staleAfter()
		}
		return fmt.Sprintf("Stale: the device is online at the broker, but sent no message for %s.", humanDuration(d))
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
