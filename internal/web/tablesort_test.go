package web

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: a string sort puts 15.10.0 before 15.9.0; fleets are updated by version.
func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"15.9.0", "15.10.0", -1},
		{"16.0.0", "16.0.0", 0},
		{"16.0.0-rc1", "16.0.0", -1},
		{"v16.0.0", "15.99.9", 1},
		{"16.0", "16.0.0", 0},
		{"16.0.0.1", "16.0.0", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareAddresses(t *testing.T) {
	if compareAddresses("172.31.13.9", "172.31.13.10") >= 0 {
		t.Error("addresses must compare numerically")
	}
	if compareAddresses("172.31.13.9", "plug.example.internal") >= 0 {
		t.Error("names go after addresses")
	}
}

func row(name, version, addr string, st model.HealthState, up int64, rssi int) deviceRow {
	return deviceRow{
		Device: model.Device{Name: name, Address: addr, Identity: model.Identity{Version: version}},
		Health: model.Health{State: st, UptimeSec: up, RSSI: rssi, LastSeen: time.Unix(1e9, 0)},
	}
}

func names(rows []deviceRow) string {
	s := ""
	for _, r := range rows {
		s += r.Device.Name + " "
	}
	return s
}

// Why: values must sort by type (numbers, versions, severity), a missing value
// goes last in both directions (never first), and the name breaks ties.
func TestSortDevices(t *testing.T) {
	rows := func() []deviceRow {
		return []deviceRow{
			row("c", "15.10.0", "172.31.13.10", model.HealthOffline, 90, 0),
			row("a", "15.9.0", "172.31.13.9", model.HealthOnline, 3600, 70),
			row("b", "", "172.31.13.100", model.HealthStale, 0, 30),
			row("d", "16.0.0", "172.31.13.2", model.HealthAuthFailed, 7200, 70),
		}
	}
	for _, c := range []struct {
		key, dir, want string
	}{
		{"name", "desc", "d c b a "},
		{"firmware", "asc", "a c d b "},
		{"firmware", "desc", "d c a b "},
		{"address", "asc", "d a c b "},
		{"status", "asc", "a b c d "},
		{"status", "desc", "d c b a "},
		{"uptime", "asc", "c a d b "},
		{"uptime", "desc", "d a c b "},
		{"wifi", "desc", "a d b c "},
	} {
		r := rows()
		sortDevices(r, tableSort{c.key, c.dir})
		if got := names(r); got != c.want {
			t.Errorf("sort %s %s = %q, want %q", c.key, c.dir, got, c.want)
		}
	}
}

func TestChannels(t *testing.T) {
	now := time.Unix(10_000, 0)
	win := 11 * time.Minute
	get := func(h model.Health) string {
		s := ""
		for _, c := range channelsOf(now, win, h) {
			s += c.Name + c.Mark + " "
		}
		return s
	}
	// Why: an HTTP-only device has no MQTT channel; an MQTT device is not polled over HTTP; a silent MQTT channel is flagged.
	if got := get(model.Health{SeenHTTP: now.Add(-time.Minute)}); got != "MQTT– HTTP✓ " {
		t.Errorf("http only: %q", got)
	}
	if got := get(model.Health{SeenMQTT: now.Add(-time.Minute), SeenHTTP: now.Add(-time.Hour), LWT: "Online"}); got != "MQTT✓ HTTP– " {
		t.Errorf("mqtt device: %q", got)
	}
	if got := get(model.Health{SeenMQTT: now.Add(-time.Hour), LWT: "Online"}); got != "MQTT✕ HTTP– " {
		t.Errorf("silent mqtt: %q", got)
	}
}
