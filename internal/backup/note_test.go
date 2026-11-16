package backup

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#926): the result said "over MQTT" for a backup whose dump came over MQTT and whose files were
// read over HTTP (Tasmota has no file transfer over MQTT). That hid the one HTTP exception of a backup
// from the person who pressed the button. The text names the transport of each part, and says one
// transport once when both used it, so a plain HTTP backup does not read as a mixed one.
func TestTheResultNamesTheTransportOfTheDumpAndOfTheFiles(t *testing.T) {
	all := []model.BackupKind{model.BackupConfigDump, model.BackupSnapshot, model.BackupDecoded, model.BackupFiles}
	for _, tc := range []struct {
		name, via, files, want string
	}{
		{"dump over MQTT, files over HTTP", "mqtt", "http", "Backup taken over MQTT, files over HTTP (config, status, decoded, files)"},
		{"both over HTTP", "http", "http", "Backup taken over HTTP (config, status, decoded, files)"},
		{"dump over MQTT, no files stored", "mqtt", "", "Backup taken over MQTT (config, status, decoded, files)"},
		{"an unknown files value is dropped", "mqtt", "<b>owned</b>", "Backup taken over MQTT (config, status, decoded, files)"},
		{"no known dump transport: nothing is claimed", "", "http", "Backup taken (config, status, decoded, files)"},
		{"a mixed trace is no transport of the dump", "http+mqtt", "http", "Backup taken (config, status, decoded, files)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Note(all, tc.via, tc.files); got != tc.want {
				t.Errorf("Note = %q, want %q", got, tc.want)
			}
		})
	}
}
