package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Why (#257): a backup that MQTT could not carry tells the person what to do (the transport's reason),
// not the generic "did not answer" of the HTTP error underneath, and never the Go error chain.
func TestBackupProblemShowsTheTransportReason(t *testing.T) {
	h := &handler{}
	err := fmt.Errorf("backup: download config: %w", &transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "enter the device's MQTT password to back up over MQTT"})
	got := h.backupProblem(err)
	if got != "Backup failed. Enter the device's MQTT password to back up over MQTT." || strings.Contains(got, "backup: download") {
		t.Fatalf("%q", got)
	}
}
