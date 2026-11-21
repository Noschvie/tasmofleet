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

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/web"
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
// token only a direct loopback scraper may read it, with one only the exact
// bearer token (constant-time) is accepted, from any peer.
func TestMetricsGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	cases := []struct {
		name, token, remote, hdr, auth string
		want                           int
	}{
		{"loopback without token", "", "127.0.0.1:1234", "", "", 200},
		{"remote without token", "", "172.31.12.5:1234", "", "", 401},
		{"via proxy on loopback", "", "127.0.0.1:1234", "203.0.113.9", "", 401},
		{"remote, token set, none sent", "t0k", "172.31.12.5:1234", "", "", 401},
		{"remote, token set, wrong", "t0k", "172.31.12.5:1234", "", "Bearer nope", 401},
		{"remote, token set, other scheme", "t0k", "172.31.12.5:1234", "", "Basic t0k", 401},
		{"loopback, token set, none sent", "t0k", "127.0.0.1:1234", "", "", 401},
		{"remote, token set, right", "t0k", "172.31.12.5:1234", "", "Bearer t0k", 200},
		{"scheme is case-insensitive", "t0k", "172.31.12.5:1234", "", "bearer t0k", 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = c.remote
		if c.hdr != "" {
			req.Header.Set("X-Forwarded-For", c.hdr)
		}
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		metricsGate(func() string { return c.token }, ok).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %d want %d", c.name, rec.Code, c.want)
		}
	}
}

// Why (security review): the token is cleared on the settings page while a
// request is in flight. The gate must decide on ONE read of the token; a second
// read that sees "" must never turn an unauthenticated remote request into a
// served one. The token source flips after its first call (deterministic, no sleep).
func TestMetricsGateReadsTheTokenOnce(t *testing.T) {
	served := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(200) })
	for _, flip := range [][2]string{{"t0k", ""}, {"", "t0k"}} {
		calls := 0
		gate := metricsGate(func() string {
			calls++
			if calls == 1 {
				return flip[0]
			}
			return flip[1]
		}, next)
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "172.31.12.5:1234"
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)
		if rec.Code != 401 || served != 0 {
			t.Errorf("flip %q: remote request without bearer got %d (served %d)", flip, rec.Code, served)
		}
		if calls != 1 {
			t.Errorf("flip %q: token read %d times, want once per request", flip, calls)
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
	_, err := svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "admin", StepUp: sysconfig.StepUp{Done: true, Possible: true}, Clear: []string{"METRICS_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := scrape("fake-scrape-token-2"); got != 401 {
		t.Errorf("cleared token: remote peer got %d, want 401 (loopback-only rule)", got)
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

type sessionFn struct {
	token string // CSRF token the caller's session (or pre-session cookie) expects
	valid bool   // the request carries a valid session
	err   error  // a resolve error that is not "unauthenticated"
}

func (s sessionFn) CSRFToken(*http.Request) string { return s.token }
func (s sessionFn) Principal(*http.Request) (model.Principal, error) {
	if s.valid {
		return model.Principal{Subject: "ann"}, nil
	}
	if s.err != nil {
		return model.Principal{}, s.err
	}
	return model.Principal{}, auth.ErrUnauthenticated
}

// Why (#508): a person whose session expired must still be able to sign out.
// Their session has no CSRF token, so the generic chain answered 403 and the
// cookie stayed. POST /logout without a valid session now reaches the handler,
// but nothing else gains from that: a valid session keeps the CSRF check on
// /logout, and every other unsafe route stays closed without a session.
func TestGuardUnsafeLogoutWithoutSession(t *testing.T) {
	var hits []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	do := func(s sessionFn, method, path, body string) int {
		hits = nil
		csrf, err := httpx.NewCSRF(s)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		guardUnsafe(s, http.NewCrossOriginProtection(), csrf.Middleware, next).ServeHTTP(rec, req)
		return rec.Code
	}
	expired := sessionFn{}                       // no token can exist
	staleLogin := sessionFn{token: "pre-cookie"} // a pre-session login cookie that does not match the old form token
	valid := sessionFn{token: "tok", valid: true}
	if c := do(expired, "POST", "/logout", "csrf=old"); c != 204 || len(hits) != 1 {
		t.Errorf("expired session: %d %v, want the logout handler reached", c, hits)
	}
	if c := do(staleLogin, "POST", "/logout", "csrf=old"); c != 204 || len(hits) != 1 {
		t.Errorf("stale pre-session cookie: %d %v, want the logout handler reached", c, hits)
	}
	if c := do(valid, "POST", "/logout", "csrf=wrong"); c != 403 || len(hits) != 0 {
		t.Errorf("valid session, wrong token: %d %v, want refused", c, hits)
	}
	if c := do(valid, "POST", "/logout", ""); c != 403 || len(hits) != 0 {
		t.Errorf("valid session, no token: %d %v, want refused", c, hits)
	}
	if c := do(valid, "POST", "/logout", "csrf=tok"); c != 204 || len(hits) != 1 {
		t.Errorf("valid session, right token: %d %v, want signed out", c, hits)
	}
	for _, path := range []string{"/logout/", "/login/local", "/users", "/devices/new", "/account/password", "/x/logout"} {
		if c := do(expired, "POST", path, "csrf=old"); c != 403 || len(hits) != 0 {
			t.Errorf("POST %s without a session: %d %v, want refused (only /logout is exempt)", path, c, hits)
		}
	}
	for _, m := range []string{"PUT", "DELETE", "PATCH"} {
		if c := do(expired, m, "/logout", ""); c != 403 || len(hits) != 0 {
			t.Errorf("%s /logout without a session: %d %v, want refused", m, c, hits)
		}
	}
}

// Why (#508): the exemption must not widen. Every request below reaches the
// handler only through the full chain, so without a token each is refused: wrong
// method, any path variant (trailing or doubled slash, query, encoded letters,
// case), and a session lookup that failed for a reason other than "no session".
func TestGuardUnsafeLogoutExemptionIsExact(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(204) })
	cases := []struct {
		name, method, target string
		s                    sessionFn
	}{
		{"trailing slash", "POST", "/logout/", sessionFn{}},
		{"double slash", "POST", "//logout", sessionFn{}},
		{"query", "POST", "/logout?x=1", sessionFn{}},
		{"empty query", "POST", "/logout?", sessionFn{}},
		{"encoded letter", "POST", "/%6cogout", sessionFn{}},
		{"upper case", "POST", "/LOGOUT", sessionFn{}},
		{"dot segment", "POST", "/x/../logout", sessionFn{}},
		{"prefix", "POST", "/logout2", sessionFn{}},
		{"PUT", "PUT", "/logout", sessionFn{}},
		{"DELETE", "DELETE", "/logout", sessionFn{}},
		{"PATCH", "PATCH", "/logout", sessionFn{}},
		{"resolve error other than unauthenticated", "POST", "/logout", sessionFn{err: errors.New("database down")}},
		{"valid session, no token", "POST", "/logout", sessionFn{valid: true}},
	}
	for _, c := range cases {
		reached = false
		csrf, err := httpx.NewCSRF(c.s)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(c.method, c.target, strings.NewReader("csrf=old"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		guardUnsafe(c.s, http.NewCrossOriginProtection(), csrf.Middleware, next).ServeHTTP(rec, req)
		if rec.Code != 403 || reached {
			t.Errorf("%s: %d reached=%v, want 403 from the full chain", c.name, rec.Code, reached)
		}
	}
	// GET and HEAD are safe methods: they never needed a token and pass as before.
	for _, m := range []string{"GET", "HEAD"} {
		reached = false
		s := sessionFn{}
		csrf, _ := httpx.NewCSRF(s)
		rec := httptest.NewRecorder()
		guardUnsafe(s, http.NewCrossOriginProtection(), csrf.Middleware, next).ServeHTTP(rec, httptest.NewRequest(m, "/logout", nil))
		if rec.Code != 204 {
			t.Errorf("%s /logout: %d", m, rec.Code)
		}
	}
}

// Why (#508): the exemption skips the token, never the origin check. A form that
// another site posts to /logout never carries the session cookie (SameSite=Lax),
// so it always looks like a request without a session, and the cookie-clearing
// answer would still sign that browser out of its valid session. The person's
// own tab is same-origin and still signs out.
func TestGuardUnsafeLogoutKeepsTheOriginCheck(t *testing.T) {
	cases := []struct {
		name    string
		header  map[string]string
		reached bool
	}{
		{"another site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}, false},
		{"a sibling subdomain", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"another origin, browser without Sec-Fetch-Site", map[string]string{"Origin": "http://evil.example"}, false},
		{"the person's own tab", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}, true},
		{"own origin, browser without Sec-Fetch-Site", map[string]string{"Origin": "http://example.com"}, true},
		{"no browser (neither header)", nil, true},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) })
		s := sessionFn{} // no session: the request the exemption is for
		csrf, err := httpx.NewCSRF(s)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader("csrf=old")) // Host: example.com
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		guardUnsafe(s, http.NewCrossOriginProtection(), csrf.Middleware, next).ServeHTTP(rec, req)
		if reached != c.reached || (!c.reached && rec.Code != http.StatusForbidden) {
			t.Errorf("%s: %d reached=%v, want reached=%v", c.name, rec.Code, reached, c.reached)
		}
	}
}

// untouchedBody fails the test when anything reads it: a request without a
// session must be answered without its body being parsed or buffered.
type untouchedBody struct{ t *testing.T }

func (b untouchedBody) Read([]byte) (int, error) {
	b.t.Error("the body of a request without a session was read")
	return 0, io.EOF
}

// Why (#717): a tab left open past the session's end still shows its htmx
// buttons. An expired session has no CSRF token, so the guard answered the
// click with a plain 403 "missing or invalid CSRF token", and htmx swapped that
// text into the page where the row or card had been. Without a session the
// click must lead to the sign-in page, the way Require answers htmx: 401 with
// HX-Redirect (back to the page's request, saying when the session expired),
// and still without reading the body. The CSRF check itself does not soften:
// a valid session with a missing or wrong token stays refused with 403, a
// request that is not htmx keeps the plain 403, and so does a header principal
// with a reserved name (Require answers that one with 403, never sign-in).
// Only unsafe requests are affected: a GET carries no token and goes on to the
// route as before (its Require answers htmx itself), and a sign-out without a
// session keeps the #508 exemption, so the handler still clears the cookie.
func TestGuardUnsafeSendsHTMXWithoutSessionToSignIn(t *testing.T) {
	cases := []struct {
		name     string
		target   string // "METHOD path"; "" is an htmx action, POST /devices/7/power/1?v=2
		s        sessionFn
		htmx     bool
		body     string // "" sends a body that fails the test when read
		want     int
		redirect string // the HX-Redirect header wanted
	}{
		{"htmx, no session", "", sessionFn{}, true, "", 401, "/login?next=%2Fdevices%2F7%2Fpower%2F1%3Fv%3D2"},
		{"htmx, session expired", "", sessionFn{err: auth.ErrSessionExpired}, true, "", 401, "/login?next=%2Fdevices%2F7%2Fpower%2F1%3Fv%3D2&expired=1"},
		{"htmx, stale pre-session cookie", "", sessionFn{token: "pre-cookie"}, true, "", 401, "/login?next=%2Fdevices%2F7%2Fpower%2F1%3Fv%3D2"},
		{"htmx, valid session, wrong token", "", sessionFn{token: "tok", valid: true}, true, "csrf=wrong", 403, ""},
		{"htmx, valid session, no token", "", sessionFn{token: "tok", valid: true}, true, "a=b", 403, ""},
		{"htmx, reserved header subject", "", sessionFn{err: auth.ErrReservedSubject}, true, "", 403, ""},
		{"htmx, resolve error other than unauthenticated", "", sessionFn{err: errors.New("database down")}, true, "", 403, ""},
		{"not htmx, no session", "", sessionFn{}, false, "", 403, ""},
		{"not htmx, session expired", "", sessionFn{err: auth.ErrSessionExpired}, false, "", 403, ""},
		{"htmx GET, no session", "GET /devices", sessionFn{}, true, "", 204, ""},
		{"htmx sign-out, no session", "POST /logout", sessionFn{}, true, "csrf=old", 204, ""},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) })
		csrf, err := httpx.NewCSRF(c.s)
		if err != nil {
			t.Fatal(err)
		}
		method, path, _ := strings.Cut(c.target, " ")
		if c.target == "" {
			method, path = http.MethodPost, "/devices/7/power/1?v=2"
		}
		var body io.Reader = untouchedBody{t}
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.htmx {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		guardUnsafe(c.s, http.NewCrossOriginProtection(), csrf.Middleware, next).ServeHTTP(rec, req)
		if rec.Code != c.want || reached != (c.want == 204) || rec.Header().Get("HX-Redirect") != c.redirect {
			t.Errorf("%s: %d HX-Redirect=%q reached=%v, want %d HX-Redirect=%q", c.name, rec.Code, rec.Header().Get("HX-Redirect"), reached, c.want, c.redirect)
		}
		if c.want == 403 && strings.TrimSpace(rec.Body.String()) != "missing or invalid CSRF token" {
			t.Errorf("%s: body %q, want the CSRF refusal", c.name, rec.Body.String())
		}
	}
}

// chromeAccept is what Chrome sends with a form post that navigates (a plain form, not htmx).
const chromeAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"

// withBase runs r through the proxy middleware with a static base path, so the request carries the
// base path in its context the way the app's handlers see it (prefix stripped from the path).
func withBase(t *testing.T, base string, h http.Handler) http.Handler {
	t.Helper()
	return httpx.Proxy{BasePath: base}.Middleware(h)
}

// Why (#1084): only a browser that loads the answer as a page may get the styled page or the
// redirect; htmx swaps the answer into the page and an API client reads the text. "*/*" alone is
// what htmx, curl and fetch send, so it must not count as asking for a page, and neither may a
// text/html the client refuses (q=0).
func TestAcceptsPage(t *testing.T) {
	cases := []struct {
		accept string
		htmx   bool
		want   bool
	}{
		{chromeAccept, false, true},
		{"text/html", false, true},
		{"TEXT/HTML; charset=utf-8", false, true},
		{"application/json, text/html;q=0.5", false, true},
		{chromeAccept, true, false},
		{"", false, false},
		{"*/*", false, false},
		{"text/*", false, false},
		{"application/json", false, false},
		{"text/html;q=0", false, false},
		{"text/html;q=0.000", false, false},
		{"text/html;q=x", false, false},
		{"text/plain, text/htmlx", false, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/devices/bulk/command", nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if c.htmx {
			r.Header.Set("HX-Request", "true")
		}
		if got := acceptsPage(r); got != c.want {
			t.Errorf("Accept %q htmx=%v: %v, want %v", c.accept, c.htmx, got, c.want)
		}
	}
}

// Why (#1084): Reload on the stale page leads back to the page the form was on. The Referer is
// request input, so it may only become a local path of this app: a Referer of another site, a
// malformed one, one outside the base path or one that a browser would read as another host
// ("//host") falls back to the base path; a backslash leaves only escaped (%5C), so the path stays
// on this host. PUBLIC_URL's origin counts as this app (a
// proxy may rewrite Host), as it does for the origin check.
func TestReloadTarget(t *testing.T) {
	cases := []struct {
		base, referer, want string
	}{
		{"", "http://example.com/devices?q=a", "/devices?q=a"},
		{"", "http://EXAMPLE.com/devices", "/devices"},
		{"", "http://example.com/devices#row-7", "/devices"},
		{"", "https://fleet.example/devices", "/devices"},
		{"", "", "/"},
		{"", "http://evil.example/devices", "/"},
		{"", "https://fleet.example.evil/devices", "/"},
		{"", "http://example.com.evil/devices", "/"},
		{"", "%zz", "/"},
		{"", "http://example.com/%zz", "/"},
		{"", "//example.com/devices", "/"},
		{"", "/devices", "/"},
		{"", "http://example.com//evil.example/x", "/"},
		{"", `http://example.com/\evil.example`, "/%5Cevil.example"}, // escaped: a path on this host, never "/\evil.example"
		{"", "javascript:alert(1)", "/"},
		{"", "ftp://example.com/devices", "/"},
		{"", "http://user@example.com/devices", "/"},
		{"/tf", "http://example.com/tf/devices", "/tf/devices"},
		{"/tf", "http://example.com/tf", "/tf"},
		{"/tf", "http://example.com/devices", "/tf/"},
		{"/tf", "http://example.com/tfx/devices", "/tf/"},
		{"/tf", "http://evil.example/tf/devices", "/tf/"},
	}
	for _, c := range cases {
		var got string
		h := withBase(t, c.base, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = reloadTarget(r, []string{"https://fleet.example"})
		}))
		r := httptest.NewRequest(http.MethodPost, c.base+"/devices/bulk/command", nil) // Host: example.com
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("base %q Referer %q: %q, want %q", c.base, c.referer, got, c.want)
		}
	}
}

// Why (#1084): a form left open past a sign-in change (sign-out and in again, another tab, a
// restart) carries a token that no longer matches. A browser got a white page with "missing or
// invalid CSRF token" and lost what it had selected; it must get the styled page that says what
// happened and leads on (Reload back to the page the form was on, Dashboard). The check itself does
// not soften: the answer stays 403 and the handler is never reached. htmx and API callers keep the
// plain text (htmx swaps it into the page, a script reads it), and a cross-origin post keeps its own
// plain refusal, so a forged form never gets the friendly page. The guard is built by
// newUnsafeGuard, as the app builds it, so the page is wired where the app wires it.
func TestGuardUnsafeStaleFormPage(t *testing.T) {
	const plain, crossOrigin = "missing or invalid CSRF token", "cross-origin request rejected"
	cases := []struct {
		name   string
		base   string
		s      sessionFn
		header map[string]string
		body   string
		want   int
		page   bool   // the stale page, else plain text
		text   string // the plain text wanted
		reload string // Reload's href on the page
	}{
		{"wrong token", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "Referer": "http://example.com/devices?q=a"}, "csrf=old", 403, true, "", "/devices?q=a"},
		{"no token", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "Referer": "http://example.com/devices"}, "id=1", 403, true, "", "/devices"},
		{"under a base path", "/tf", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "Referer": "http://example.com/tf/devices"}, "csrf=old", 403, true, "", "/tf/devices"},
		{"Referer of another site", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "Referer": "http://evil.example/devices"}, "csrf=old", 403, true, "", "/"},
		{"pre-session cookie, wrong token", "", sessionFn{token: "pre"}, map[string]string{"Accept": chromeAccept}, "csrf=old", 403, true, "", "/"},
		{"htmx", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "HX-Request": "true"}, "csrf=old", 403, false, plain, ""},
		{"API caller", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": "application/json"}, "csrf=old", 403, false, plain, ""},
		{"no Accept", "", sessionFn{token: "tok", valid: true}, nil, "csrf=old", 403, false, plain, ""},
		{"another site", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept, "Sec-Fetch-Site": "cross-site"}, "csrf=old", 403, false, crossOrigin, ""},
		{"right token", "", sessionFn{token: "tok", valid: true}, map[string]string{"Accept": chromeAccept}, "csrf=tok", 204, false, "", ""},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) })
		guard, err := newUnsafeGuard(c.s, web.NewPages(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, c.base+"/devices/bulk/command", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		withBase(t, c.base, guard(next)).ServeHTTP(rec, req)
		body, ctype := rec.Body.String(), rec.Header().Get("Content-Type")
		if rec.Code != c.want || reached != (c.want == 204) {
			t.Errorf("%s: %d reached=%v, want %d", c.name, rec.Code, reached, c.want)
			continue
		}
		switch {
		case c.page:
			dashboard := c.base + "/"
			for _, want := range []string{
				"<h1>This page is out of date</h1>",
				"Your sign-in changed since you opened it. Reload the page and try again.",
				`<a class="btn btn-primary" href="` + c.reload + `">Reload</a>`,
				`<a class="btn" href="` + dashboard + `">Dashboard</a>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: missing %q in %s", c.name, want, body)
				}
			}
			if !strings.HasPrefix(ctype, "text/html") || strings.Contains(body, plain) {
				t.Errorf("%s: %q, want the styled page and not the plain text", c.name, ctype)
			}
		case c.text != "":
			if strings.TrimSpace(body) != c.text || !strings.HasPrefix(ctype, "text/plain") {
				t.Errorf("%s: %q %q, want the plain %q", c.name, ctype, body, c.text)
			}
		}
	}
}

// Why (#1084): without any session no token can exist, so a form posted from a tab whose sign-in
// ended (expired, signed out elsewhere, a restart that dropped the session) can never pass. A
// browser is sent to the sign-in page that says the session expired, instead of a white 403 page;
// nothing is read from the body. Everything else keeps today's answer: htmx gets #717's 401, the
// plain sign-out #508's exemption, an API caller and a failed session lookup the plain 403, and a
// request with a pre-session cookie (the login form itself) still goes through the token check, so
// a correct sign-in is never redirected. A post from another site never gets the redirect: the
// origin check comes first (a page there cannot sign you out, e2e auth-local.spec.ts).
func TestGuardUnsafeSendsAPageWithoutSessionToSignIn(t *testing.T) {
	const plain = "missing or invalid CSRF token"
	cases := []struct {
		name     string
		base     string
		target   string // "METHOD path", default POST /devices/bulk/command
		s        sessionFn
		header   map[string]string
		body     string // "" sends a body that fails the test when read
		want     int
		location string
	}{
		{"no session", "", "", sessionFn{}, map[string]string{"Accept": chromeAccept}, "", 303, "/login?expired=1"},
		{"no session, same origin", "", "", sessionFn{}, map[string]string{"Accept": chromeAccept, "Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}, "", 303, "/login?expired=1"},
		{"session expired", "", "", sessionFn{err: auth.ErrSessionExpired}, map[string]string{"Accept": chromeAccept}, "", 303, "/login?expired=1"},
		{"under a base path", "/tf", "", sessionFn{}, map[string]string{"Accept": chromeAccept}, "", 303, "/tf/login?expired=1"},
		{"another site", "", "", sessionFn{}, map[string]string{"Accept": chromeAccept, "Sec-Fetch-Site": "cross-site"}, "", 403, ""},
		{"another site signs out", "", "POST /logout", sessionFn{}, map[string]string{"Accept": chromeAccept, "Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}, "", 403, ""},
		{"another origin without Sec-Fetch-Site", "", "", sessionFn{}, map[string]string{"Accept": chromeAccept, "Origin": "http://evil.example"}, "", 403, ""},
		{"API caller", "", "", sessionFn{}, map[string]string{"Accept": "application/json"}, "", 403, ""},
		{"no Accept", "", "", sessionFn{}, nil, "", 403, ""},
		{"Accept */*", "", "", sessionFn{}, map[string]string{"Accept": "*/*"}, "", 403, ""},
		{"session lookup failed", "", "", sessionFn{err: errors.New("database down")}, map[string]string{"Accept": chromeAccept}, "", 403, ""},
		{"reserved header subject", "", "", sessionFn{err: auth.ErrReservedSubject}, map[string]string{"Accept": chromeAccept}, "", 403, ""},
		{"htmx", "", "", sessionFn{}, map[string]string{"Accept": chromeAccept, "HX-Request": "true"}, "", 401, ""},
		{"sign-in with the pre-session cookie", "", "POST /login/local", sessionFn{token: "pre"}, map[string]string{"Accept": chromeAccept}, "csrf=pre", 204, ""},
		{"plain sign-out", "", "POST /logout", sessionFn{}, map[string]string{"Accept": chromeAccept}, "csrf=old", 204, ""},
		{"GET", "", "GET /devices", sessionFn{}, map[string]string{"Accept": chromeAccept}, "", 204, ""},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusNoContent) })
		guard, err := newUnsafeGuard(c.s, web.NewPages(), nil)
		if err != nil {
			t.Fatal(err)
		}
		method, path, _ := strings.Cut(c.target, " ")
		if c.target == "" {
			method, path = http.MethodPost, "/devices/bulk/command"
		}
		var body io.Reader = untouchedBody{t}
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req := httptest.NewRequest(method, c.base+path, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		withBase(t, c.base, guard(next)).ServeHTTP(rec, req)
		if rec.Code != c.want || reached != (c.want == 204) || rec.Header().Get("Location") != c.location {
			t.Errorf("%s: %d Location=%q reached=%v, want %d Location=%q", c.name, rec.Code, rec.Header().Get("Location"), reached, c.want, c.location)
		}
		if c.want == 403 && strings.TrimSpace(rec.Body.String()) != plain {
			t.Errorf("%s: body %q, want the plain CSRF refusal", c.name, rec.Body.String())
		}
	}
}
