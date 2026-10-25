package web

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// labels is the one place where machine values (health states, job states,
// decisions, kinds) get the wording the UI shows. Anything not listed falls
// back to humanLabel, so a new enum value is readable before it is listed.
var labels = map[string]string{
	// device health
	"online": "Online", "stale": "Stale", "offline": "Offline",
	"auth_failed": "Auth failed", "unknown": "Unknown",
	// relay states
	"ON": "On", "OFF": "Off",
	// OTA job states
	"queued": "Queued", "preflight": "Checking", "backup": "Backing up",
	"set_ota_url": "Preparing", "upgrade_issued": "Flashing", "await_reboot": "Rebooting",
	"applying": "Applying", "apply": "Sending the commands", "done": "Done", "verify": "Verifying", "restore_ota_url": "Cleaning up", "restore_power": "Restoring power",
	"succeeded": "Succeeded", "failed": "Failed", "cancelled": "Cancelled",
	// OTA stages
	"final": "Final image", "minimal": "Minimal image",
	// update decisions
	"upgrade": "Upgrade", "reinstall": "Reinstall", "downgrade": "Downgrade", "skip": "Skip",
	// backup kinds
	"dmp": "Config file", "snapshot": "Snapshot", "decoded": "Decoded JSON",
	// discovery proposals
	"new": "New device", "address-change": "Known device – address changed", "conflict": "Conflict",
	// import plan actions
	"create": "Create", "merge": "Merge",
}

// humanLabel turns "set_ota_url" into "Set ota url" for values without an entry.
func humanLabel(v string) string {
	if v == "" {
		return ""
	}
	s := strings.NewReplacer("_", " ", "-", " ").Replace(v)
	return strings.ToUpper(s[:1]) + s[1:]
}

// label returns the display text of an enum-like value of any string type.
func label(v any) string {
	s := asString(v)
	if l, ok := labels[s]; ok {
		return l
	}
	return humanLabel(s)
}

// tones maps values to the status tone the badge is drawn in: ok, warn, bad,
// info (in progress) or neutral. Tone always comes with a glyph and the text,
// never colour alone.
var tones = map[string]string{
	"online": "ok", "succeeded": "ok", "upgrade": "ok", "create": "ok",
	"stale": "warn", "reinstall": "warn", "downgrade": "warn", "conflict": "warn", "address-change": "warn", "merge": "warn",
	"offline": "bad", "auth_failed": "bad", "failed": "bad", "skip": "neutral",
	"created": "ok", "updated": "warn", "skipped": "neutral", // import result lines (the plan says create, merge, skip)
	"queued": "neutral", "cancelled": "neutral", "unknown": "neutral",
	"done": "ok", "sending": "info", "waiting": "info", "verifying": "info",
	"applying": "info", "preflight": "info", "backup": "info", "set_ota_url": "info", "upgrade_issued": "info",
	"await_reboot": "info", "verify": "info", "restore_ota_url": "info", "restore_power": "info", "new": "info",
}

// tone returns the badge tone of a value (neutral when not listed).
func tone(v any) string {
	if t, ok := tones[asString(v)]; ok {
		return t
	}
	return "neutral"
}

// asString accepts plain and named string types (model.HealthState, ...).
func asString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// jobSteps is the number of steps of the progress bar of an update.
const jobSteps = 8

// jobStep places a job on the progress bar: check, backup, minimal image
// (staged ESP8266 only), reboot, final image, verify, restore power
// settings, done. It never goes backwards along either flow, so the bar of a
// running update only ever grows. Failed and cancelled jobs show no progress
// (the step they stopped at is not recorded on the job).
func jobStep(state model.JobState, stage model.Stage) (int, string) {
	minimal := stage == model.StageMinimal
	switch state {
	case model.JobQueued:
		return 0, "Waiting for a free slot"
	case model.JobPreflight:
		return 1, "Checking the device"
	case model.JobBackup:
		return 2, "Backing up the configuration"
	case model.JobSetOtaURL, model.JobUpgradeIssued:
		if minimal {
			return 3, "Installing the minimal image"
		}
		return 5, "Installing the firmware"
	case model.JobAwaitReboot:
		if minimal {
			return 4, "Rebooting into the minimal image"
		}
		return 5, "Rebooting"
	case model.JobVerify:
		return 6, "Verifying the new version"
	case model.JobRestoreOtaURL:
		return 7, "Restoring settings"
	case model.JobRestorePower:
		return 7, "Restoring the relay state"
	case model.JobApplying:
		return 0, "Applying"
	case model.JobSucceeded:
		return jobSteps, "Done"
	case model.JobFailed:
		return 0, "Failed"
	case model.JobCancelled:
		return 0, "Cancelled"
	}
	return 0, label(string(state))
}

// policyStep places a "Make compliant" job on the same bar: commands sent,
// values read back, done.
func policyStep(state model.JobState) (int, string) {
	switch state { //nolint:exhaustive // an update state never occurs on a policy job; the default words it
	case model.JobQueued:
		return 0, "Waiting for a free slot"
	case model.JobApplying:
		return 3, "Sending the commands and saving"
	case model.JobVerify:
		return 6, "Reading the values back"
	case model.JobSucceeded:
		return jobSteps, "Done"
	case model.JobFailed:
		return 0, "Failed"
	case model.JobCancelled:
		return 0, "Cancelled"
	}
	return 0, label(string(state))
}

// commandStep places a "Send command" job on the same bar: out, done.
// backupStep words the steps of a bulk backup job.
func backupStep(state model.JobState) (int, string) {
	if state == model.JobApplying {
		return 5, "Backing up"
	}
	return commandStep(state)
}

func commandStep(state model.JobState) (int, string) {
	switch state { //nolint:exhaustive // an update state never occurs on a command job; the default words it
	case model.JobQueued:
		return 0, "Waiting for a free slot"
	case model.JobApplying:
		return 5, "Sending the command"
	case model.JobSucceeded:
		return jobSteps, "Done"
	case model.JobFailed:
		return 0, "Failed"
	case model.JobCancelled:
		return 0, "Cancelled"
	}
	return 0, label(string(state))
}

// eventText words a persisted job event for the log. While an update runs the
// device is silent on purpose (it downloads the image, then reboots), so a
// missed probe in those phases is progress, not a timeout to show in red;
// the detail of such a probe is dropped. The same holds for a command that
// timed out while the engine kept waiting (to await_reboot): the device is
// busy flashing. Real failures keep the engine's text. The job gives the
// outcome and the target version their words, pr turns the subject of a
// cancel into a name, and minimal says the event belongs to the first stage
// of a staged update (see eventStageMinimal).
func eventText(e model.JobEvent, j model.Job, pr *principalDir, minimal bool) string {
	if t, ok := silentText(e, j, minimal); ok {
		return t
	}
	switch {
	case e.Event == "command_failed" && e.To == model.JobAwaitReboot:
		return "Device busy, still flashing..."
	case e.Event == "identity" && e.From == model.JobVerify, e.Event == "command_ok" && e.To == model.JobSucceeded && e.From != model.JobApplying:
		if t, ok := outcomeText(e, j); ok {
			return t
		}
	}
	if who, ok := strings.CutPrefix(e.Detail, "cancelled by "); ok && e.Event == "cancel" {
		who = pr.Text(who) // a person's name, never the id
		// not "Cancel cancelled by adam": the event and its detail said the same thing
		if e.To == model.JobCancelled {
			return "Cancelled by " + who
		}
		return "Cancel requested by " + who // the device is flashing; the job ends after this step
	}
	return strings.TrimSpace(label(e.Event) + " " + e.Detail)
}

// eventStageMinimal reports whether evs[i] happened during the minimal stage of
// a staged update. The events carry no stage, but the engine switches stages in
// exactly one place (the verify of the minimal image ends in set_ota_url), so
// replaying the log tells. A job that was already on minimal firmware and
// skipped stage 1 cannot be told from the log and reads as final.
func eventStageMinimal(evs []model.JobEvent, i int, j model.Job) bool {
	if j.Plan != model.PlanStaged {
		return false
	}
	for _, p := range evs[:i] {
		if p.Event == "identity" && p.From == model.JobVerify && p.To == model.JobSetOtaURL {
			return false
		}
	}
	return true
}

// silentText words the quiet phases of an update, naming the firmware the log
// is waiting for: the minimal image, or the target version (direct updates
// name only the version).
func silentText(e model.JobEvent, j model.Job, minimal bool) (string, bool) {
	v := j.TargetRelease
	switch e.Event {
	case "probe_offline":
		switch e.To { //nolint:exhaustive // only the quiet phases are worded
		case model.JobSetOtaURL, model.JobUpgradeIssued:
			switch {
			case minimal:
				return "Downloading the minimal firmware...", true
			case v != "" && j.Plan == model.PlanStaged:
				return "Downloading the final firmware (" + v + ")...", true
			case v != "":
				return "Downloading firmware " + v + "...", true
			}
			return "Downloading firmware...", true
		case model.JobAwaitReboot:
			switch {
			case minimal:
				return "Rebooting into the minimal firmware...", true
			case v != "":
				return "Rebooting into " + v + "...", true
			}
			return "Rebooting...", true
		case model.JobVerify, model.JobRestoreOtaURL:
			return "Waiting for the device to come back...", true
		}
	case "probe_online":
		switch e.To { //nolint:exhaustive // only the quiet phases are worded
		case model.JobUpgradeIssued, model.JobAwaitReboot:
			return "Device answers again", true
		case model.JobVerify:
			switch {
			case minimal:
				return "Waiting for the minimal firmware to answer...", true
			case v != "":
				return "Waiting for " + v + " to answer...", true
			}
			return "Waiting for the new version...", true
		}
	}
	return "", false
}

// outcomeText words the identity read of the verify step, and the last step of
// an update, by what they decided.
func outcomeText(e model.JobEvent, j model.Job) (string, bool) {
	target := "the update"
	if j.TargetRelease != "" {
		target = j.TargetRelease
	}
	switch e.To { //nolint:exhaustive // only the outcomes of a verify read are worded; the rest keeps the generic text
	case model.JobSetOtaURL:
		return "Minimal firmware installed, installing " + target, true
	case model.JobRestoreOtaURL:
		return "New version verified, restoring the device's OtaUrl", true
	case model.JobAwaitReboot:
		return "Not on the new version yet", true
	case model.JobSucceeded:
		t := "Update verified"
		if j.TargetRelease != "" {
			t = "Updated to " + j.TargetRelease
		}
		if j.CancelRequested {
			t += ". Cancel came too late: the update had already been written"
		}
		return t, true
	}
	return "", false
}

// humanReason turns an internal error or refusal text ("no build can be
// chosen: select: fwselect: unknown hardware, refusing to guess") into a
// sentence for people. The templates show it with the original as a tooltip,
// so nothing is lost for support.
func humanReason(s string) string {
	if t, ok := knownReason(s); ok {
		return t
	}
	for _, p := range []string{"no build can be chosen: ", "select: ", "fwselect: ", "tasmota: ", "probe failed: "} {
		s = strings.TrimPrefix(s, p)
	}
	return humanLabel(s)
}

// knownReason words the technical reasons that have a plain sentence.
func knownReason(s string) (string, bool) {
	l := strings.ToLower(s)
	for _, r := range []struct{ contains, text string }{
		{"unknown hardware", "Unknown hardware, not updatable"},
		{"authentication needed", "The device asked for a login, the stored credentials are missing or wrong"},
		{"credentials rejected", "The device rejected the stored credentials"},
		{"family mismatch", "The firmware does not fit this chip family"},
		{"safeboot", "The device runs safeboot firmware"},
		{"timeout", "The device did not answer in time"},
		{"unreachable", "The device is unreachable"},
	} {
		if strings.Contains(l, r.contains) {
			return r.text, true
		}
	}
	return "", false
}

// sentence turns the lowercase clause a service returns ("replaced; restored; verified") into a
// sentence for a page: first letter upper case, a full stop at the end.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}

// needConfirm is the refusal of a confirmation form whose box was not ticked; the pages show it
// next to the box instead of at the top.
const needConfirm = "Tick the box to confirm."

// testNoteView is the result line of a connection test (MQTT, SSO).
type testNoteView struct {
	Label, Msg string
	Failed     bool
}

// testNote builds the result line. sysconfig reports a test that ran but did not
// succeed as the message "failed: <reason>" with no error (the audit row says the same),
// so the prefix is the contract. A failure must show in the error notice, never in the
// green success one.
func testNote(label, msg string) testNoteView {
	return testNoteView{Label: label, Msg: msg, Failed: strings.HasPrefix(msg, "failed")}
}

// uiToneClass is the design v2 tone class (ui.Tone) of a state: t-ok, t-warn, t-danger or t-neutral.
func uiToneClass(v any) string {
	switch tone(v) {
	case "ok":
		return "t-ok"
	case "warn":
		return "t-warn"
	case "bad":
		return "t-danger"
	}
	return "t-neutral"
}

// chipLabel is a chip family as people write it: "esp32c3" is "ESP32-C3", "esp8266" "ESP8266".
func chipLabel(f model.ChipFamily) string {
	s := strings.ToUpper(string(f))
	if rest, ok := strings.CutPrefix(s, "ESP32"); ok && rest != "" {
		return "ESP32-" + rest
	}
	return s
}
