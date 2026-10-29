package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Why (#152): the container HEALTHCHECK must probe with the scheme the listener
// really serves. The scheme is no longer a variable of its own: it follows from
// the mode (no proxy and no certificate: self-signed TLS; trusted proxies: HTTP),
// on the same port either way. A wrong scheme marks a healthy container unhealthy.
func TestHealthcheckUsesTheSchemeOfTheMode(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	for name, tc := range map[string]struct {
		srv   *httptest.Server
		extra string // environment that selects the mode
	}{
		"zero configuration probes https": {httptest.NewTLSServer(ok), ""},
		"trusted proxies probe http":      {httptest.NewServer(ok), "10.0.0.1/32"},
	} {
		t.Run(name, func(t *testing.T) {
			defer tc.srv.Close()
			u, _ := url.Parse(tc.srv.URL)
			t.Setenv("TASMOFLEET_DATA_DIR", t.TempDir())
			t.Setenv("TASMOFLEET_CONFIG_FILE", "/dev/null")
			t.Setenv("TASMOFLEET_LISTEN", u.Host)
			t.Setenv("TASMOFLEET_TRUSTED_PROXIES", tc.extra)
			if err := healthcheck(context.Background()); err != nil {
				t.Fatalf("healthy listener reported unhealthy: %v", err)
			}
		})
	}
}
