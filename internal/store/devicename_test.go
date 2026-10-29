package store

import (
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (#125): a name typed by hand is never overwritten, so the page has to be able to show the
// DeviceName the device reports now. The store therefore hands out the recorded DeviceName, and
// the DeviceName the admin chose to keep ignoring ("Keep"), with the device.
func TestDeviceCarriesTheRecordedAndTheKeptDeviceName(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "Fan", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "Modbus Lüftung Test"}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.Name != "Fan" || d.DeviceName != "Modbus Lüftung Test" || d.KeptDeviceName != "" {
		t.Fatalf("name %q device name %q kept %q", d.Name, d.DeviceName, d.KeptDeviceName)
	}
	if err := s.DismissDeviceName(ctx, id, "Modbus Lüftung Test"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.KeptDeviceName != "Modbus Lüftung Test" || d.Name != "Fan" {
		t.Fatalf("kept %q name %q", d.KeptDeviceName, d.Name)
	}
	// The next probe reads another DeviceName: the kept one no longer matches, so the page asks again.
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "Modbus Lüftung Büro"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.DeviceName != "Modbus Lüftung Büro" || d.KeptDeviceName != "Modbus Lüftung Test" {
		t.Fatalf("device name %q kept %q", d.DeviceName, d.KeptDeviceName)
	}
}

// Why (#125): "Keep" answers one specific DeviceName. If the device reported another one since
// the page was drawn, the click must not silence the newer name unseen.
func TestDismissDeviceNameOnlyForTheNameTheAdminSaw(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "Fan", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "New"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissDeviceName(ctx, id, "Old"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if d, _ := s.GetDevice(ctx, id); d.KeptDeviceName != "" {
		t.Fatalf("kept %q", d.KeptDeviceName)
	}
	if err := s.DismissDeviceName(ctx, 9999, "New"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Why (#125): a refresh over MQTT reads the DeviceName but must not write the identity (MQTT is
// the weaker source and never sets DetectedAt). UpdateProbeName stores the name and nothing else,
// under the same follow rule as UpdateProbe: a name tasmofleet filled in follows, a hand-set one stays.
func TestUpdateProbeNameWritesOnlyTheNameFacts(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "Tasmota TV", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01",
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0"}})
	if err := s.UpdateProbeName(ctx, id, g, "Tasmota TV", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProbeName(ctx, id, g, "TV Wall", ""); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.Name != "TV Wall" || d.DeviceName != "TV Wall" || d.Identity.Version != "16.0.0" || d.Identity.Family != model.ChipESP32 {
		t.Fatalf("%+v", d)
	}
	d.Name = "Living room"
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProbeName(ctx, id, g, "TV Other", ""); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if d.Name != "Living room" || d.DeviceName != "TV Other" {
		t.Fatalf("name %q device name %q", d.Name, d.DeviceName)
	}
	if err := s.UpdateProbeName(ctx, id, ProbeGuard{Address: "192.0.2.99", MAC: g.MAC}, "X", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict for a moved device", err)
	}
}
