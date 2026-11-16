package backup

import (
	"slices"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
)

// noteParts are the parts of one backup in the order the result lists them.
var noteParts = []struct {
	kind model.BackupKind
	name string
}{
	{model.BackupConfigDump, "config"},
	{model.BackupSnapshot, "status"},
	{model.BackupDecoded, "decoded"},
	{model.BackupFiles, "files"},
}

// Note words the result of one device's backup (#838): one backup with its parts named, never the
// number of stored files, which reads like several backups. Every page that shows a backup shows this
// text: the engine's job result and the device page's own Back up. via is the audit "via" of the run:
// which way the dump came.
//
// The text is built from fixed words only. A kind or a via that is not known is left out, so a caller
// may pass values that came from a link without ever showing text a person did not get from here.
func Note(kinds []model.BackupKind, via string) string {
	note := "Backup taken"
	switch via {
	case "mqtt", "http+mqtt":
		note += " over MQTT"
	case "http":
		note += " over HTTP"
	}
	var parts []string
	for _, p := range noteParts {
		if slices.Contains(kinds, p.kind) {
			parts = append(parts, p.name)
		}
	}
	if len(parts) > 0 {
		note += " (" + strings.Join(parts, ", ") + ")"
	}
	return note
}

// KindsOf is the kinds of a run's backups, for Note.
func KindsOf(bs []model.Backup) []model.BackupKind {
	kinds := make([]model.BackupKind, 0, len(bs))
	for _, b := range bs {
		kinds = append(kinds, b.Kind)
	}
	return kinds
}
