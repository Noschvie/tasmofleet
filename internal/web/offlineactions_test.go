package web

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (#93): "can this device be reached now" is the health tracker's answer, which already
// covers every transport: a device that is online over MQTT only is online. Offline, never
// answered and a refused login cannot take an action that needs the device; stale (silent for a
// moment, still being polled) can, like its power switch.
func TestReachableNowFollowsTheHealthState(t *testing.T) {
	for st, want := range map[model.HealthState]bool{
		model.HealthOnline: true, model.HealthStale: true,
		model.HealthOffline: false, model.HealthUnknown: false, model.HealthAuthFailed: false,
	} {
		if got := reachableNow(st); got != want {
			t.Errorf("%s: reachable %v, want %v", st, got, want)
		}
	}
}

// Why: Back up, Settings and Console are all disabled with a reason; an offline device must say
// "offline", not stay clickable until the request fails.
func TestBlockReasonNamesAnOfflineDevice(t *testing.T) {
	d := model.Device{Identity: model.Identity{DetectedAt: time.Now()}}
	if got := blockReason(d, model.Health{State: model.HealthOffline, LastSeen: time.Now()}, nil); got != "The device is offline." {
		t.Errorf("offline: %q", got)
	}
	if got := blockReason(d, model.Health{State: model.HealthOnline, LastSeen: time.Now()}, nil); got != "" {
		t.Errorf("online: %q, want usable", got)
	}
}

// Why: the device list marks each row with its health state so the bulk bar and the row menus
// can tell, live (the badge is swapped over SSE), which devices cannot take an action. Back up
// and Restart in the menus and every action of the bar are tagged as needing the device.
func TestDeviceListCarriesTheStateAndTagsTheActionsThatNeedTheDevice(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now()}
		d.DeviceCmd = &fakeCmd{}
	})
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, `data-state="offline"`) || !strings.Contains(body, `data-state="online"`) {
		t.Error("the status badge lacks the health state")
	}
	bar := between(body, `data-bulkbar`, `class="bulk-clear"`)
	if n := strings.Count(bar, "data-needs-device"); n < 2 { // Update firmware and the power buttons at least
		t.Errorf("bulk bar actions tagged as needing a device: %d\n%s", n, bar)
	}
	if !strings.Contains(body, `data-bar-note`) {
		t.Error("the bar has no place for the offline note")
	}
}

// Why: the offline device's header must not offer Restart or an update; both would fail.
func TestOfflineDeviceHeaderDisablesRestartAndUpdate(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Health.(fakeHealth)[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}
		d.DeviceCmd = &fakeCmd{}
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `<button type="button" class="btn" disabled title="The device is offline.">Restart</button>`) {
		t.Errorf("Restart is not disabled on an offline device")
	}
	if !strings.Contains(body, `title="The device is offline."`) || strings.Contains(body, `data-dialog="dlg-restart"`) {
		t.Errorf("the restart dialog can still be opened")
	}
}

// Why: a stale device is still polled and its power switch works; the bulk scope must run it
// too instead of calling it offline, while an offline one is listed and skipped with a note.
func TestBulkScopeRunsStaleDevicesAndSkipsOfflineOnes(t *testing.T) {
	e, _ := bulkEnv(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthStale, LastSeen: time.Now()}
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "Status 2"))
	if !strings.Contains(body, "1 device run · 1 skipped") {
		t.Errorf("stale runs, offline is skipped: %s", between(body, "run ·", "skipped"))
	}
}
