package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

var trusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("172.30.32.2/32")}

type seen struct {
	base, path, scheme string
	client             netip.Addr
}

func run(t *testing.T, p Proxy, r *http.Request) (seen, int) {
	t.Helper()
	var s seen
	h := p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s = seen{BasePath(r.Context()), r.URL.Path, Scheme(r.Context()), ClientIP(r.Context())}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return s, w.Code
}

func req(peer string, hdr map[string]string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://app.local/devices", nil)
	r.RemoteAddr = peer
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// Forwarded headers change links and the logged client; from an untrusted
// peer they must be ignored, otherwise anyone on the network can spoof them
// (CLAUDE.md safety rule 6).
func TestForwardedHeadersOnlyFromTrustedPeers(t *testing.T) {
	p := Proxy{TrustedProxies: trusted, PublicURL: "https://fleet.example", BasePath: ""}
	hdr := map[string]string{
		"X-Forwarded-Proto":  "https",
		"X-Forwarded-Prefix": "/fleet",
		"X-Ingress-Path":     "/api/hassio_ingress/tok",
		"X-Forwarded-For":    "203.0.113.9",
	}
	cases := []struct {
		name, peer string
		want       seen
	}{
		{"untrusted peer", "192.168.1.50:5555", seen{"", "/devices", "http", netip.MustParseAddr("192.168.1.50")}},
		{"untrusted peer spoofing a trusted XFF", "192.168.1.50:5555", seen{"", "/devices", "http", netip.MustParseAddr("192.168.1.50")}},
		{"trusted proxy", "10.0.0.7:5555", seen{"/api/hassio_ingress/tok", "/devices", "https", netip.MustParseAddr("203.0.113.9")}},
		{"trusted ipv4-mapped ipv6 peer", "[::ffff:10.0.0.7]:5555", seen{"/api/hassio_ingress/tok", "/devices", "https", netip.MustParseAddr("203.0.113.9")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := hdr
			if strings.Contains(c.name, "spoofing") {
				h = map[string]string{"X-Forwarded-For": "10.0.0.7", "X-Ingress-Path": "/evil"}
			}
			got, _ := run(t, p, req(c.peer, h))
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestBasePathSources(t *testing.T) {
	p := Proxy{TrustedProxies: trusted, BasePath: "/tasmofleet"}
	cases := []struct {
		name, peer, url string
		hdr             map[string]string
		wantBase, path  string
	}{
		{"static base is stripped", "1.2.3.4:1", "/tasmofleet/devices", nil, "/tasmofleet", "/devices"},
		{"static base without prefix (proxy stripped it)", "1.2.3.4:1", "/devices", nil, "/tasmofleet", "/devices"},
		{"prefix must match a whole segment", "1.2.3.4:1", "/tasmofleetx/devices", nil, "/tasmofleet", "/tasmofleetx/devices"},
		{"base root", "1.2.3.4:1", "/tasmofleet", nil, "/tasmofleet", "/"},
		{"X-Forwarded-Prefix from trusted proxy", "10.0.0.1:1", "/devices", map[string]string{"X-Forwarded-Prefix": "/fleet/"}, "/fleet", "/devices"},
		{"ingress wins over forwarded prefix", "172.30.32.2:1", "/devices", map[string]string{"X-Forwarded-Prefix": "/fleet", "X-Ingress-Path": "/api/hassio_ingress/abc"}, "/api/hassio_ingress/abc", "/devices"},
		{"ingress from untrusted peer ignored", "172.30.32.3:1", "/devices", map[string]string{"X-Ingress-Path": "/api/hassio_ingress/abc"}, "/tasmofleet", "/devices"},
		{"malformed prefix falls back to static", "10.0.0.1:1", "/devices", map[string]string{"X-Forwarded-Prefix": "//evil.example"}, "/tasmofleet", "/devices"},
		{"prefix with dot-dot falls back", "10.0.0.1:1", "/devices", map[string]string{"X-Forwarded-Prefix": "/a/../b"}, "/tasmofleet", "/devices"},
		{"prefix with markup falls back", "10.0.0.1:1", "/devices", map[string]string{"X-Forwarded-Prefix": `/a"><script>`}, "/tasmofleet", "/devices"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://app.local"+c.url, nil)
			r.RemoteAddr = c.peer
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			got, _ := run(t, p, r)
			if got.base != c.wantBase || got.path != c.path {
				t.Fatalf("base %q path %q, want %q %q", got.base, got.path, c.wantBase, c.path)
			}
		})
	}
}

// The client IP feeds the login rate limiter: the proxy chain must be
// walked from the right so a client cannot pick its own address.
func TestClientIPFromForwardedChain(t *testing.T) {
	p := Proxy{TrustedProxies: trusted}
	got, _ := run(t, p, req("10.0.0.1:1", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.4, 10.0.0.9"}))
	if got.client != netip.MustParseAddr("198.51.100.4") {
		t.Fatalf("client %v, want the rightmost untrusted hop", got.client)
	}
}

func TestURLUsesPublicURLOnly(t *testing.T) {
	p := Proxy{PublicURL: "https://fleet.example/", BasePath: "/tf"}
	if got := p.URL("/oidc/callback"); got != "https://fleet.example/tf/oidc/callback" {
		t.Fatal(got)
	}
}

func TestLocalPath(t *testing.T) {
	cases := map[string]string{
		"":                    "/",
		"/devices?x=1":        "/devices?x=1",
		"//evil.example":      "/",
		"https://evil":        "/",
		`/\evil`:              "/",
		`/a\b`:                "/",
		"javascript:alert(1)": "/",
		"devices":             "/",
		"/ok\r\nSet-Cookie:x": "/",
		"/%2F%2Fevil":         "/%2F%2Fevil", // stays a local path; browsers do not decode it into a host
		"///evil":             "/",
		"/\tevil":             "/",
	}
	for in, want := range cases {
		if got := LocalPath(in); got != want {
			t.Errorf("LocalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	mw, err := SecurityHeaders([]string{"https://ha.example"}, []string{"https://auth.example"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mw(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self';", "frame-ancestors 'self' https://ha.example;", "form-action 'self' https://auth.example"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP must not allow inline script (XSS via device strings)")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Error("missing nosniff / referrer policy")
	}
	// A config value must not be able to inject extra CSP directives.
	if _, err := SecurityHeaders([]string{"https://a; script-src *"}, nil); err == nil {
		t.Error("CSP source with ';' accepted")
	}
}

type tokens string

func (t tokens) CSRFToken(*http.Request) string { return string(t) }

func TestCSRF(t *testing.T) {
	c, err := NewCSRF(tokens("tok123"), "https://fleet.example")
	if err != nil {
		t.Fatal(err)
	}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		body   string
		want   int
	}{
		{"GET needs no token", http.MethodGet, nil, "", http.StatusNoContent},
		{"cross-site POST rejected even with token", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "X-CSRF-Token": "tok123"}, "", http.StatusForbidden},
		{"cross-origin POST (Origin only) rejected", http.MethodPost, map[string]string{"Origin": "https://evil.example", "X-CSRF-Token": "tok123"}, "", http.StatusForbidden},
		{"same-origin POST without token rejected", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, "", http.StatusForbidden},
		{"same-origin POST wrong token rejected", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin", "X-CSRF-Token": "tok124"}, "", http.StatusForbidden},
		{"same-origin POST with header token", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin", "X-CSRF-Token": "tok123"}, "", http.StatusNoContent},
		{"same-origin POST with form token", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/x-www-form-urlencoded"}, "csrf=tok123", http.StatusNoContent},
		{"public URL origin trusted (proxy rewrote Host)", http.MethodPost, map[string]string{"Origin": "https://fleet.example", "X-CSRF-Token": "tok123"}, "", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "http://internal:8080/devices/1/delete", strings.NewReader(tc.body))
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
	// No expected token (no session) means every unsafe request fails.
	c2, _ := NewCSRF(tokens(""))
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/logout", nil)
	r.Header.Set("X-CSRF-Token", "")
	c2.Middleware(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("empty expected token accepted: %d", w.Code)
	}
}

// Why: an open event stream re-checks its session with Detach. It must keep the scheme and base
// path (the session cookie's name and path depend on them) but drop everything else: a principal
// cached at connect time would make the re-check a no-op, and the request's cancellation is not
// the re-check's business.
func TestDetachKeepsOnlyRequestInfo(t *testing.T) {
	type otherKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), otherKey{}, "cached"))
	parent = context.WithValue(parent, baseKey{}, reqInfo{base: "/tf", scheme: "https"})
	cancel()
	d := Detach(parent)
	if BasePath(d) != "/tf" || Scheme(d) != "https" {
		t.Fatalf("request info lost: base %q scheme %q", BasePath(d), Scheme(d))
	}
	if d.Value(otherKey{}) != nil {
		t.Fatal("other context values must not survive")
	}
	if d.Err() != nil {
		t.Fatal("cancellation must not survive")
	}
}

func accessLogLines(t *testing.T, level slog.Level, path string, status int) string {
	t.Helper()
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	h := AccessLog(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	return buf.String()
}

// Why: the container health check hits /healthz every 30 s; at info it drowns out the
// real requests. A failing health check is news and must stay visible.
func TestAccessLogHealthzOnlyAtDebugWhenOK(t *testing.T) {
	if out := accessLogLines(t, slog.LevelInfo, "/healthz", 200); out != "" {
		t.Fatalf("successful /healthz logged at info: %s", out)
	}
	if out := accessLogLines(t, slog.LevelDebug, "/healthz", 200); !strings.Contains(out, `"level":"DEBUG"`) {
		t.Fatalf("successful /healthz not logged at debug: %s", out)
	}
	if out := accessLogLines(t, slog.LevelInfo, "/healthz", 503); !strings.Contains(out, `"level":"INFO"`) {
		t.Fatalf("failing /healthz must stay at info: %s", out)
	}
	if out := accessLogLines(t, slog.LevelInfo, "/devices", 200); !strings.Contains(out, `"level":"INFO"`) {
		t.Fatalf("other paths must log at info: %s", out)
	}
}

// Why: slog encodes time.Duration as bare nanoseconds, which nobody reads as 12 µs.
// The unit belongs in the key, and the value is a float so sub-millisecond requests keep their size.
func TestAccessLogDurationHasUnitInKey(t *testing.T) {
	out := accessLogLines(t, slog.LevelInfo, "/devices", 200)
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if _, old := rec["duration"]; old {
		t.Fatal("unitless duration key still present")
	}
	if v, ok := rec["duration_ms"].(float64); !ok || v < 0 || v > 1000 {
		t.Fatalf("duration_ms = %v", rec["duration_ms"])
	}
}
