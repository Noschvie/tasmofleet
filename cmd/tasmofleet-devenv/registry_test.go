package main

import (
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"net/http"
	"net/http/httptest"
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
