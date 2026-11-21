package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: the Stale tooltip must name the threshold that is really configured
// (it changes live on the settings page), not a number baked into the text.
func TestStaleHintNamesConfiguredThreshold(t *testing.T) {
	got := hintText(model.HealthStale, func() time.Duration { return 25 * time.Minute })
	if !strings.Contains(got, "25m") || !strings.Contains(got, "no message") {
		t.Fatalf("hint = %q", got)
	}
	if def := hintText("stale", nil); !strings.Contains(def, "11m") {
		t.Fatalf("default hint = %q", def)
	}
}

// Why (#516): dead copy is not reviewed, and it is where wording the design system retired
// ("Warn", "Enforce") survives. hints may hold exactly the words a call site can pass to h.hint:
// the health state and "mqtt-down" (cells.templ), the job state (updates.go) and "no-ota" (cells.templ). "stale"
// is the exception: hintText builds it from the configured threshold. A new state without an
// entry, or an entry nobody can look up, fails here.
func TestHintsAreExactlyTheWordsACallSiteCanPass(t *testing.T) {
	want := map[string]bool{"no-ota": true, "mqtt-down": true}
	for _, s := range []model.HealthState{model.HealthUnknown, model.HealthOnline, model.HealthStale, model.HealthOffline, model.HealthAuthFailed} {
		if s != model.HealthStale {
			want[string(s)] = true
		}
	}
	for _, s := range []model.JobState{
		model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued, model.JobAwaitReboot,
		model.JobSafebootSwitch, model.JobSafebootRecover, model.JobVerify, model.JobRestoreOtaURL, model.JobApplying,
		model.JobSucceeded, model.JobFailed, model.JobCancelled,
	} {
		want[string(s)] = true
	}
	for k := range hints {
		if !want[k] {
			t.Errorf("hints[%q] cannot be reached: no call site passes it", k)
		}
	}
	for k := range want {
		if hints[k].Title == "" {
			t.Errorf("%q can be passed to h.hint but has no hint", k)
		}
	}
	// "stale" is not in the table; hintText must still answer for it.
	if hints["stale"].Title != "" {
		t.Error(`hints["stale"] would be shadowed: hintText builds the Stale text from the configured threshold`)
	}
	if hintText("stale", nil) == "" {
		t.Error(`hintText("stale") is empty`)
	}
}

func TestFamilyHint(t *testing.T) {
	if got := familyHint("esp32s3"); !strings.HasPrefix(got, "ESP32-S3 devices") {
		t.Errorf("got %q", got)
	}
	if got := familyHint("esp8266"); !strings.HasPrefix(got, "ESP8266 devices") {
		t.Errorf("got %q", got)
	}
	if got := familyHint(""); !strings.Contains(got, "Unknown chip") {
		t.Errorf("got %q", got)
	}
}

// Why: the device list shows the configured threshold in the Stale badge.
func TestDeviceListStaleBadgeTitle(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.StaleAfter = func() time.Duration { return 42 * time.Minute }
		d.Health = fakeHealth{1: {DeviceID: 1, State: model.HealthStale, LastSeen: time.Now()}}
	})
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, "no message for over 42m") {
		t.Fatalf("stale badge title missing the configured threshold")
	}
}
