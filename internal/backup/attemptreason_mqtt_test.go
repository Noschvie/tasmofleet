package backup

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

type reasoned struct{ err error }

func (r reasoned) Error() string      { return r.err.Error() + " (secret-looking detail)" }
func (r reasoned) Unwrap() error      { return r.err }
func (r reasoned) UserReason() string { return "enter the device's MQTT password to back up over MQTT" }

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
