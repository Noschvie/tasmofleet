//go:build integration

package ota

import (
	"context"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// uploadsOf counts the image uploads a simulated device received.
func (e *itEnv) uploadsOf(dev model.DeviceID) int {
	n := 0
	for _, c := range e.ctrl[dev].Commands() {
		if strings.HasPrefix(c, "POST /u2 ") && !strings.Contains(c, "no upload type") {
			n++
		}
	}
	return n
}

// assertFlashed fails unless the device received the command that carries the expected file,
// and ends on the image tag every ESP32 chip build reports.
func (e *itEnv) assertFlashed(dev model.DeviceID, want string) {
	e.t.Helper()
	found := false
	for _, c := range e.ctrl[dev].Commands() {
		found = found || strings.HasPrefix(c, want) || strings.HasSuffix(c, want)
	}
	if !found {
		e.t.Fatalf("no command %q: %v", want, e.ctrl[dev].Commands())
	}
	if got := e.ctrl[dev].Image(); got != "tasmota32" {
		e.t.Fatalf("device runs image %q, want tasmota32", got)
	}
}

func (e *itEnv) assertNoPullCommands(dev model.DeviceID) {
	e.t.Helper()
	for _, c := range e.ctrl[dev].Commands() {
		lc := strings.ToLower(c)
		if strings.HasPrefix(lc, "otaurl http") || strings.HasPrefix(lc, "upgrade 1") {
			e.t.Fatalf("a push job sent %q", c)
		}
	}
}

// The push delivery end to end on an ESP32: the image arrives by upload, the
// device runs the new version, and neither Upgrade nor an OtaUrl change was sent.
func TestITPushESP32(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if j.Delivery != model.DeliveryPush {
		t.Fatalf("delivery %q", j.Delivery)
	}
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads, want 1: %v", n, e.ctrl[dev].Commands())
	}
	e.assertNoPullCommands(dev)
	if got := e.ctrl[dev].Image(); got != "tasmota32" {
		t.Fatalf("device runs image %q", got)
	}
}

// Every ESP32 chip gets the image of its own family by upload. All chip builds report the image
// tag "tasmota32" (#85), so the uploaded file name proves the build.
func TestITPushPerChip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile simulator.Profile
		file    string
	}{
		{"esp32c3", simulator.ProfileESP32C3, "tasmota32c3.bin"},
		{"esp32s3", simulator.ProfileESP32S3, "tasmota32s3.bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newItEnv(t)
			dev := e.addDevice(simulator.DeviceSpec{Profile: tc.profile})
			orig := e.ctrl[dev].OtaURL()
			eng := e.start(2)
			e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
			e.expectSuccess(dev, orig, 0, model.PlanDirect)
			e.assertFlashed(dev, "POST /u2 "+tc.file+" ")
		})
	}
}

// A password-protected device: both requests of the upload carry the
// inventory credentials.
func TestITPushPasswordProtectedDevice(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, User: "admin", Password: fakePassword})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	e.expectSuccess(dev, orig, 0, model.PlanDirect)
}

// Push where possible: in a mixed batch the ESP32 is updated by upload and
// the ESP8266 by its normal download; nothing is refused.
func TestITPushWherePossibleMixedBatch(t *testing.T) {
	e := newItEnv(t)
	esp32 := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	plug := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	orig32, origPlug := e.ctrl[esp32].OtaURL(), e.ctrl[plug].OtaURL()
	eng := e.start(2)
	if _, err := eng.Submit(context.Background(), "it", []Request{
		{DeviceID: esp32, Release: itRelease, SkipBackup: true, Delivery: model.DeliveryPush},
		{DeviceID: plug, Release: itRelease, SkipBackup: true, Delivery: model.DeliveryPush},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if j := e.expectSuccess(esp32, orig32, 0, model.PlanDirect); j.Delivery != model.DeliveryPush {
		t.Fatalf("ESP32 job delivery %q", j.Delivery)
	}
	e.assertNoPullCommands(esp32)
	if j := e.expectSuccess(plug, origPlug, 1, model.PlanDirect); j.Delivery != model.DeliveryPull {
		t.Fatalf("ESP8266 job delivery %q", j.Delivery)
	}
	if n := e.uploadsOf(plug); n != 0 {
		t.Fatalf("%d uploads to an ESP8266", n)
	}
}

// The reply of the upload is lost, but the device flashed: the engine probes,
// sees the new version and must not upload a second time.
func TestITPushLostReplyIsNotResent(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultUploadLostReply}}})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	e.expectSuccess(dev, orig, 0, model.PlanDirect)
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads after a lost reply, want 1", n)
	}
}

// The connection is cut halfway: nothing is flashed, the job tries again only
// after probing, stops after the attempts it has, and the device keeps its
// old version.
func TestITPushCutBodyFailsWithoutFlashing(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultUploadCut}}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	e.settle(dev)
	if c := e.ctrl[dev]; c.Version() != "15.6.0" {
		t.Fatalf("device runs %s", c.Version())
	}
	if n := e.uploadsOf(dev); n < 1 || n > e.machine.MaxAttempts {
		t.Fatalf("%d uploads, want 1..%d", n, e.machine.MaxAttempts)
	}
	e.assertNoPullCommands(dev)
}

// The device refuses the image (wrong family fault): the job fails with the
// reason, the device is not flashed.
func TestITPushRefusedByDevice(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultWrongFamily}}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	e.settle(dev)
	if c := e.ctrl[dev]; c.Version() != "15.6.0" {
		t.Fatalf("device runs %s", c.Version())
	}
	e.assertNoViolations()
}

// A safeboot-layout ESP32 answers "Not enough space" (office-monitor-thomas,
// 2026-10-24): the job fails at once with the reason, after one upload,
// instead of waiting out the upgrade window; the device keeps its version and
// never reboots.
func TestITPushNoSpaceFailsAtOnce(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, Faults: []simulator.Fault{{Kind: simulator.FaultUploadNoSpace}}})
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Delivery: model.DeliveryPush})
	j := e.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "Not enough space") || !strings.Contains(j.LastError, "nothing was flashed") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if waited := j.UpdatedAt.Sub(j.CreatedAt); waited >= e.machine.UpgradeAccept {
		t.Fatalf("job took %s, it waited for the upgrade window (%s)", waited, e.machine.UpgradeAccept)
	}
	if n := e.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads, want 1", n)
	}
	e.settle(dev)
	if c := e.ctrl[dev]; c.Version() != "15.6.0" || c.Boots() != 0 {
		t.Fatalf("device runs %s after %d boots", c.Version(), c.Boots())
	}
	e.assertNoViolations()
}
