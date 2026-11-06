// Package notify tells a person about things that happen without them watching: an update
// transaction that finished, a scheduled device backup run with failures, a failed database
// snapshot (#249). Two channels exist, a webhook and SMTP e-mail. The webhook has four formats:
// generic JSON and Home Assistant (the same JSON body), ntfy (plain text body, Title, Click and
// X-Priority headers) and Gotify (JSON for POST /message). Both channels are off until switched on on the Settings
// page, Notifications tab.
//
// Delivery never blocks the code that raised the event and never fails it: Emit sends in the
// background, records the outcome per channel in memory (it starts empty after a restart) and
// logs a failure without the secret. Secrets are only used to send; they are never part of an
// error text, a log line or a status.
package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
)

// Channel is a way to deliver an event.
type Channel string

// Channels.
const (
	Webhook Channel = "webhook"
	Email   Channel = "email"
)

// Event names, as the JSON body carries them.
const (
	EventUpdateFinished = "update.finished"
	EventBackupFailed   = "backup.scheduled_failed"
	EventSnapshotFailed = "snapshot.failed"
	EventTest           = "test"
)

// Results of an update transaction.
const (
	ResultOK        = "ok"
	ResultPartial   = "partial"
	ResultFailed    = "failed"
	ResultCancelled = "cancelled"
)

// Event is one thing to tell. Title is the e-mail subject, Text and URL its body.
type Event struct {
	Name    string
	Result  string
	Title   string
	Text    string
	URL     string // absolute link to the page that shows more; empty without PUBLIC_URL
	Path    string // the same page as a path below the base path; the browser notification opens it
	Time    time.Time
	Context string // short words for the status line, e.g. "update job #41 (partial)"
}

// Selected reports whether the settings ask for this event. The test event is always sent, to
// the channel the person pressed the button of.
func Selected(c config.Config, ev Event) bool {
	switch ev.Name {
	case EventUpdateFinished:
		if !c.NotifyUpdate {
			return false
		}
		return c.NotifyUpdateWhen != config.NotifyWhenProblems || ev.Result == ResultPartial || ev.Result == ResultFailed
	case EventBackupFailed:
		return c.NotifyBackup
	case EventSnapshotFailed:
		return c.NotifySnapshot
	case EventTest:
		return true
	}
	return false
}

// Enabled lists the channels that are switched on and complete.
func Enabled(c config.Config) []Channel {
	var out []Channel
	if c.NotifyWebhook && c.NotifyWebhookURL != "" {
		out = append(out, Webhook)
	}
	if c.NotifySMTP && c.NotifySMTPHost != "" {
		out = append(out, Email)
	}
	return out
}

// Link joins the public address, the base path and a page path. Empty without PUBLIC_URL, so a
// message never carries an address nobody can open.
func Link(c config.Config, path string) string {
	if c.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(c.PublicURL, "/") + strings.TrimRight(c.BasePath, "/") + path
}

// UpdateResult is the result of a transaction from its device counts: partial when some devices
// worked and some failed, cancelled when nothing ran to the end and nothing failed.
func UpdateResult(ok, failed int) string {
	switch {
	case failed > 0 && ok > 0:
		return ResultPartial
	case failed > 0:
		return ResultFailed
	case ok == 0:
		return ResultCancelled
	}
	return ResultOK
}

// UpdateFinished is the event of a finished update transaction. target is the firmware release
// the devices were sent to (empty if the jobs differ).
func UpdateFinished(c config.Config, batch int64, ok, failed, cancelled int, target string, at time.Time) Event {
	res := UpdateResult(ok, failed)
	var parts []string
	for _, p := range []struct {
		n    int
		word string
	}{{ok, "ok"}, {failed, "failed"}, {cancelled, "cancelled"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.word))
		}
	}
	text := plural(ok+failed+cancelled, "device")
	if target != "" {
		text += " to " + target
	}
	if failed > 0 {
		text += fmt.Sprintf("; %d failed", failed)
	}
	return Event{Name: EventUpdateFinished, Result: res,
		Title: fmt.Sprintf("Update job #%d: %s", batch, strings.Join(parts, ", ")), Text: text,
		URL: Link(c, fmt.Sprintf("/transactions/%d", batch)), Path: fmt.Sprintf("/transactions/%d", batch), Time: at,
		Context: fmt.Sprintf("update job #%d (%s)", batch, res)}
}

// maxNamed is how many device names a message lists before it says "and N more".
const maxNamed = 10

// BackupFailed is the event of a scheduled backup run in which some devices failed: one message
// per run, listing the devices.
func BackupFailed(c config.Config, total int, failed []string, at time.Time) Event {
	shown := failed
	more := 0
	if len(shown) > maxNamed {
		shown, more = shown[:maxNamed], len(shown)-maxNamed
	}
	text := "Failed: " + strings.Join(shown, ", ")
	if more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return Event{Name: EventBackupFailed, Result: ResultFailed,
		Title: fmt.Sprintf("Scheduled backup: %d of %d devices failed", len(failed), total), Text: text,
		URL: Link(c, "/devices"), Path: "/devices", Time: at, Context: "scheduled backup"}
}

// BackupMissed is the event of a scheduled backup run that did not happen: TasmoFleet was not
// running at the scheduled time and noticed too late to start it (#601). It is a scheduled backup
// that did not complete, so it follows the same setting and name as BackupFailed. Times are on the
// server's clock, as the schedule is.
func BackupMissed(c config.Config, slot, at time.Time) Event {
	return Event{Name: EventBackupFailed, Result: ResultFailed,
		Title: "Scheduled backup skipped",
		Text: fmt.Sprintf("The scheduled backup of %s was skipped: TasmoFleet was not running then and was back at %s, more than %d minutes late. The next one runs as scheduled.",
			slot.Format("Mon 2 Jan 15:04"), at.Format("Mon 2 Jan 15:04"), int(MissedGrace/time.Minute)),
		URL: Link(c, "/devices/backups"), Path: "/devices/backups", Time: at, Context: "scheduled backup"}
}

// MissedGrace is how late a scheduled backup still starts; the text of BackupMissed names it.
const MissedGrace = 60 * time.Minute

// SnapshotMissed is the event of a scheduled database snapshot that did not happen: TasmoFleet was
// not running at its time and was back too late to take it. It is the snapshot failure event, so
// the same setting (NOTIFY_SNAPSHOT) governs it (#602).
func SnapshotMissed(c config.Config, slot, at time.Time) Event {
	return Event{Name: EventSnapshotFailed, Result: ResultFailed,
		Title: "Scheduled database snapshot skipped",
		Text: fmt.Sprintf("The scheduled snapshot of the TasmoFleet database of %s was skipped: TasmoFleet was not running then and was back at %s, more than %d minutes late. The next one is taken as scheduled.",
			slot.Format("Mon 2 Jan 15:04"), at.Format("Mon 2 Jan 15:04"), int(MissedGrace/time.Minute)),
		URL: Link(c, "/settings/system/snapshots"), Path: "/settings/system/snapshots", Time: at, Context: "database snapshot"}
}

// SnapshotFailed is the event of a failed scheduled database snapshot.
func SnapshotFailed(c config.Config, reason string, at time.Time) Event {
	return Event{Name: EventSnapshotFailed, Result: ResultFailed, Title: "Database snapshot failed",
		Text: "The scheduled snapshot of the TasmoFleet database could not be written: " + reason,
		URL:  Link(c, "/settings/system/snapshots"), Path: "/settings/system/snapshots", Time: at, Context: "database snapshot"}
}

// TestEvent is the message of the "Send test" buttons.
func TestEvent(c config.Config, at time.Time) Event {
	return Event{Name: EventTest, Result: ResultOK, Title: "TasmoFleet test notification",
		Text: "This is a test message from TasmoFleet. If you can read it, this channel works.",
		URL:  Link(c, "/settings/system?tab=notifications"), Time: at, Context: "test"}
}

// sampleAt is the time of the example message.
var sampleAt = time.Date(2026, 10, 31, 3, 12, 7, 0, time.FixedZone("", 3600))

// Sample is the example the Message card on the settings page shows. It is built by the same
// constructor as a real finished update, so its link is there exactly when a real message would
// carry one (PUBLIC_URL set): the preview must not promise a link that is never sent (#579).
func Sample(c config.Config) Event {
	return UpdateFinished(c, 41, 11, 1, 0, "16.1.0", sampleAt)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
