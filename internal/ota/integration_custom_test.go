//go:build integration

package ota

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// customVersion equals the upstream release of the test environment on purpose:
// a self-built image usually reports the version it was built from.
const customVersion = itRelease

// customImage is an uncompressed synthetic image carrying customVersion,
// optionally padded with incompressible bytes (above the free flash of the
// 1 MB captures, so the plan is staged).
func customImage(t *testing.T, name string, pad int) []byte {
	t.Helper()
	img, err := simulator.NewImage(itFixtures, name, customVersion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}
		if img, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	filler := make([]byte, pad)
	rand.New(rand.NewSource(2)).Read(filler) //nolint:gosec // deterministic test filler
	return append(img, filler...)
}

// recordFirmwareRequests wraps the device-facing firmware server and returns
// the paths devices asked for.
func (e *itEnv) recordFirmwareRequests() (paths func() []string, reset func()) {
	var mu sync.Mutex
	var got []string
	inner := firmware.Server(e.cache)
	e.fwHandler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK && r.Method == http.MethodGet {
			mu.Lock()
			got = append(got, r.URL.Path)
			mu.Unlock()
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) },
		func() { mu.Lock(); got = nil; mu.Unlock() }
}

func (e *itEnv) uploadCustom(img []byte) firmware.CustomImage {
	e.t.Helper()
	got, err := e.cache.(firmware.Custom).AddCustom(context.Background(), firmware.CustomInput{
		Body: bytes.NewReader(img), FileName: "my.bin", Label: "it build", Version: customVersion, Actor: "it"})
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// Why: an uploaded ESP8266 image must be treated exactly like an upstream
// one. A big image on a 1 MB device is staged: minimal first (a copy borrowed
// from upstream and served from the custom release directory), then the
// custom image, one Upgrade per hop, and the original OtaUrl restored.
func TestITCustomESP8266IsStagedViaMinimal(t *testing.T) {
	e := newItEnv(t)
	up := e.uploadCustom(customImage(t, "tasmota.bin.gz", bigImageLen))
	if up.Artifact.Filename != "tasmota.bin.gz" || up.Artifact.Source != firmware.CustomSource {
		t.Fatalf("stored %+v", up.Artifact)
	}
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev, Release: customVersion, Custom: up.Artifact.Custom})
	j := e.waitTerminal(dev)
	if j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q%s", j.State, j.LastError, e.history())
	}
	if j.Plan != model.PlanStaged || e.upgrades(dev) != 2 {
		t.Fatalf("plan %s, %d upgrades; want staged with 2", j.Plan, e.upgrades(dev))
	}
	e.settle(dev)
	if c := e.ctrl[dev]; c.Version() != customVersion || c.OtaURL() != orig {
		t.Fatalf("device runs %s, OtaUrl %q (want %s, %q)", c.Version(), c.OtaURL(), customVersion, orig)
	}
	min, err := e.st.FindCustomArtifact(context.Background(), up.Artifact.Custom, customVersion, "tasmota-minimal.bin.gz")
	if err != nil || min.Source != "custom-minimal" || j.MinimalArtifact != min.ID || j.TargetArtifact != up.Artifact.ID || j.TargetCustom != up.Artifact.Custom {
		t.Fatalf("minimal %+v (%v), job holds #%d", min, err, j.MinimalArtifact)
	}
	e.assertNoViolations()
}

// Why: the family guard has no custom exception. An ESP32 upload offered to an
// ESP8266 device is refused at submit; the device never sees an Upgrade, and
// the same upload does go to a matching ESP32 device (direct plan).
func TestITCustomImageNeverCrossesFamilies(t *testing.T) {
	e := newItEnv(t)
	up := e.uploadCustom(customImage(t, "tasmota32.bin", 0))
	wrong := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	eng := e.start(2)
	_, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: wrong, Release: customVersion, Custom: up.Artifact.Custom, SkipBackup: true}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Submit for the wrong family: %v, want ErrRefused", err)
	}
	for _, c := range e.ctrl[wrong].Commands() {
		if strings.HasPrefix(strings.ToLower(c), "upgrade 1") || strings.HasPrefix(strings.ToLower(c), "otaurl http") {
			t.Fatalf("the ESP8266 device received %q", c)
		}
	}

	right := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32})
	e.submit(eng, Request{DeviceID: right, Release: customVersion, Custom: up.Artifact.Custom})
	j := e.waitTerminal(right)
	if j.State != model.JobSucceeded || j.Plan != model.PlanDirect || e.ctrl[right].Version() != customVersion {
		t.Fatalf("job %s plan %s %q version %s%s", j.State, j.Plan, j.LastError, e.ctrl[right].Version(), e.history())
	}
	e.assertNoViolations()
}

// Why: the point of the key. Upstream 16.0.0 and a custom 16.0.0 live side by
// side; an upstream job must be served the upstream files only and a custom
// job the custom files only (also its minimal stage), as seen by what the
// devices actually downloaded. This is also the regression test that
// upstream jobs without a custom key behave as before.
func TestITCustomAndUpstreamOfTheSameVersionAreServedApart(t *testing.T) {
	e := newItEnv(t)
	up := e.uploadCustom(customImage(t, "tasmota.bin.gz", bigImageLen))
	key := up.Artifact.Custom
	paths, reset := e.recordFirmwareRequests()
	eng := e.start(2)

	a := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	e.submit(eng, Request{DeviceID: a, Release: customVersion})
	if j := e.waitTerminal(a); j.State != model.JobSucceeded || j.TargetCustom != "" {
		t.Fatalf("upstream job %s %q custom %q%s", j.State, j.LastError, j.TargetCustom, e.history())
	}
	got := strings.Join(paths(), " ")
	if !strings.Contains(got, "/fw/16.0.0/tasmota.bin.gz") || strings.Contains(got, "/c/") {
		t.Fatalf("the upstream job downloaded %s", got)
	}

	reset()
	b := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	e.submit(eng, Request{DeviceID: b, Release: customVersion, Custom: key})
	if j := e.waitTerminal(b); j.State != model.JobSucceeded || j.TargetCustom != key {
		t.Fatalf("custom job %s %q custom %q%s", j.State, j.LastError, j.TargetCustom, e.history())
	}
	got = strings.Join(paths(), " ")
	for _, want := range []string{"/c/" + key + "/fw/16.0.0/tasmota.bin.gz", "/c/" + key + "/fw/16.0.0/tasmota-minimal.bin.gz"} {
		if !strings.Contains(got, want) {
			t.Errorf("the custom job did not download %s (got %s)", want, got)
		}
	}
	for _, p := range paths() {
		if !strings.HasPrefix(p, "/c/"+key+"/") {
			t.Errorf("the custom job downloaded the non-custom %s", p)
		}
	}
	e.assertNoViolations()
}
