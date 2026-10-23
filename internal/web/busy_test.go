package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/devlock"
)

// A device held by another operation is a conflict the user can resolve by
// waiting, not a server error; the message names the holder.
func TestBusyDeviceMapsToConflict(t *testing.T) {
	err := fmt.Errorf("power: %w", devlock.BusyError{Holder: "restore"})
	status, msg := deviceErrorText(err)
	if status != http.StatusConflict || !strings.Contains(msg, "restore") {
		t.Fatalf("got %d %q", status, msg)
	}
	if status, _ := powerErrorText(err); status != http.StatusConflict {
		t.Fatalf("power: %d", status)
	}
}
