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
