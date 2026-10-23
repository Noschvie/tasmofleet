package simulator

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v want %v", what, got, want)
	}
}

// Why: the 1 MB staged flow is the riskiest real-world path; the timeline
// must match docs/research (pending, blocking download, restart wait,
// reboot gap) and the OtaUrl rule must hold: the minimal URL persisted in
// step 1 is what the full firmware boots with.
func TestStagedUpdateTimelineAndOtaUrlRule(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota-minimal.bin.gz": "16.1.0", "tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_1M, Version: "16.0.0"})
	minURL, fullURL := fw.URL+"/tasmota-minimal.bin.gz", fw.URL+"/tasmota.bin.gz"

	cm(t, d, "OtaUrl "+minURL)
	if got := cm(t, d, "Upgrade 1"); !strings.Contains(got, "Version 16.0.0 from "+minURL) {
		t.Fatal(got)
	}
	settled(t, d, 1)
	eq(t, "image after step 1", d.Image(), "minimal")
	eq(t, "version after step 1", d.Version(), "16.1.0")
	want := []Phase{PhaseRun, PhasePending, PhaseDownloading, PhaseRestartWait, PhaseRebooting, PhaseRun}
	if got := phases(d); !slices.Equal(got, want) {
		t.Fatalf("timeline %v want %v", got, want)
	}
	if got := cm(t, d, "Status 2"); !strings.Contains(got, `"Command":"Unknown"`) {
		t.Errorf("minimal must not answer Status: %s", got)
	}
	eq(t, "persisted OtaUrl in minimal", d.PersistedOtaURL(), minURL)

	cm(t, d, "OtaUrl "+fullURL)
	eq(t, "RAM OtaUrl", d.OtaURL(), fullURL)
	cm(t, d, "Upgrade 1")
	settled(t, d, 2)
	eq(t, "image after step 2", d.Image(), "tasmota")
	eq(t, "OtaUrl after step 2 is the minimal URL (minimal never persists)", d.OtaURL(), minURL)
	if got := cm(t, d, "Status 2"); !strings.Contains(got, `"Version":"16.1.0(release-tasmota)"`) {
		t.Error(got)
	}
	eq(t, "BootCount counts normal boots", d.BootCount(), 219)
}

// Why: engines must not trust an OtaUrl set while minimal runs: a reboot
// discards it.
func TestOtaUrlSetInMinimalIsLostAfterReboot(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266Min})
	before := d.PersistedOtaURL()
	cm(t, d, "OtaUrl http://127.0.0.1:1/x.bin.gz")
	eq(t, "RAM", d.OtaURL(), "http://127.0.0.1:1/x.bin.gz")
	d.PowerCycle()
	settled(t, d, 1)
	eq(t, "after reboot", d.OtaURL(), before)
}

// Why: on a normal image OtaUrl survives a software restart (SettingsSaveAll).
func TestOtaUrlSetInNormalSurvivesRestart(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
	cm(t, d, "OtaUrl http://127.0.0.1:1/a.bin.gz")
	cm(t, d, "Restart 1")
	settled(t, d, 1)
	eq(t, "OtaUrl", d.OtaURL(), "http://127.0.0.1:1/a.bin.gz")
}

// Why: Tasmota's own fallback must continue without any command from the
// manager, otherwise a 1 MB device would sit in minimal forever.
func TestAutoFallbackContinuesOnItsOwn(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota-minimal.bin.gz": "16.1.0", "tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_1M})
	full := fw.URL + "/tasmota.bin.gz"
	cm(t, d, "OtaUrl "+full)
	cm(t, d, "Upgrade 1")
	settled(t, d, 2)
	eq(t, "final image", d.Image(), "tasmota")
	eq(t, "version", d.Version(), "16.1.0")
	eq(t, "OtaUrl stays the original one", d.OtaURL(), full)
	if fw.hits["tasmota-minimal.bin.gz"] != 1 || fw.hits["tasmota.bin.gz"] < 2 {
		t.Errorf("hits %v", fw.hits)
	}
	sent := 0
	for _, c := range d.Commands() {
		if strings.HasPrefix(c, "Upgrade") {
			sent++
		}
	}
	eq(t, "Upgrade commands sent", sent, 1)
}

// Why: 4 MB ESP8266 and ESP32 go direct: one reboot, no minimal hop.
func TestDirectUpdates(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0", "tasmota32.bin": "16.1.0", "tasmota32c3.bin": "16.1.0", "tasmota32s3.bin": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	for _, tc := range []struct {
		p    Profile
		file string
	}{{ProfileESP8266_4M, "tasmota.bin.gz"}, {ProfileESP32, "tasmota32.bin"}, {ProfileESP32C3, "tasmota32c3.bin"}, {ProfileESP32S3, "tasmota32s3.bin"}} {
		d := add(t, f, DeviceSpec{Name: string(tc.p), Profile: tc.p})
		cm(t, d, "OtaUrl "+fw.URL+"/"+tc.file)
		cm(t, d, "Upgrade 1")
		settled(t, d, 1)
		eq(t, string(tc.p)+" version", d.Version(), "16.1.0")
		if d.Minimal() {
			t.Errorf("%s hopped through minimal", tc.p)
		}
	}
}

// Why: the guard is the last defence; the device must also refuse an image
// of the wrong family like the real bootloader check does, and stay usable.
func TestWrongFamilyImageFailsAndDeviceStaysUp(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0", "tasmota32c3.bin": "16.1.0", "tasmota32.bin": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	for _, tc := range []struct {
		p    Profile
		file string
	}{{ProfileESP32, "tasmota.bin.gz"}, {ProfileESP8266_4M, "tasmota32.bin"}, {ProfileESP32, "tasmota32c3.bin"}} {
		d := add(t, f, DeviceSpec{Name: string(tc.p) + tc.file, Profile: tc.p})
		cm(t, d, "OtaUrl "+fw.URL+"/"+tc.file)
		cm(t, d, "Upgrade 1")
		ctx, cancel := context.WithTimeout(ctxT(t), 15*time.Second)
		err := d.WaitFor(ctx, func(c Controller) bool { return c.DownloadAttempts() > 0 && c.Phase() == PhaseRun })
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		eq(t, string(tc.p)+" boots", d.Boots(), 0)
		eq(t, string(tc.p)+" version", d.Version(), "16.0.0")
	}
}

// Why: during the blocking download the real web server accepts the
// connection and never answers; during the reboot the port is closed.
func TestBlockingAndRefusingWindows(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{TimeScale: 0.2})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
	cm(t, d, "OtaUrl "+fw.URL+"/tasmota.bin.gz")
	cm(t, d, "Upgrade 1")
	ctx := ctxT(t)
	if err := d.WaitFor(ctx, func(c Controller) bool { return c.Phase() == PhaseDownloading }); err != nil {
		t.Fatal(err)
	}
	_, _, err := cmWith(d, "Status 2", 200*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("download window must hang (accept, no answer), got %v", err)
	}
	if err := d.WaitFor(ctx, func(c Controller) bool { return c.Phase() == PhaseRebooting }); err != nil {
		t.Fatal(err)
	}
	_, _, err = cmWith(d, "Status 2", time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("reboot window must refuse connections, got %v", err)
	}
	settled(t, d, 1)
	if _, _, err := cmWith(d, "Status 2", time.Second, nil); err != nil {
		t.Errorf("same port must answer again: %v", err)
	}
}

// Why: a download failure must leave the device on the old firmware with a
// working web server, after the documented retry count.
func TestDownloadFailureKeepsOldFirmware(t *testing.T) {
	fw := newFirmwareServer(t, nil)
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
	cm(t, d, "OtaUrl "+fw.URL+"/missing.bin.gz")
	cm(t, d, "Upgrade 1")
	ctx := ctxT(t)
	if err := d.WaitFor(ctx, func(c Controller) bool { return c.DownloadAttempts() == 10 && c.Phase() == PhaseRun }); err != nil {
		t.Fatalf("%v attempts %d", err, d.DownloadAttempts())
	}
	eq(t, "boots", d.Boots(), 0)
	eq(t, "server hits", fw.hits["missing.bin.gz"], 10)
	cm(t, d, "Upgrade 1") // a failed upgrade does not wedge the device
}

func TestWebUIStartsUpgrade(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M})
	body, code, err := cmWithPath(d, "/u1", "o="+fw.URL+"/tasmota.bin.gz")
	if err != nil || code != 200 || !strings.Contains(body, "Upgrade started") {
		t.Fatal(body, code, err)
	}
	settled(t, d, 1)
	eq(t, "version", d.Version(), "16.1.0")
}
