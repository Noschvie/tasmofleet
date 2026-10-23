package auth

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setCookies(r resp) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range (&http.Response{Header: r.hdr}).Cookies() {
		out[c.Name] = c
	}
	return out
}

// Why: a standalone install over plain HTTP must still be able to sign in
// (a Secure cookie would be dropped by the browser), and over HTTPS the
// cookie must be Secure and host-bound, with no PUBLIC_URL configured.
func TestCookieSecurityFollowsTheRequestScheme(t *testing.T) {
	e := localEnv(t)
	login := func(hdr map[string]string, csrfName string) resp {
		b := e.browser()
		pre := b.do(http.MethodGet, "/login", nil, hdr)
		c := setCookies(pre)[csrfName]
		if c == nil {
			t.Fatalf("no %s cookie in %v", csrfName, pre.hdr["Set-Cookie"])
		}
		hdr2 := map[string]string{"Cookie": csrfName + "=" + c.Value}
		for k, v := range hdr {
			hdr2[k] = v
		}
		return b.do(http.MethodPost, "/login/local", url.Values{"csrf": {c.Value}, "username": {"admin"}, "password": {testPassword}}, hdr2)
	}

	plain := setCookies(login(nil, CookieInsecure+"-csrf"))
	if c := plain[CookieInsecure]; c == nil || c.Secure || !c.HttpOnly {
		t.Errorf("plain HTTP session cookie: %+v", c)
	}
	if c := plain[CookieInsecure+"-device"]; c == nil || c.Secure {
		t.Errorf("plain HTTP device cookie: %+v", c)
	}

	// TLS ended at a trusted proxy (the test peer 127.0.0.1 is trusted).
	secure := setCookies(login(map[string]string{"X-Forwarded-Proto": "https"}, CookieSecure+"-csrf"))
	if c := secure[CookieSecure]; c == nil || !c.Secure || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("HTTPS session cookie: %+v", c)
	}
	if c := secure[CookieSecure+"-device"]; c == nil || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("HTTPS device cookie: %+v", c)
	}
}

// An untrusted client must not be able to switch its cookies to Secure by
// header (that would only break its own login, but the scheme must follow
// the trust rules everywhere).
func TestForwardedProtoFromUntrustedPeerIsIgnored(t *testing.T) {
	e := localEnv(t)
	e.svc.o.Proxy.TrustedProxies = nil
	e.srv.Config.Handler = e.handler()
	pre := e.browser().do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Proto": "https"})
	if _, ok := setCookies(pre)[CookieInsecure+"-csrf"]; !ok {
		t.Fatalf("cookies %v", pre.hdr["Set-Cookie"])
	}
}

// Why: standalone mode has no IdP and no PUBLIC_URL; nothing may require
// them, and /login must lead to the local form instead of an OIDC redirect.
func TestLocalModeNeedsNoPublicURLOrIdP(t *testing.T) {
	e := localEnv(t)
	if !e.svc.Ready() {
		t.Fatal("local mode is ready without discovery")
	}
	b := e.browser()
	r := b.get("/login?next=%2Fviewer")
	if r.code != http.StatusOK || !strings.Contains(r.body, `action="/login/local"`) || strings.Contains(r.body, "/login/sso") {
		t.Fatalf("/login: %d %q", r.code, r.body)
	}
	if r := b.get("/oidc/callback"); r.code < 400 {
		t.Fatalf("callback in local mode: %d %q", r.code, r.loc)
	}
	// Host / forwarded-host never end up in a redirect target.
	r = b.do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Host": "evil.example", "Host": "evil.example"})
	if strings.Contains(r.loc, "evil") || strings.HasPrefix(r.loc, "http") {
		t.Fatalf("redirect derived from the request: %q", r.loc)
	}
}

func TestSessionLifetimeDefaults(t *testing.T) {
	e := localEnv(t)
	b := e.login("admin", testPassword)
	// Idle 2 h.
	e.clock.Advance(2*time.Hour - time.Minute)
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("1h59 idle: %d", r.code)
	}
	e.clock.Advance(2*time.Hour + time.Second)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("session survived 2 h of inactivity")
	}
	// Absolute 12 h, even when active.
	b = e.login("admin", testPassword)
	for range 12 {
		e.clock.Advance(59 * time.Minute)
		if r := b.get("/viewer"); r.code != http.StatusOK {
			break
		}
	}
	e.clock.Advance(time.Hour)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("an active session outlived 12 h")
	}
}

// Why: a trusted browser keeps one device cookie; without reuse, every
// login adds a row, and without per-cookie rows one stolen cookie could not
// be revoked without signing out the user's other devices.
func TestDeviceCookiesAreRecordedReusedAndRevocableIndividually(t *testing.T) {
	e := localEnv(t)
	b := e.browser()
	postLocal(b, "192.0.2.20", "admin", testPassword)
	postLocal(b, "192.0.2.20", "admin", testPassword)
	if n := e.cookies.count(); n != 1 {
		t.Fatalf("two logins from one browser recorded %d cookies, want 1", n)
	}
	laptop := e.browser()
	postLocal(laptop, "192.0.2.21", "admin", testPassword)
	if n := e.cookies.count(); n != 2 {
		t.Fatalf("recorded %d cookies, want 2", n)
	}
	phone := b.cookie(CookieInsecure + "-device")
	lap := laptop.cookie(CookieInsecure + "-device")
	req := func(v string) *http.Request {
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: CookieInsecure + "-device", Value: v})
		return r
	}
	if !e.svc.validDeviceCookie(req(phone), "admin") || !e.svc.validDeviceCookie(req(lap), "admin") {
		t.Fatal("fresh cookies must be valid")
	}
	if err := e.svc.RevokeDeviceCookie(t.Context(), phone); err != nil {
		t.Fatal(err)
	}
	if e.svc.validDeviceCookie(req(phone), "admin") {
		t.Error("the revoked cookie is still recognised")
	}
	if !e.svc.validDeviceCookie(req(lap), "admin") {
		t.Error("revoking one cookie revoked the other device too")
	}
	// Garbage and forged values are ignored, not errors, and revoke nothing.
	for _, v := range []string{"", "x", "v1.a.b.c.d", phone + "x"} {
		if err := e.svc.RevokeDeviceCookie(t.Context(), v); err != nil {
			t.Errorf("RevokeDeviceCookie(%q): %v", v, err)
		}
	}
	if e.cookies.count() != 1 {
		t.Errorf("rows = %d, want 1", e.cookies.count())
	}
	// A validly signed cookie without a row (e.g. lost with the database) is not recognised.
	payload := devicePayload("admin", e.clock.Now(), "never-recorded")
	if e.svc.validDeviceCookie(req(payload+"."+e.svc.deviceMAC(payload)), "admin") {
		t.Error("a signed cookie without a store row was accepted")
	}
}

// Why: keys that change at a restart end every device cookie and header-mode
// CSRF token; keys that are shared between purposes let one signature stand
// in for another.
func TestKeyFileIsPersistentPrivateAndPurposeBound(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %v, %d bytes", err, len(k1))
	}
	fi, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", fi, err)
	}
	k2, err := LoadOrCreateKey(dir)
	if err != nil || string(k1) != string(k2) {
		t.Fatalf("the key changed between starts: %v", err)
	}
	a, b := subKey(k1, labelCSRF), subKey(k1, labelDevice)
	c := subKey(k1, labelSession)
	if string(a) == string(b) || string(a) == string(c) || string(b) == string(c) {
		t.Fatal("purpose keys are not distinct")
	}
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o644); err != nil { //nolint:gosec // test: provoke the mode check
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir); err == nil {
		t.Error("a world-readable key file was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir); err == nil {
		t.Error("a truncated key file was accepted (it would silently weaken the signatures)")
	}
}

func TestPersistedKeySurvivesRestartForCSRFAndDeviceCookies(t *testing.T) {
	key, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func() *env { return newEnv(t, ModeHeader, func(o *Options) { o.SecretKey = key }) }
	e1, e2 := mk(), mk()
	if e1.svc.headerToken("boss") != e2.svc.headerToken("boss") {
		t.Error("header CSRF tokens changed with a restart")
	}
	p := devicePayload("admin", e1.clock.Now(), "id")
	if e1.svc.deviceMAC(p) != e2.svc.deviceMAC(p) {
		t.Error("device cookie MACs changed with a restart")
	}
}

// Why: the OIDC flow cookie holds the PKCE verifier and nonce; one that this
// server did not sign must not be read.
func TestFlowCookieMustBeSigned(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	cb := b.startOIDC("/", alice)
	body, _, _ := cutLast(b.cookie(e.flowName()), ".")
	b.setCookie(e.flowName(), body) // signature stripped
	if r := b.get(cb); r.code != http.StatusBadRequest {
		t.Fatalf("unsigned flow cookie: %d", r.code)
	}
}

// Why: break-glass access goes to the container's own address over plain
// HTTP (http://<container>:8080) when the reverse proxy or the IdP is down.
// PUBLIC_URL names the public HTTPS site, but it must not force Secure
// cookies on such a request: the browser drops them and the login fails
// with "invalid CSRF token". Via the trusted proxy (X-Forwarded-Proto https)
// the cookies stay Secure and __Host- prefixed; the same header from an
// untrusted peer changes nothing.
func TestPlainHTTPLoginWorksWithHTTPSPublicURL(t *testing.T) {
	e := newEnv(t, ModeOIDC, func(o *Options) { o.Proxy.PublicURL = "https://fleet.example" })

	// The cookie jar behaves like a browser: Secure cookies are never sent over http.
	b := e.browser()
	if r := localLogin(b, "admin", testPassword, "/viewer"); r.code != http.StatusSeeOther {
		t.Fatalf("plain HTTP login: %d %s", r.code, r.body)
	}
	if b.cookie(CookieInsecure) == "" {
		t.Fatal("no plain-HTTP session cookie after login")
	}
	if r := b.post("/admin-action", nil); r.code != http.StatusOK || r.body != "ok local:admin" {
		t.Fatalf("CSRF-protected POST over plain HTTP: %d %q", r.code, r.body)
	}

	viaProxy := setCookies(e.browser().do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Proto": "https"}))
	if c := viaProxy[CookieSecure+"-csrf"]; c == nil || !c.Secure || c.Path != "/" {
		t.Errorf("HTTPS via trusted proxy: %v", viaProxy)
	}

	e.svc.o.Proxy.TrustedProxies = nil
	e.srv.Config.Handler = e.handler()
	untrusted := setCookies(e.browser().do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Proto": "https"}))
	if c := untrusted[CookieInsecure+"-csrf"]; c == nil || c.Secure {
		t.Errorf("X-Forwarded-Proto from an untrusted peer must be ignored: %v", untrusted)
	}
}
