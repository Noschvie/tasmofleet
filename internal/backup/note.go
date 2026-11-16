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
// which way the dump came. files is the way the files archive came ("" when none was stored); it is
// named only when it differs from via, which is the one case a person would otherwise misread: the
// dump over MQTT, the files over HTTP, because Tasmota has no file transfer over MQTT (#926).
//
// The text is built from fixed words only. A kind, a via or a files that is not known is left out, so
// a caller may pass values that came from a link without ever showing text a person did not get from
// here. "http+mqtt" is not a transport of the dump (the snapshot is pinned to the dump's path) and
// reads as unknown.
func Note(kinds []model.BackupKind, via, files string) string {
	note := "Backup taken"
	switch via {
	case "mqtt":
		note += " over MQTT"
	case "http":
		note += " over HTTP"
	}
	if known(via) && known(files) && files != via {
		note += ", files over " + strings.ToUpper(files)
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

// known reports whether v is a transport word Note prints.
func known(v string) bool { return v == "http" || v == "mqtt" }

// KindsOf is the kinds of a run's backups, for Note.
func KindsOf(bs []model.Backup) []model.BackupKind {
	kinds := make([]model.BackupKind, 0, len(bs))
	for _, b := range bs {
		kinds = append(kinds, b.Kind)
	}
	return kinds
}
