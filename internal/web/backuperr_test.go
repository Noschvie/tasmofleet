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

// Why (#257): a backup refused before anything was sent (no usable MQTT password) must not start the
// 60 s cooldown: the person fixes the cause and retries at once. A refused password (Error 1) was an
// attempted transfer and does count; so does any other failure.
func TestRefusedBackupStartsNoCooldownButAnAttemptedOneDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		cooldown bool
	}{
		"nothing sent":      {&transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "enter the device's MQTT password to back up over MQTT"}, false},
		"password refused":  {&transport.BackupBlockedError{Err: tasmota.ErrUnreachable, Reason: "x", Attempted: true}, true},
		"device not answer": {tasmota.ErrUnreachable, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, func(d *Deps) { d.BackupCooldown = time.Hour; d.Backups = &fakeBackups{failErr: tc.err} })
			e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
			resp, body := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
			limited := resp.StatusCode == http.StatusTooManyRequests || strings.Contains(body, "a new one is possible in")
			if limited != tc.cooldown {
				t.Fatalf("cooldown after the first attempt = %v, want %v (status %d)", limited, tc.cooldown, resp.StatusCode)
			}
		})
	}
}
