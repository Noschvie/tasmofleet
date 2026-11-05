package model

import (
	"testing"
	"time"
)

// Why (#447): APIOff is the one rule behind the tile, card, list and every gate. An online device whose refusal
// no HTTP answer followed is off; an answer after the refusal clears it; for a device that went offline it counts
// only when the refusal came after that (a dead device must not read "API off" for good).
func TestAPIOff(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	cases := []struct {
		name string
		h    Health
		want bool
	}{
		{"never refused", Health{State: HealthOnline}, false},
		{"online, refused", Health{State: HealthOnline, HTTPRefusedAt: min(0)}, true},
		{"stale, refused", Health{State: HealthStale, HTTPRefusedAt: min(0)}, true},
		{"online, answered after the refusal", Health{State: HealthOnline, HTTPRefusedAt: min(0), SeenHTTP: min(1)}, false},
		{"online, refused after an answer", Health{State: HealthOnline, HTTPRefusedAt: min(2), SeenHTTP: min(1)}, true},
		{"online again after the refusal (the state changed later)", Health{State: HealthOnline, Since: min(2), HTTPRefusedAt: min(1)}, true},
		{"offline since before the refusal", Health{State: HealthOffline, Since: min(0), HTTPRefusedAt: min(1)}, true},
		{"offline since after the refusal", Health{State: HealthOffline, Since: min(2), HTTPRefusedAt: min(1)}, false},
	}
	for _, c := range cases {
		if got := c.h.APIOff(); got != c.want {
			t.Errorf("%s: APIOff = %v, want %v", c.name, got, c.want)
		}
	}
}
