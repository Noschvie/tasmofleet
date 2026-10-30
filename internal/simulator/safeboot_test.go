package simulator

import (
	"strings"
	"testing"
)

// Why: the safeboot push (#30) decides on the /up page. The single-OTA layout
// must show the form that calls upl(this) while the normal firmware runs, the
// dual layout and safeboot itself the plain one; otherwise the engine's
// layout test would be tested against a simulator that cannot tell them apart.
func TestUpPageShowsTheLayout(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	single := add(t, f, DeviceSpec{Name: "single", Profile: ProfileESP32})
	dual := add(t, f, DeviceSpec{Name: "dual", Profile: ProfileESP32Dual})
	page := func(d Device) string {
		b, code, err := cmWithPath(d, "/up", "")
		if err != nil || code != 200 {
			t.Fatalf("GET /up: %d %v", code, err)
		}
		return b
	}
	if !strings.Contains(page(single), "upl(this)") {
		t.Error("the single-OTA layout must show the safeboot upload form")
	}
	if strings.Contains(page(dual), "upl(this)") {
		t.Error("the dual layout must show the plain upload form")
	}
	if b, _, _ := cmWithPath(dual, "/cm", "cmnd=Status%204"); !strings.Contains(b, `"Free":2880`) {
		t.Errorf("dual layout Free: %s", b)
	}
	// Into safeboot the same device serves the plain form.
	if b, _, err := cmWithPath(single, "/u4", "u4=fct&api="); err != nil || b != "false" {
		t.Fatalf("/u4 fct: %q %v", b, err)
	}
	settled(t, single, 1)
	eq(t, "image", single.Image(), "safeboot")
	if strings.Contains(page(single), "upl(this)") {
		t.Error("safeboot itself must show the plain upload form")
	}
}

// Why: /u4 is the only way into safeboot; its answers are what the engine
// reads (false = restarting, true = nothing to do, none = no factory
// partition). In safeboot fct must not restart again, and the switch back
// must restore the app that was running.
func TestU4SwitchesAndAnswers(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	get := func(q string) string {
		b, _, err := cmWithPath(d, "/u4", q)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	eq(t, "ota in the app", get("u4=ota&api="), "true")
	eq(t, "boots after a no-op", d.Boots(), 0)
	eq(t, "fct in the app", get("u4=fct&api="), "false")
	settled(t, d, 1)
	eq(t, "image in safeboot", d.Image(), "safeboot")
	eq(t, "fct in safeboot", get("u4=fct&api="), "true")
	eq(t, "boots after fct in safeboot", d.Boots(), 1)
	eq(t, "ota in safeboot", get("u4=ota&api="), "false")
	settled(t, d, 2)
	eq(t, "image after the way back", d.Image(), "tasmota32")
	eq(t, "version after the way back", d.Version(), "16.0.0")

	dual := add(t, f, DeviceSpec{Name: "dual", Profile: ProfileESP32Dual})
	if b, _, _ := cmWithPath(dual, "/u4", "u4=fct&api="); b != "none" {
		t.Errorf("dual layout answered %q, want none", b)
	}
	plug := add(t, f, DeviceSpec{Name: "esp8266", Profile: ProfileESP8266_4M})
	if _, code, _ := cmWithPath(plug, "/u4", "u4=fct&api="); code != 404 {
		t.Errorf("an ESP8266 has no /u4, got %d", code)
	}
}

// Why: the faults are what the engine's recovery is tested against; each must
// do exactly what its name says and nothing else.
func TestSafebootFaults(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	t.Run("no answer does not restart", func(t *testing.T) {
		d := add(t, f, DeviceSpec{Name: "a", Profile: ProfileESP32, Faults: []Fault{{Kind: FaultSwitchNoAnswer}}})
		if _, _, err := cmWithPath(d, "/u4", "u4=fct&api="); err == nil {
			t.Error("the reply must be cut")
		}
		eq(t, "boots", d.Boots(), 0)
		eq(t, "image", d.Image(), "tasmota32")
	})
	t.Run("lost reply restarts", func(t *testing.T) {
		d := add(t, f, DeviceSpec{Name: "b", Profile: ProfileESP32, Faults: []Fault{{Kind: FaultSwitchLostReply}}})
		if _, _, err := cmWithPath(d, "/u4", "u4=fct&api="); err == nil {
			t.Error("the reply must be cut")
		}
		settled(t, d, 1)
		eq(t, "image", d.Image(), "safeboot")
	})
	t.Run("refused upload keeps safeboot", func(t *testing.T) {
		d := add(t, f, DeviceSpec{Name: "c", Profile: ProfileESP32, Faults: []Fault{{Kind: FaultSafebootRefuseUpload}}})
		_, _, _ = cmWithPath(d, "/u4", "u4=fct&api=")
		settled(t, d, 1)
		getUp(t, d)
		page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0"))
		if err != nil || !strings.Contains(page, "Failed") {
			t.Fatalf("%v %s", err, page)
		}
		eq(t, "boots", d.Boots(), 1)
		eq(t, "image", d.Image(), "safeboot")
	})
	t.Run("stuck in safeboot", func(t *testing.T) {
		d := add(t, f, DeviceSpec{Name: "d", Profile: ProfileESP32, Faults: []Fault{{Kind: FaultStuckInSafeboot}}})
		_, _, _ = cmWithPath(d, "/u4", "u4=fct&api=")
		settled(t, d, 1)
		_, _, _ = cmWithPath(d, "/u4", "u4=ota&api=")
		settled(t, d, 2)
		eq(t, "image after the switch back", d.Image(), "safeboot")
		getUp(t, d)
		if page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); err != nil || !strings.Contains(page, "Successful") {
			t.Fatalf("%v %s", err, page)
		}
		settled(t, d, 3)
		eq(t, "image after an upload", d.Image(), "safeboot")
	})
	t.Run("upload in safeboot flashes the app", func(t *testing.T) {
		d := add(t, f, DeviceSpec{Name: "e", Profile: ProfileESP32, Version: "16.0.0"})
		_, _, _ = cmWithPath(d, "/u4", "u4=fct&api=")
		settled(t, d, 1)
		getUp(t, d)
		if page, err := postU2(d, "tasmota32.bin", esp32Image(t, "tasmota32.bin", "16.1.0")); err != nil || !strings.Contains(page, "Successful") {
			t.Fatalf("%v %s", err, page)
		}
		settled(t, d, 2)
		eq(t, "image", d.Image(), "tasmota32")
		eq(t, "version", d.Version(), "16.1.0")
	})
}
