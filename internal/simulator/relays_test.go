package simulator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Why: the OTA engine restores relays that were ON, so the simulator must
// reproduce when real Tasmota loses them: never over a plain soft restart of
// an ESP8266, always through the minimal image and on an ESP32.

func TestMultiRelayStatusAndPower(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "strip", Profile: ProfileESP8266_4M, Relays: 3})
	cm(t, d, "Power1 OFF")
	cm(t, d, "Power2 ON")
	got := cm(t, d, "Status 11")
	for _, want := range []string{`"POWER1":"OFF"`, `"POWER2":"ON"`, `"POWER3":"OFF"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Status 11 lacks %s: %s", want, got)
		}
	}
	if got := cm(t, d, "Power3 ON"); !strings.Contains(got, `{"POWER3":"ON"}`) {
		t.Errorf("Power3 reply %s", got)
	}
	cm(t, d, "Power0 OFF")
	eq(t, "all relays after Power0 OFF", len(d.Relays()), 3)
	for i, on := range d.Relays() {
		if on {
			t.Errorf("relay %d still ON after Power0 OFF", i+1)
		}
	}
	if got := cm(t, d, "Power4 ON"); !strings.Contains(got, "Unknown") {
		t.Errorf("relay 4 of a 3 relay device: %s", got)
	}
}

func TestSingleRelayStillReportsPOWER(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
	cm(t, d, "Power1 ON")
	if got := cm(t, d, "Status 11"); !strings.Contains(got, `"POWER":"ON"`) {
		t.Errorf("Status 11: %s", got)
	}
}

func TestRelayStateAcrossFlashing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile Profile
		file    string
		hops    int
		keeps   bool
		so0     *bool
	}{
		{"esp8266 relay keeps it (RTC RAM)", ProfileESP8266_4M, "tasmota.bin.gz", 1, true, nil},
		{"esp32c3 without SetOption0 never restores", ProfileESP32C3, "tasmota32c3.bin", 1, false, boolp(false)},
		{"esp32 capture has SetOption0 1 and keeps it", ProfileESP32, "tasmota32.bin", 1, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFirmwareServer(t, map[string]string{tc.file: "16.1.0"})
			f := newTestFleet(t, "", Extra{})
			d := add(t, f, DeviceSpec{Name: "dev", Profile: tc.profile, Version: "16.0.0", SetOption0: tc.so0})
			cm(t, d, "Power1 ON")
			cm(t, d, "OtaUrl "+fw.URL+"/"+tc.file)
			cm(t, d, "Upgrade 1")
			settled(t, d, tc.hops)
			eq(t, "relay after the update", d.Power(), tc.keeps)
		})
	}
}

func TestRelayStateKeptThroughMinimal(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota-minimal.bin.gz": "16.1.0", "tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_1M, Version: "16.0.0", Relays: 2})
	d.SetRelay(1, true)
	d.SetRelay(2, true)
	cm(t, d, "OtaUrl "+fw.URL+"/tasmota-minimal.bin.gz")
	cm(t, d, "Upgrade 1")
	settled(t, d, 1)
	cm(t, d, "OtaUrl "+fw.URL+"/tasmota.bin.gz")
	cm(t, d, "Upgrade 1")
	settled(t, d, 2)
	for i, on := range d.Relays() {
		if !on {
			t.Errorf("relay %d is OFF after the two-step update; relays survive the minimal hop (observed on the fleet)", i+1)
		}
	}
}

// Why: the fleet's lights (MagicHome PWM) come back ON for about 4 ms after the
// boot and then switch themselves OFF (observed 2026-10-19); they report
// Dimmer and CT in Status 11, which is how the engine recognises a light.
func TestLightComesBackOnThenOff(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_4M, Version: "16.0.0", Light: true})
	cm(t, d, "Power1 ON")
	cm(t, d, "OtaUrl "+fw.URL+"/tasmota.bin.gz")
	cm(t, d, "Upgrade 1")
	settled(t, d, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.WaitFor(ctx, func(c Controller) bool { return !c.Power() }); err != nil {
		t.Fatalf("the light never switched itself off: %v", err)
	}
	if got := cm(t, d, "Status 11"); !strings.Contains(got, `"Dimmer":100`) {
		t.Errorf("a light reports its Dimmer in Status 11: %s", got)
	}
}

// Why: the two ESP32 lamps of the fleet run SetOption0 1 and do come back
// with their state; a simulator that always clears would hide the case where
// the restore must not act.
func TestSetOption0KeepsStateOnEveryChipAndImage(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota32c3.bin": "16.1.0", "tasmota-minimal.bin.gz": "16.1.0", "tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	esp32 := add(t, f, DeviceSpec{Name: "lamp32", Profile: ProfileESP32C3, Version: "16.0.0", SetOption0: boolp(true)})
	cm(t, esp32, "Power1 ON")
	cm(t, esp32, "OtaUrl "+fw.URL+"/tasmota32c3.bin")
	cm(t, esp32, "Upgrade 1")
	settled(t, esp32, 1)
	eq(t, "ESP32-C3 with SetOption0 1 after the update", esp32.Power(), true)

	lamp := add(t, f, DeviceSpec{Name: "lamp8266", Profile: ProfileESP8266_1M, Version: "16.0.0", SetOption0: boolp(true)})
	cm(t, lamp, "Power1 ON")
	cm(t, lamp, "OtaUrl "+fw.URL+"/tasmota-minimal.bin.gz")
	cm(t, lamp, "Upgrade 1")
	settled(t, lamp, 1)
	cm(t, lamp, "OtaUrl "+fw.URL+"/tasmota.bin.gz")
	cm(t, lamp, "Upgrade 1")
	settled(t, lamp, 2)
	eq(t, "ESP8266 with SetOption0 1 through minimal", lamp.Power(), true)
}

func TestPulseTimeAndInterlockReads(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "door", Profile: ProfileESP8266_4M, Relays: 2, PulseTimes: []int{0, 100}, Interlock: "1,2"})
	if got := cm(t, d, "PulseTime"); !strings.Contains(got, `"Set":[0,100,0,0,0,0,0,0]`) {
		t.Errorf("PulseTime: %s", got)
	}
	if got := cm(t, d, "Interlock"); !strings.Contains(got, `"Interlock":"ON","Groups":"1,2"`) {
		t.Errorf("Interlock: %s", got)
	}
	plain := add(t, f, DeviceSpec{Name: "plain", Profile: ProfileESP8266_4M})
	if got := cm(t, plain, "Interlock"); !strings.Contains(got, `"Interlock":"OFF"`) {
		t.Errorf("Interlock without groups: %s", got)
	}
}

func boolp(b bool) *bool { return &b }

func TestSetOption0OffOverridesTheCapture(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota32.bin": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP32, Version: "16.0.0", SetOption0: boolp(false)})
	cm(t, d, "Power1 ON")
	cm(t, d, "OtaUrl "+fw.URL+"/tasmota32.bin")
	cm(t, d, "Upgrade 1")
	settled(t, d, 1)
	eq(t, "ESP32 with SetOption0 forced off", d.Power(), false)
}

// Why: only a device with a light driver reports Dimmer/Color/CT in Status 11;
// the engine uses that to restore lights and leave plain relays alone, so a
// plain simulated device must not inherit the light state of a lamp capture.
func TestOnlyLightsReportLightState(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	relay := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32})
	light := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP32, Light: true})
	if got := cm(t, relay, "Status 11"); strings.Contains(got, "Dimmer") || strings.Contains(got, `"Color"`) || strings.Contains(got, `"POWER":`) == false {
		t.Errorf("plain relay: %s", got)
	}
	if got := cm(t, light, "Status 11"); !strings.Contains(got, `"Dimmer":100`) {
		t.Errorf("light: %s", got)
	}
}

// Why: the design fixture (design/v2/README.md) must
// start 60 devices with the prototype's power state, TelePeriod, PowerOnState
// and MqttHost. Without spec-level initial state the devenv would have to send
// commands at start-up, and a command that changes MqttHost restarts the device.
func TestSpecSetsInitialPowerAndSettings(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{
		Name: "switch", Profile: ProfileESP8266_4M, Relays: 2,
		Power:    []bool{true, false},
		Settings: map[string]string{"teleperiod": "60", "poweronstate": "0", "mqtthost": "mqtt.lab.home"},
	})
	got := cm(t, d, "Status 11")
	for _, want := range []string{`"POWER1":"ON"`, `"POWER2":"OFF"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Status 11 lacks %s: %s", want, got)
		}
	}
	for cmnd, want := range map[string]string{"TelePeriod": `"TelePeriod":60`, "PowerOnState": `"PowerOnState":0`, "MqttHost": `"MqttHost":"mqtt.lab.home"`} {
		if got := cm(t, d, cmnd); !strings.Contains(got, want) {
			t.Errorf("%s answers %s, want %s", cmnd, got, want)
		}
	}
	// A device without overrides keeps what the capture says.
	plain := add(t, f, DeviceSpec{Name: "plain", Profile: ProfileESP8266_4M})
	if got := cm(t, plain, "TelePeriod"); strings.Contains(got, `"TelePeriod":60`) {
		t.Errorf("settings leaked into another device: %s", got)
	}
}
