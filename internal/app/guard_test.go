package app

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

type tokenFn func(*http.Request) string

func (f tokenFn) CSRFToken(r *http.Request) string { return f(r) }

// Why (review F1): unauthenticated clients must not make the server parse or
// buffer request bodies; the CSRF middleware reads form bodies before any role check.
func TestGuardBodies(t *testing.T) {
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	withToken := guardBodies(tokenFn(func(*http.Request) string { return "tok" }), next)
	noToken := guardBodies(tokenFn(func(*http.Request) string { return "" }), next)

	do := func(h http.Handler, method, ct, body string) *httptest.ResponseRecorder {
		reached = false
		req := httptest.NewRequest(method, "/x", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(noToken, "POST", "application/x-www-form-urlencoded", "a=b"); rec.Code != 403 || reached {
		t.Errorf("anonymous POST: %d reached=%v", rec.Code, reached)
	}
	if rec := do(withToken, "POST", "multipart/form-data; boundary=x", "--x--"); rec.Code != 415 || reached {
		t.Errorf("multipart: %d reached=%v", rec.Code, reached)
	}
	if rec := do(withToken, "POST", "application/x-www-form-urlencoded", "a=b"); rec.Code != 204 || !reached {
		t.Errorf("normal POST: %d", rec.Code)
	}
	if rec := do(noToken, "GET", "", ""); rec.Code != 204 {
		t.Errorf("GET must pass: %d", rec.Code)
	}
}

// The body cap holds even for authenticated requests.
func TestGuardBodiesCapsBody(t *testing.T) {
	var got int
	h := guardBodies(tokenFn(func(*http.Request) string { return "tok" }), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			got += n
			if err != nil {
				break
			}
		}
	}))
	req := httptest.NewRequest("POST", "/x", strings.NewReader(strings.Repeat("a", 3<<20)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got > maxFormBody {
		t.Fatalf("read %d bytes, cap is %d", got, maxFormBody)
	}
}

// Why (review F8): /metrics lists device names and backup cadence; without a
// token only a direct loopback scraper may read it.
func TestMetricsGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	cases := []struct {
		name     string
		tokenSet bool
		remote   string
		hdr      string
		want     int
	}{
		{"loopback without token", false, "127.0.0.1:1234", "", 200},
		{"remote without token", false, "172.31.12.5:1234", "", 401},
		{"via proxy on loopback", false, "127.0.0.1:1234", "203.0.113.9", 401},
		{"remote with token set is left to the registry", true, "172.31.12.5:1234", "", 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = c.remote
		if c.hdr != "" {
			req.Header.Set("X-Forwarded-For", c.hdr)
		}
		rec := httptest.NewRecorder()
		metricsGate(func() bool { return c.tokenSet }, ok).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %d want %d", c.name, rec.Code, c.want)
		}
	}
}

// Why (review F2): device cookies and header-mode CSRF tokens must survive
// a restart, so the signing key has to come from the data directory, and
// standalone mode must create its first admin.
func TestSetupAuthPersistsKeyAndSeedsInitialAdmin(t *testing.T) {
	svc := importServices(t) // AUTH_MODE=local, plain HTTP allowed
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	if err := a.setupAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(svc.Cfg.DataDir, "csrf.key")
	first, err := os.ReadFile(key)
	if err != nil || len(first) != 32 {
		t.Fatalf("csrf.key: %v (%d bytes)", err, len(first))
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(svc.Cfg.DataDir, "initial-admin-password")); err != nil {
		t.Fatalf("generated initial password file missing: %v", err)
	}
	b := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	if err := b.setupAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second, _ := os.ReadFile(key); string(first) != string(second) {
		t.Fatal("the key must not change between starts")
	}
}

// Why: the import upload is the only multipart route. Without the CSRF header
// the CSRF middleware would parse the form itself and spill files to disk, so
// the guard must refuse it; other paths stay closed to multipart.
func TestGuardBodiesAllowsMultipartOnlyForImportWithHeader(t *testing.T) {
	var reached bool
	h := guardBodies(tokenFn(func(*http.Request) string { return "tok" }), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(204)
	}))
	do := func(path, hdr string) int {
		reached = false
		req := httptest.NewRequest("POST", path, strings.NewReader("--x--"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		if hdr != "" {
			req.Header.Set("X-CSRF-Token", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("/import/tasmoadmin", "tok"); c != 204 || !reached {
		t.Errorf("import with header: %d", c)
	}
	if c := do("/import/tasmoadmin", ""); c != 415 || reached {
		t.Errorf("import without header: %d", c)
	}
	if c := do("/devices/new", "tok"); c != 415 || reached {
		t.Errorf("other path: %d", c)
	}
}

// Why: the custom firmware upload is the one large multipart route. Like the
// import it must carry the CSRF token in the header (so the CSRF middleware
// never parses the file into memory or onto disk), its body is capped at the
// firmware limit, and a wrong or missing token never reaches the handler.
func TestGuardBodiesCustomFirmwareUploadNeedsHeaderTokenAndIsCapped(t *testing.T) {
	var reached bool
	var read int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		n, _ := io.Copy(io.Discard, r.Body)
		read = n
		w.WriteHeader(204)
	})
	tokens := tokenFn(func(*http.Request) string { return "tok" })
	csrf, err := httpx.NewCSRF(tokens)
	if err != nil {
		t.Fatal(err)
	}
	h := guardBodies(tokens, csrf.Middleware(next))
	do := func(hdr string, size int) int {
		reached, read = false, 0
		req := httptest.NewRequest("POST", web.CustomUploadPath, strings.NewReader(strings.Repeat("a", size)))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		if hdr != "" {
			req.Header.Set("X-CSRF-Token", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("tok", 100); c != 204 || !reached {
		t.Errorf("valid token: %d", c)
	}
	if c := do("", 100); c != 415 || reached {
		t.Errorf("no header token: %d (a form-field token would make the middleware parse the file)", c)
	}
	if c := do("wrong", 100); c != 403 || reached {
		t.Errorf("wrong token: %d", c)
	}
	if do("tok", int(firmware.MaxCustomUpload)+4096); read > firmware.MaxCustomUpload {
		t.Errorf("handler read %d bytes, cap is %d", read, firmware.MaxCustomUpload)
	}
}

// Why (#151): the scrape token is a setting on the page. Saving it must apply to
// the very next /metrics request (no restart), removing it must close the
// endpoint to remote scrapers again, and the environment variable must keep
// winning and stay read-only (the Unraid CA template sets it that way).
func TestMetricsTokenFromSettingsAppliesLive(t *testing.T) {
	svc := importServices(t)
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	h := a.metricsHandler()
	scrape := func(bearer string) int {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "172.31.12.5:1234"
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := scrape(""); got != 401 {
		t.Fatalf("remote scrape without any token: %d, want 401", got)
	}
	apply := func(v string) {
		t.Helper()
		_, err := svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "admin", StepUp: sysconfig.StepUp{Done: true, Possible: true}, Values: map[string]string{"METRICS_TOKEN": v}})
		if err != nil {
			t.Fatal(err)
		}
	}
	apply("fake-scrape-token-1")
	if got := scrape("fake-scrape-token-1"); got != 200 {
		t.Errorf("remote scrape with the page-set token: %d, want 200", got)
	}
	if got := scrape("wrong"); got != 401 {
		t.Errorf("wrong token: %d, want 401", got)
	}
	apply("fake-scrape-token-2")
	if got := scrape("fake-scrape-token-1"); got != 401 {
		t.Errorf("replaced token still accepted: %d", got)
	}
	if got := scrape("fake-scrape-token-2"); got != 200 {
		t.Errorf("new token: %d, want 200", got)
	}
}

// Why (#151): an install that sets TASMOFLEET_METRICS_TOKEN (the Unraid CA
// template does) must keep working unchanged: the variable authenticates the
// scraper from the first request and the page cannot override it.
func TestMetricsTokenFromEnvironmentLocksAndWins(t *testing.T) {
	cfg, err := config.Load([]string{
		"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=local",
		"TASMOFLEET_DEVICE_NETWORKS=127.0.0.0/8", "TASMOFLEET_ALLOW_INSECURE_HTTP=true",
		"TASMOFLEET_METRICS_TOKEN=fake-env-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := openServices(cfg, slog.New(slog.DiscardHandler), "test", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log, bgContext: t.Context()}
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "172.31.12.5:1234"
	req.Header.Set("Authorization", "Bearer fake-env-token")
	rec := httptest.NewRecorder()
	a.metricsHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("env token scrape: %d, want 200", rec.Code)
	}
	_, err = svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "admin", StepUp: sysconfig.StepUp{Done: true, Possible: true}, Values: map[string]string{"METRICS_TOKEN": "page"}})
	if !errors.Is(err, sysconfig.ErrLocked) {
		t.Fatalf("page change of an env-locked token: %v, want ErrLocked", err)
	}
}
