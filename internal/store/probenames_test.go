package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// probeNamesSetup stores one device and returns it with the guard a probe of it carries.
func probeNamesSetup(t *testing.T, s *SQLite, d model.Device) (model.DeviceID, ProbeGuard) {
	t.Helper()
	id, err := s.CreateDevice(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	return id, ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}
}

// Why (#91, #52): "Refresh identity" must bring the device name up to date when the user did not
// choose one: the name follows the device's DeviceName as long as it is still the DeviceName
// tasmofleet recorded at the previous probe. A name the user typed is never overwritten.
func TestUpdateProbeNameFollowsTheDeviceNameOnlyWhileItIsTheDevicesOwn(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "Tasmota TV", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	// First probe: nothing recorded yet, the name equals the DeviceName and is recorded as such.
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "Tasmota TV"}); err != nil {
		t.Fatal(err)
	}
	// The DeviceName changed on the device: the stored name was the device's own, so it follows.
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "TV Wall"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.Name != "TV Wall" {
		t.Fatalf("Name = %q, want the new DeviceName", d.Name)
	}
	// The user renames it in tasmofleet: from now on the name is theirs.
	d, _ := s.GetDevice(ctx, id)
	d.Name = "Living room TV"
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "Something else"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.Name != "Living room TV" {
		t.Fatalf("Name = %q, the user's name was overwritten", d.Name)
	}
	// A probe that did not read the Status object (DeviceName "") changes nothing.
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.Name != "Living room TV" {
		t.Fatalf("Name = %q after a probe without a DeviceName", d.Name)
	}
}

// Why (#91): devices added before tasmofleet recorded the DeviceName were named after their
// hostname. The prober recognises such a name (NameFrom) and the name follows the DeviceName,
// but only while it is still exactly that name: an edit made during the probe wins.
func TestUpdateProbeReplacesARecognisedFallbackNameOnlyIfUnchanged(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "tasmota-2be970-2416", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceName: "Tasmota TV Wand", NameFrom: "tasmota-2be970-2416"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.Name != "Tasmota TV Wand" {
		t.Fatalf("Name = %q, want the DeviceName", d.Name)
	}

	id2, g2 := probeNamesSetup(t, s, model.Device{Name: "edited meanwhile", Address: "192.0.2.11", MAC: "aa:bb:cc:00:00:02"})
	if err := s.UpdateProbe(ctx, id2, g2, ProbeResult{DeviceName: "Tasmota TV Wand", NameFrom: "tasmota-old-hostname"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id2); d.Name != "edited meanwhile" {
		t.Fatalf("Name = %q, an edit made during the probe was overwritten", d.Name)
	}
}

// Why (#91, #111): the MQTT topic is a mirror used to match messages and must always follow the
// device (else health never matches and the device shows "MQTT: Not connected"), also when it was
// typed or the row predates device_topic. A topic another device holds is never taken (unique).
func TestUpdateProbeTopicAlwaysFollowsTheDevice(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.CreateDevice(ctx, model.Device{Name: "holder", Address: "192.0.2.99", Topic: "taken"}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, stored, recorded, probed, want string
	}{
		{"unresolved placeholder", "tasmota_%06X", "", "tasmota_2BE970", "tasmota_2BE970"},
		{"empty", "", "", "tasmota_2BE972", "tasmota_2BE972"},
		{"device changed its topic", "kitchen", "kitchen", "kitchen-lamp", "kitchen-lamp"},
		{"user typed a topic", "my-own", "", "tasmota_2BE971", "tasmota_2BE971"},
		{"not read", "kitchen2", "kitchen2", "", "kitchen2"},
		{"held by another device", "", "", "taken", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, g := probeNamesSetup(t, s, model.Device{Name: tc.name, Address: "192.0.2.20", Port: 1000 + i, MAC: fmt.Sprintf("aa:bb:cc:00:01:%02x", i), Topic: tc.stored})
			if tc.recorded != "" {
				if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceTopic: tc.recorded}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.UpdateProbe(ctx, id, g, ProbeResult{DeviceTopic: tc.probed}); err != nil {
				t.Fatal(err)
			}
			if d, _ := s.GetDevice(ctx, id); d.Topic != tc.want {
				t.Fatalf("Topic = %q, want %q", d.Topic, tc.want)
			}
		})
	}
}

// Why (#65): a device stored without a MAC has nothing to compare a probe with; an admin's
// refresh binds the MAC it read (with the Ethernet MAC of a wired board), so later probes can
// tell another device at the address. A MAC another device holds is refused with ErrMACInUse
// and nothing of the probe is written.
func TestUpdateProbeBindsTheMACOfADeviceWithoutOne(t *testing.T) {
	s, _, _ := newStore(t)
	id, g := probeNamesSetup(t, s, model.Device{Name: "imported", Address: "192.0.2.30"})
	if err := s.UpdateProbe(ctx, id, g, ProbeResult{BindMACs: []string{"AA:BB:CC:00:00:31", "aa:bb:cc:00:00:32"}}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.MAC != "aa:bb:cc:00:00:31" || !d.HasMAC("aa:bb:cc:00:00:32") {
		t.Fatalf("MAC = %q %v, want the bound pair", d.MAC, d.MACs)
	}

	other, og := probeNamesSetup(t, s, model.Device{Name: "other", Address: "192.0.2.31"})
	err := s.UpdateProbe(ctx, other, og, ProbeResult{Identity: model.Identity{Version: "16.0.0"}, BindMACs: []string{"aa:bb:cc:00:00:32"}})
	if !errors.Is(err, ErrMACInUse) || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrMACInUse (a conflict)", err)
	}
	if d, _ := s.GetDevice(ctx, other); d.MAC != "" || d.Identity.Version != "" {
		t.Fatalf("a refused binding wrote %+v", d)
	}

	// A device that has a MAC is never rebound by a probe: the guard holds its MAC.
	if err := s.UpdateProbe(ctx, id, ProbeGuard{Address: "192.0.2.30", MAC: ""}, ProbeResult{BindMACs: []string{"aa:bb:cc:00:00:99"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a conflict for a bound device", err)
	}
	if d, _ := s.GetDevice(ctx, id); d.MAC != "aa:bb:cc:00:00:31" {
		t.Fatalf("MAC = %q, a bound MAC was replaced", d.MAC)
	}
}
