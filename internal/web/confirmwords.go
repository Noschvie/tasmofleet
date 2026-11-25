package web

import (
	"github.com/a-h/templ"

	"github.com/hellwand/tasmofleet/internal/firmware"
)

// confirmWords is what one destructive confirmation says, written once for its three places: the opener's data-dlg-*
// attributes (a dialog shared by many rows), the dialog itself and its page without JavaScript. The three used to carry
// their own copies of the words, and the copies drifted: a blue verb in the dialog and a red one on the page, a title
// that named the rule on one side and "this rule" on the other (#1093). Every action here is destructive, so the verb
// is danger-solid wherever it is drawn; destructive_test.go renders both sides and compares them.
type confirmWords struct {
	Title, Text, Verb string
	Dismiss           string // the dismissing button when "Cancel" would read like the verb ("Keep running"); "" is "Cancel"
}

// attrs are the data-dlg-* attributes of a link that opens a shared dialog (dialog.js fills it from them): the words,
// where the form posts and the danger tone of the verb.
func (w confirmWords) attrs(action string) templ.Attributes {
	a := templ.Attributes{
		"data-dlg-title":  w.Title,
		"data-dlg-text":   w.Text,
		"data-dlg-action": action,
		"data-dlg-verb":   w.Verb,
		"data-dlg-danger": true,
	}
	if w.Dismiss != "" {
		a["data-dlg-dismiss"] = w.Dismiss
	}
	return a
}

// cancelRemainingWords confirm "Cancel remaining" of a running transaction; text is what it does to the queued devices.
func cancelRemainingWords(txn, text string) confirmWords {
	return confirmWords{Title: "Cancel " + txn + "?", Text: text, Verb: "Cancel remaining", Dismiss: "Keep running"}
}

// cancelScheduleWords confirm cancelling a plan that has not started.
func cancelScheduleWords(txn, text string) confirmWords {
	return confirmWords{Title: "Cancel " + txn + "?", Text: text, Verb: "Cancel schedule", Dismiss: "Keep schedule"}
}

// cancelUpdateWords confirm cancelling the update of one device: planned for later, or running.
func cancelUpdateWords(device, text string, planned bool) confirmWords {
	w := confirmWords{Title: "Cancel the update of " + device + "?", Text: text, Verb: "Cancel update", Dismiss: "Keep running"}
	if planned {
		w.Dismiss = "Keep schedule"
	}
	return w
}

// jobRunningCancelText is what cancelling the update of one device that is already at work does.
const jobRunningCancelText = "A running flash cannot be stopped; a device that has not started keeps its previous state."

// restartDialogID is the id of the restart dialog of the device header; the console's quick Restart opens it too.
const restartDialogID = "dlg-restart"

// restartWords confirm a restart of one device: the device header's dialog, the list's row dialog and the page.
func restartWords(device string) confirmWords {
	return confirmWords{Title: "Restart " + device + "?", Text: "Its relays switch off for a few seconds.", Verb: "Restart device"}
}

// ruleDeleteWords confirm deleting a policy rule: the list's shared dialog, the rule page's dialog and the page without
// JavaScript. applied is how many devices the rule judges now; they stop being checked for command.
func ruleDeleteWords(name, command string, applied int, disabled bool) confirmWords {
	text := "It currently judges " + plural(applied, "device") + ". They stop being checked for " + command +
		". The change is saved as a new version you can revert to."
	if disabled {
		text = "It is disabled and judges no device. The change is saved as a new version you can revert to."
	}
	return confirmWords{Title: "Delete rule “" + name + "”?", Text: text, Verb: "Delete rule"}
}

// isCustomFile says whether a cached file is an image the person uploaded: it cannot be downloaded again.
func isCustomFile(a artifactView) bool {
	return a.Upload != nil || firmware.IsCustomSource(a.Source)
}

// fileName is how a cached file is named in a menu or a title: the label of an uploaded image, else its file name.
func fileName(a artifactView) string {
	if a.Upload != nil {
		return a.Upload.Label
	}
	return a.Filename
}

// fileDeleteWords confirm deleting one cached firmware file: the dialog of the cache list and the page of its Delete link.
func fileDeleteWords(a artifactView) confirmWords {
	if isCustomFile(a) {
		return confirmWords{
			Title: "Delete " + fileName(a) + " (custom) from the cache?",
			Text:  "This is an image you uploaded. Deleting it removes it for good: it cannot be downloaded again, so keep your own copy if you still need it.",
			Verb:  "Delete image",
		}
	}
	return confirmWords{Title: "Delete " + a.Filename + " (" + a.Release + ") from the cache?", Text: waitingText(a), Verb: "Delete file"}
}

// restoreVerb is the confirming button of the restore page: the trigger's verb with its object. The restore replaces
// the device's settings and restarts it, so it is danger-solid like every confirmation that overwrites something.
const restoreVerb = "Restore backup"

// resetWords confirm "Reset all settings": the Advanced tab's dialog and its page.
func resetWords() confirmWords {
	return confirmWords{Title: "Reset all settings?", Text: sysResetText, Verb: "Reset settings"}
}
