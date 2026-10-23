package web

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
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

// Why: every status word the UI can draw as a badge explains itself; a new
// state without an explanation is a gap the user would hit.
func TestEveryToneHasAHint(t *testing.T) {
	for k := range tones {
		if k == "stale" {
			continue
		}
		if hints[k] == "" {
			t.Errorf("status %q has a tone but no hint", k)
		}
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
	if !strings.Contains(body, "no message for 42m") {
		t.Fatalf("stale badge title missing the configured threshold")
	}
}
