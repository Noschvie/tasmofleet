package backup

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type reasoned struct{ err error }

func (r reasoned) Error() string      { return r.err.Error() + " (secret-looking detail)" }
func (r reasoned) Unwrap() error      { return r.err }
func (r reasoned) UserReason() string { return "Enter the device's MQTT password to back up over MQTT" }

// Why (#257): the history of a failed backup says what to do ("enter the password"), not the generic
// "did not answer" of the HTTP error underneath, and never the error text itself.
func TestAttemptReasonPrefersAReasonTheTransportWrote(t *testing.T) {
	got := AttemptReason(fmt.Errorf("backup: download config: %w", reasoned{tasmota.ErrUnreachable}))
	if got != "Enter the device's MQTT password to back up over MQTT" || strings.Contains(got, "secret") {
		t.Fatalf("%q", got)
	}
	if errors.Is(errors.New("x"), tasmota.ErrUnreachable) || AttemptReason(tasmota.ErrUnreachable) != "The device did not answer" {
		t.Fatal("the plain unreachable reason changed")
	}
}

// Why (#257 review M1): a firmware upload with an unknown outcome blocks the backup, and the history says
// what to do, in the channel's own words, from the sentinel and not from an error text.
func TestAttemptReasonForAPendingFirmwareUpload(t *testing.T) {
	refused := fmt.Errorf("%w: %w", mqttcmd.ErrRefused, mqttcmd.ErrFirmwarePending)
	err := &transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "a backup over MQTT was refused: " + mqttcmd.Reason(refused)}
	if got := AttemptReason(err); !strings.Contains(got, "wait for the device to restart") {
		t.Fatalf("%q", got)
	}
}
