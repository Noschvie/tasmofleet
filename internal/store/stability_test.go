package store

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: a boot-looping device restarts every few minutes for weeks; without a cap the table would grow
// without bound, and the cap must drop the oldest rows of that device only, not another device's.
func TestAppendBootKeepsTheNewestRowsPerDevice(t *testing.T) {
	s, _, _ := newStore(t)
	a, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	b, _ := s.CreateDevice(ctx, model.Device{Name: "b", Address: "192.0.2.11", MAC: "aa:bb:cc:00:00:02"})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.AppendBoot(ctx, model.DeviceBoot{DeviceID: b, At: base, BootCount: 1, Reason: "Exception"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxBootsPerDevice+10; i++ {
		err := s.AppendBoot(ctx, model.DeviceBoot{DeviceID: a, At: base.Add(time.Duration(i) * time.Minute), BootCount: i + 1,
			Reason: "Power on", UptimeBefore: 90 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListBoots(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxBootsPerDevice {
		t.Fatalf("device a keeps %d restarts, want %d", len(got), MaxBootsPerDevice)
	}
	if got[0].BootCount != MaxBootsPerDevice+10 || got[len(got)-1].BootCount != 11 {
		t.Fatalf("kept BootCount %d..%d, want the newest %d..11 (newest first)", got[0].BootCount, got[len(got)-1].BootCount, MaxBootsPerDevice+10)
	}
	if got[0].UptimeBefore != 90*time.Second || got[0].Reason != "Power on" {
		t.Fatalf("round trip lost fields: %+v", got[0])
	}
	if other, _ := s.ListBoots(ctx, b, 0); len(other) != 1 {
		t.Fatalf("device b has %d restarts, the cap of a must not touch it", len(other))
	}
	if two, _ := s.ListBoots(ctx, a, 2); len(two) != 2 {
		t.Fatalf("limit 2 returned %d", len(two))
	}
}

// Why: the dashboard and the list judge every device from one read; it must return only what is inside
// the window and group it per device, or every page view would query per device.
func TestBootsSinceGroupsPerDeviceAndHonoursTheWindow(t *testing.T) {
	s, _, _ := newStore(t)
	a, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	b, _ := s.CreateDevice(ctx, model.Device{Name: "b", Address: "192.0.2.11", MAC: "aa:bb:cc:00:00:02"})
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, x := range []model.DeviceBoot{
		{DeviceID: a, At: now.Add(-8 * 24 * time.Hour)},
		{DeviceID: a, At: now.Add(-time.Hour)},
		{DeviceID: b, At: now.Add(-2 * time.Hour)},
	} {
		if err := s.AppendBoot(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.BootsSince(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got[a]) != 1 || len(got[b]) != 1 {
		t.Fatalf("a: %d, b: %d restarts in the window, want 1 and 1", len(got[a]), len(got[b]))
	}
}

// Why: hourly samples are the only Wi-Fi history; the 7-day prune must remove old ones, keep new
// ones, and a second sample at the same instant must replace the first instead of failing.
func TestWifiSamplesAreReplacedAtTheSameInstantAndPruned(t *testing.T) {
	s, _, _ := newStore(t)
	a, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, w := range []model.WifiSample{
		{DeviceID: a, At: now.Add(-8 * 24 * time.Hour), DBm: -60},
		{DeviceID: a, At: now.Add(-time.Hour), DBm: -70},
		{DeviceID: a, At: now.Add(-time.Hour), DBm: -71},
		{DeviceID: a, At: now, DBm: -80},
	} {
		if err := s.AppendWifiSample(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PruneWifiSamples(ctx, now.Add(-WifiSampleKeep)); err != nil || n != 1 {
		t.Fatalf("pruned %d (err %v), want the one 8-day-old sample", n, err)
	}
	got, err := s.WifiSamplesSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[a]) != 2 || got[a][0].DBm != -71 || got[a][1].DBm != -80 {
		t.Fatalf("samples %+v, want -71 then -80 (oldest first)", got[a])
	}
}

// Why: deleting a device must not leave history rows behind; a new device may reuse the id.
func TestDeletingADeviceRemovesItsStabilityHistory(t *testing.T) {
	s, _, _ := newStore(t)
	a, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	_ = s.AppendBoot(ctx, model.DeviceBoot{DeviceID: a, At: time.Now()})
	_ = s.AppendWifiSample(ctx, model.WifiSample{DeviceID: a, At: time.Now(), DBm: -60})
	if err := s.DeleteDevice(ctx, a); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.ListBoots(ctx, a, 0); len(b) != 0 {
		t.Fatalf("%d restarts left", len(b))
	}
	if w, _ := s.WifiSamplesSince(ctx, time.Time{}); len(w[a]) != 0 {
		t.Fatalf("%d samples left", len(w[a]))
	}
}
