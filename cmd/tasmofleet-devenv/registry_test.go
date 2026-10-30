package main

import (
	"bufio"
	"context"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Why: the e2e tests pull plugs and press buttons through the control server by device name. A
// name or action it does not know must answer 404, not silently do nothing, or a typo in a test
// would look like "the app did not notice".
func TestDeviceControlRefusesWhatItDoesNotKnow(t *testing.T) {
	mux := http.NewServeMux()
	reg := &deviceRegistry{byName: map[string]simulator.Device{}}
	mux.HandleFunc("POST /device/{name}/{action}", reg.act)
	mux.HandleFunc("POST /device/{name}/relay/{n}/{state}", reg.relay)
	for _, path := range []string{"/device/nobody/off", "/device/nobody/relay/1/on"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
	}
}

// Why: the Overview e2e tests need one device per mockup shape, but the base fleet is counted by
// other tests; the shapes are opt-in and must all start.
func TestOverviewShapesAreOptInAndAllStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02", "-overview-shapes"}, pw, io.Discard)
		_ = pw.Close()
	}()
	var lines []string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	text := strings.Join(lines, "\n")
	for _, s := range overviewDevices() {
		if !strings.Contains(text, "name="+s.Name+" ") {
			t.Errorf("shape %s did not start", s.Name)
		}
	}
}

// Why: the MQTT transport e2e tests need devices TasmoFleet can reach only over the broker, but
// the base fleet is counted by other tests; they are opt-in, must all start, and must really be
// cut off from HTTP.
func TestMQTTTransportDevicesAreOptInAndHTTPIsCutOff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02", "-mqtt-transport"}, pw, io.Discard)
		_ = pw.Close()
	}()
	var lines []string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	text := strings.Join(lines, "\n")
	for _, s := range mqttTransportDevices() {
		if !strings.Contains(text, "name="+s.Name+" ") {
			t.Errorf("device %s did not start", s.Name)
		}
	}
	var addr string
	for _, l := range lines {
		if strings.Contains(l, "name=mq-esp32 ") {
			for _, f := range strings.Fields(l) {
				if v, ok := strings.CutPrefix(f, "addr="); ok {
					addr = v
				}
			}
		}
	}
	if addr == "" {
		t.Fatalf("no address for mq-esp32 in %q", text)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/cm?cmnd=Status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Error("mq-esp32 answers over HTTP")
	}
}

// Why: other tests count the base fleet; the MQTT transport devices must not
// appear unless asked for.
func TestMQTTTransportDevicesAreAbsentByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02"}, pw, io.Discard)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	for _, l := range lines {
		if strings.Contains(l, "name=mq-") {
			t.Errorf("unrequested device: %s", l)
		}
	}
	if len(lines) < 5 {
		t.Errorf("the fleet did not start: %v", lines)
	}
}
