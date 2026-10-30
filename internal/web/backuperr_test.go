package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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

// Why (#257): a backup refused before anything was published starts only a short floor, not the full
// cooldown (the person fixes the cause and retries soon), but a tight loop is still rate-limited. A
// refusal after something was published (the opening abort, a start, Error 1) or any other failure
// starts the full cooldown.
func TestRefusedBackupStartsOnlyAShortFloorButAnAttemptedOneTheFullCooldown(t *testing.T) {
	old := refusedBackupFloor
	refusedBackupFloor = 50 * time.Millisecond
	t.Cleanup(func() { refusedBackupFloor = old })
	for name, tc := range map[string]struct {
		err        error
		fullWindow bool
	}{
		"nothing published": {&transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "enter the device's MQTT password"}, false},
		"abort published":   {&transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "x", Attempted: true}, true},
		"device not answer": {tasmota.ErrUnreachable, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, func(d *Deps) { d.BackupCooldown = time.Hour; d.Backups = &fakeBackups{failErr: tc.err} })
			limited := func() bool {
				resp, _ := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
				return resp.StatusCode == http.StatusTooManyRequests
			}
			if limited() {
				t.Fatal("the first attempt was limited")
			}
			if !limited() {
				t.Fatal("a second attempt right away must be rate-limited, whatever the failure")
			}
			time.Sleep(120 * time.Millisecond)
			if again := !limited(); again == tc.fullWindow {
				t.Fatalf("after the floor: retry allowed = %v, want %v", again, !tc.fullWindow)
			}
		})
	}
}
