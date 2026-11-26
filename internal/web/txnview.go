package web

import (
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The wording and view helpers of the Updates pages (design v2, WP P4): transaction ids, titles,
// results, the short state of a device row and the "when" of a history row.

// txTitle names what a transaction does: "Update to Tasmota 14.3.0", "Make compliant · TelePeriod",
// "Back up 58 devices", "Send Status 5". The command is the redacted text (it may hold a secret).
func txTitle(j model.Job, devices int) string {
	switch {
	case j.IsPolicy():
		if s := policySettings(j.Commands); s != "" {
			return "Make compliant · " + s
		}
		return "Make compliant"
	case j.IsCommand():
		return txTarget(j)
	case j.IsBackup():
		return "Back up " + plural(devices, "device")
	case j.IsTemplate():
		return "Apply GPIO template to " + plural(devices, "device")
	case j.TargetCustom != "":
		return "Update to custom firmware " + j.TargetRelease
	}
	return "Update to Tasmota " + j.TargetRelease
}

// policySettings lists the settings a "Make compliant" job changes, in the order of its commands:
// the first word of each command line ("TelePeriod 300" -> "TelePeriod").
func policySettings(commands string) string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(commands, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, f[0])
	}
	return strings.Join(out, ", ")
}

// txResult is the result badge of a transaction: Running, Succeeded, Cancelled, Failed, or what
// came of the devices when they differ ("30 succeeded · 1 failed").
func txResult(done, running, queued, failed, canceled int) (string, ui.Tone) {
	total := done + running + queued + failed + canceled
	switch {
	case running+queued > 0:
		return label("running"), ui.ToneAccent
	case done == total:
		return label(model.JobSucceeded), ui.ToneOK
	case canceled == total:
		return label(model.JobCancelled), ui.ToneNeutral
	case failed == total:
		return label(model.JobFailed), ui.ToneDanger
	}
	if failed > 0 {
		return resultCounts(done, failed, canceled), ui.ToneDanger
	}
	return resultCounts(done, failed, canceled), ui.ToneNeutral
}

// jobBadge is the short state of one device of a transaction, the word of its badge: what the
// device is doing now. The OtaUrl step is the start of the device's download, the Upgrade step
// its download and flash in one pass (Tasmota writes the image while it receives it).
func jobBadge(j model.Job) (string, ui.Tone) {
	if j.CancelRequested && !j.State.Terminal() {
		return "Cancelling", ui.ToneNeutral // a flash cannot be stopped: the job ends after this step
	}
	switch j.State {
	case model.JobScheduled:
		return "Planned", ui.ToneNeutral // waits for its start time (#603), not queued for a slot; the transaction as a whole reads "Scheduled"
	case model.JobQueued:
		return label(model.JobQueued), ui.ToneNeutral
	case model.JobPreflight:
		return "Checking", ui.ToneAccent
	case model.JobBackup:
		return "Backing up", ui.ToneAccent
	case model.JobSetOtaURL:
		return "Downloading", ui.ToneAccent
	case model.JobUpgradeIssued:
		return "Flashing", ui.ToneAccent
	case model.JobSafebootSwitch:
		return "Restarting into safeboot", ui.ToneAccent
	case model.JobSafebootRecover:
		return "Switching back", ui.ToneAccent
	case model.JobAwaitReboot:
		return "Rebooting", ui.ToneAccent
	case model.JobVerify:
		return "Verifying", ui.ToneAccent
	case model.JobRestoreOtaURL:
		return "Restoring", ui.ToneAccent
	case model.JobApplying:
		return "Applying", ui.ToneAccent
	case model.JobSucceeded:
		return "Done", ui.ToneOK
	case model.JobFailed:
		return "Failed", ui.ToneDanger
	case model.JobCancelled:
		if j.NotStarted {
			return "Not started", ui.ToneWarn // its start time passed while TasmoFleet was down (#603)
		}
		return "Cancelled", ui.ToneNeutral
	}
	return label(string(j.State)), ui.ToneNeutral
}

// changeBadge is the state of one change of a "Make compliant" device card.
func changeBadge(s model.JobState) (string, ui.Tone) {
	switch s { //nolint:exhaustive // every other state is the job at work
	case model.JobSucceeded:
		return "Applied", ui.ToneOK
	case model.JobFailed:
		return "Failed", ui.ToneDanger
	case model.JobCancelled:
		return "Cancelled", ui.ToneNeutral
	case model.JobQueued:
		return label(model.JobQueued), ui.ToneNeutral
	}
	return "Applying", ui.ToneAccent
}

// chipName is a chip family as the datasheets write it: "ESP8266", "ESP32", "ESP32-C3".
func chipName(f model.ChipFamily) string {
	s := strings.ToUpper(string(f))
	if rest, ok := strings.CutPrefix(s, "ESP32"); ok && rest != "" {
		return "ESP32-" + rest
	}
	return s
}

// updatePerDevice is the time one device's update typically takes on a LAN (preflight, backup,
// download and flash, reboot, verify). It is for the estimate on the update pages only; the
// state machine's timeouts are the limits.
const updatePerDevice = time.Minute

// updateEstimate is how long n devices take when parallel of them update at a time: one round per
// parallel devices, worded in whole minutes ("6 min").
func updateEstimate(n, parallel int) string {
	if n <= 0 {
		return "–"
	}
	if parallel < 1 {
		parallel = 1
	}
	rounds := (n + parallel - 1) / parallel
	return strconv.Itoa(int((time.Duration(rounds) * updatePerDevice).Minutes())) + " min"
}

// chipNameOf is chipName for a family held as text (the picker's quick selection).
func chipNameOf(f string) string { return chipName(model.ChipFamily(f)) }

// healthDot is the colour of a device's status dot in the update picker.
func healthDot(s model.HealthState) ui.Tone {
	switch s {
	case model.HealthOnline:
		return ui.ToneOK
	case model.HealthOffline, model.HealthAuthFailed:
		return ui.ToneDanger
	case model.HealthStale:
		return ui.ToneWarn
	case model.HealthUnknown:
	}
	return ui.ToneNeutral
}

// upstreamFile is the upstream file the release would install on a device as it runs now, ""
// when that cannot be said (unknown identity, an upload). The preview decides for real.
func upstreamFile(id model.Identity, release string) string {
	if release == "" || strings.HasPrefix(release, customReleasePrefix) || id.Family == model.ChipUnknown {
		return ""
	}
	sel, err := fwselect.Select(id, fwselect.Choice{Release: release, Policy: model.PolicyReinstall})
	if err != nil {
		return ""
	}
	return sel.Filename
}

// releaseOptions are the release cards of the new update: the newest two upstream releases ("Latest"
// on the first, with the release date), every upload, and the older releases apart (folded).
func releaseOptions(rels []firmware.Release, uploads []firmware.CustomImage, chosen string) (cards, older []ui.Option) {
	for i, rel := range rels {
		o := ui.Option{Value: rel.Version, Label: "Tasmota " + rel.Version, Checked: rel.Version == chosen}
		lead := ""
		if i == 0 {
			lead = "Latest"
		}
		if rel.Published.IsZero() {
			o.Text = lead
		} else {
			if lead != "" {
				lead += " · "
			}
			o.TextStamp = ui.Stamped{Lead: lead, At: rel.Published, Stamp: ui.StampDate}
		}
		if i < 2 || o.Checked {
			cards = append(cards, o)
		} else {
			older = append(older, o)
		}
	}
	for _, img := range uploads {
		v := customReleasePrefix + img.Artifact.Custom
		cards = append(cards, ui.Option{Value: v, Label: "Custom firmware " + img.Artifact.Release, Checked: v == chosen,
			TextStamp: ui.Stamped{Lead: img.Label + " · uploaded ", At: img.UploadedAt, Stamp: ui.StampDate}})
	}
	return cards, older
}

// otaParallel is how many devices update at a time: the system setting OTA_PARALLEL, else the
// engine's default of 2.
func (h *handler) otaParallel() int {
	if h.d.SysConfig != nil {
		for _, f := range h.d.SysConfig.State() {
			if f.Key == "OTA_PARALLEL" {
				if n, err := strconv.Atoi(f.Value); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 2
}

func bid(id model.BatchID) string { return strconv.FormatInt(int64(id), 10) }
func jid(id model.JobID) string   { return strconv.FormatInt(int64(id), 10) }

func joinComma(s []string) string { return strings.Join(s, ", ") }

// txListView is the Updates page: the running transactions as cards, then every transaction.
type txListView struct {
	Base     string
	CSRF     string // the Cancel dialog of a plan
	CanStart bool   // updates are switched on
	Running  []ui.TxnData
	History  []txHistRow
	// Scheduled are the plans that wait for their start, soonest first (#604).
	Scheduled []txSchedRow
	// Device is the name of the device the list is limited to ("" for the whole fleet); Self is
	// the list's own address, so the live refresh keeps the filter.
	Device, Self string
}

// txHistRow is one transaction of the history: id, title, devices, result, who and when.
type txHistRow struct {
	ID, Title, Devices, Result, By, When, At, Href string
	Tone                                           ui.Tone
	// A planned update's "when" (#604): WhenLabel ("was due ") is written before the time and Suffix ("· scheduled")
	// after it; Due, when set, is the time shown instead of the creation time: the planned start, or the real one once the update began.
	WhenLabel, Suffix string
	Due               time.Time
}

// txnPage is one transaction: header, progress, then its devices with the open log, or for a
// "Make compliant" transaction (Policy) one card per device with its changes.
type txnPage struct {
	Base, CSRF, LiveURL string
	Batch               model.BatchID
	ID, Title, Sub      string
	State               string
	StateTone           ui.Tone
	Policy              bool
	Active              bool
	// A planned update (#604): Sched is its story (nil for every other transaction), Waiting says that nothing of it
	// has run yet and it waits for its start, NeverRan that nothing of it ever will (not started, cancelled).
	Sched    *txnSched
	Waiting  bool
	NeverRan bool
	Planned  int // devices that wait for their start

	Bar                                                           ui.TxnData
	Total, Done, Running, Queued, Failed, Canceled, CancelPending int
	RetryN                                                        int // failed and cancelled devices: "Retry failed (N)"
	RetryOf                                                       model.BatchID
	RetriedBy                                                     []model.BatchID
	CancelText                                                    string

	Rows  []txnRow
	Sel   *txnSel
	Cards []fixCard

	// Template: an "Apply GPIO template" transaction (#327): one card per device with its four steps.
	Template   bool
	SubPre     string // "started from Policy by adam on "
	SubAt      time.Time
	SubPost    string
	TCards     []tmplCard
	CheckAgain *checkAgain // "Check again…" (nil = none)
}

// txnRow is one device of a transaction; the row links to its log.
type txnRow struct {
	JobID          model.JobID
	Name, From, To string
	Upload         bool   // the image is pushed to the device (push where possible)
	Build          string // the chip build the user chose (ESP32-S2), "" if none
	Badge          string
	Tone           ui.Tone
	Bar            bool // a running device shows its progress
	Pct            int
	Href           string
	Selected       bool
}

// txnSel is the device whose log is open.
type txnSel struct {
	JobID     model.JobID
	DeviceID  model.DeviceID
	Name      string
	CanCancel bool
	CanRetry  bool
	Log       []logLine
}

// fixCard is one device of a "Make compliant" transaction: its changes and their state.
type fixCard struct {
	Name, Href, Reason string
	Note               string // the read-back found drift (#947)
	Tone               ui.Tone
	Changes            []fixChange
}

// fixChange is one line of the card. Have is the device's own value when its read-back differs; Note says more
// where the badge alone does not (#947).
type fixChange struct {
	Text, Label string
	Tone        ui.Tone
	Have, Note  string
}

// errEvents are the job events that report a failure: their log lines are drawn in red.
var errEvents = map[string]bool{"command_failed": true, "guard_refused": true, "timeout": true, "backup_failed": true}

// retryRunningWhy is what stands under "Retry failed" while devices of the transaction still run: the engine would
// refuse a retry then (ErrBatchRunning), so the button says when it comes back (#1072).
const retryRunningWhy = "Retry becomes available when every device has finished."
