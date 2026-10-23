// Package httpx contains the HTTP plumbing shared by every handler:
//
//   - external URL handling: PUBLIC_URL is the source of truth for absolute
//     URLs (OIDC redirect_uri, post-logout redirect, firmware base is separate).
//     X-Forwarded-Proto/-Host/-Prefix and X-Ingress-Path are honoured only
//     when RemoteAddr is inside TrustedProxies; otherwise they are ignored.
//   - base path: requests may arrive under a prefix (BASE_PATH, X-Forwarded-Prefix,
//     or Home Assistant ingress X-Ingress-Path); BasePath(ctx) returns it and
//     templates build every link from it.
//   - security headers (CSP without inline script, frame-ancestors 'self',
//     nosniff, referrer policy), request IDs, structured access log.
//   - CSRF: net/http.CrossOriginProtection (stdlib, Sec-Fetch-Site/Origin)
//     plus a per-session token checked on every non-GET (form field or
//     X-CSRF-Token header sent by HTMX).
package httpx

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Proxy describes the trusted reverse proxies.
type Proxy struct {
	TrustedProxies []netip.Prefix
	PublicURL      string // e.g. https://tasmofleet.example.com
	BasePath       string // static prefix, "" or "/tasmofleet"
}

// Trusted reports whether the TCP peer of r is a trusted proxy.
// Only RemoteAddr counts; X-Forwarded-For is never consulted.
func (p Proxy) Trusted(r *http.Request) bool {
	a, ok := peer(r)
	return ok && p.trustedAddr(a)
}

func (p Proxy) trustedAddr(a netip.Addr) bool {
	for _, pfx := range p.TrustedProxies {
		if pfx.Contains(a) {
			return true
		}
	}
	return false
}

func peer(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// URL returns the absolute external URL PUBLIC_URL + BASE_PATH + path. It is
// never derived from request headers.
func (p Proxy) URL(path string) string {
	return strings.TrimSuffix(p.PublicURL, "/") + cleanBase(p.BasePath) + path
}

// Middleware resolves base path, scheme and client address and stores them
// in the request context. With a static BASE_PATH a matching prefix is
// stripped from the request path, so handlers see paths relative to the base.
func (p Proxy) Middleware(next http.Handler) http.Handler {
	static := cleanBase(p.BasePath)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := reqInfo{base: static, scheme: "http"}
		if r.TLS != nil {
			info.scheme = "https"
		}
		peerAddr, _ := peer(r)
		info.client = peerAddr
		trusted := p.trustedAddr(peerAddr)
		if trusted {
			for _, h := range []string{"X-Ingress-Path", "X-Forwarded-Prefix"} {
				if v := r.Header.Get(h); v != "" {
					if b, ok := validBase(v); ok {
						info.base = b
						break
					}
				}
			}
			switch strings.ToLower(r.Header.Get("X-Forwarded-Proto")) {
			case "https":
				info.scheme = "https"
			case "http":
				info.scheme = "http"
			}
			info.client = p.clientFromXFF(r, peerAddr)
		}
		if static != "" && info.base == static {
			r = stripPrefix(r, static)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), baseKey{}, info)))
	})
}

// clientFromXFF walks X-Forwarded-For from the right and returns the first
// hop that is not a trusted proxy, so a client cannot choose its address.
func (p Proxy) clientFromXFF(r *http.Request, peerAddr netip.Addr) netip.Addr {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peerAddr
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return client
		}
		client = a.Unmap()
		if !p.trustedAddr(client) {
			return client
		}
	}
	return client
}

func stripPrefix(r *http.Request, prefix string) *http.Request {
	path := r.URL.Path
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return r
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(path, prefix)
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	r2.URL.RawPath = ""
	return r2
}

var baseRe = regexp.MustCompile(`^(/[A-Za-z0-9._~\-]+)+$`)

// validBase normalises a base path from config or a trusted header.
func validBase(s string) (string, bool) {
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", true
	}
	if !baseRe.MatchString(s) {
		return "", false
	}
	for seg := range strings.SplitSeq(s[1:], "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	return s, true
}

func cleanBase(s string) string {
	b, _ := validBase(s) // config validation rejects invalid values; here they degrade to ""
	return b
}

type reqInfo struct {
	base   string
	scheme string
	client netip.Addr
}

type baseKey struct{}

func info(ctx context.Context) reqInfo {
	i, _ := ctx.Value(baseKey{}).(reqInfo)
	return i
}

// Detach returns a fresh context that carries only the request info of ctx
// (base path, scheme, client), nothing else: no cancellation and no values other
// middleware cached. Long-lived streams re-check their session with it, so the
// cookie lookup sees the request the way the browser made it.
func Detach(ctx context.Context) context.Context {
	return context.WithValue(context.Background(), baseKey{}, info(ctx))
}

// BasePath returns the effective base path of the request ("" or "/x").
func BasePath(ctx context.Context) string { return info(ctx).base }

// Scheme returns the scheme the client used ("http" or "https"), from
// X-Forwarded-Proto only when the peer is trusted. For logging only.
func Scheme(ctx context.Context) string { return info(ctx).scheme }

// ClientIP returns the client address: the peer, or the rightmost untrusted
// X-Forwarded-For hop when the peer is a trusted proxy. For logging and rate
// limiting only, never for trust decisions.
func ClientIP(ctx context.Context) netip.Addr { return info(ctx).client }

// LocalPath returns s if it is a safe local redirect target (starts with a
// single "/", no scheme, host, backslash or control character), else "/".
func LocalPath(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsAny(s, `\`) {
		return "/"
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return s
}

// SecurityHeaders returns a middleware that sets CSP, nosniff and the
// referrer policy. frameAncestors (FRAME_ANCESTORS) and formActions (the
// OIDC issuer origin, needed for the logout redirect after a form POST) are
// added to the respective CSP directives.
func SecurityHeaders(frameAncestors, formActions []string) (func(http.Handler) http.Handler, error) {
	for _, s := range append(append([]string{}, frameAncestors...), formActions...) {
		if s == "" || strings.ContainsAny(s, "; ,'\"\r\n\t") {
			return nil, fmt.Errorf("httpx: invalid CSP source %q", s)
		}
	}
	csp := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"frame-ancestors " + strings.Join(append([]string{"'self'"}, frameAncestors...), " ") + "; " +
		"form-action " + strings.Join(append([]string{"'self'"}, formActions...), " ")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			next.ServeHTTP(w, r)
		})
	}, nil
}

// TokenSource returns the CSRF token expected for r ("" if none), e.g. the
// session's synchronizer token (implemented by package auth).
type TokenSource interface {
	CSRFToken(r *http.Request) string
}

// CSRF protects unsafe methods with CrossOriginProtection plus a token.
type CSRF struct {
	cop    *http.CrossOriginProtection
	tokens TokenSource
}

// CSRFField and CSRFHeader carry the token in forms and HTMX requests.
const (
	CSRFField  = "csrf"
	CSRFHeader = "X-CSRF-Token"
)

// NewCSRF builds the middleware. trustedOrigins (PUBLIC_URL's origin) are
// accepted by the origin check even when the Host header differs.
func NewCSRF(tokens TokenSource, trustedOrigins ...string) (*CSRF, error) {
	cop := http.NewCrossOriginProtection()
	for _, o := range trustedOrigins {
		u, err := url.Parse(o)
		if err != nil {
			return nil, fmt.Errorf("httpx: trusted origin %q: %w", o, err)
		}
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			return nil, fmt.Errorf("httpx: trusted origin %q: %w", o, err)
		}
	}
	return &CSRF{cop: cop, tokens: tokens}, nil
}

// ErrCSRF is reported when a request fails the CSRF check.
var ErrCSRF = errors.New("httpx: CSRF check failed")

// Middleware rejects unsafe requests that are cross-origin or carry no
// valid token with 403.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if err := c.cop.Check(r); err != nil {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		want := c.tokens.CSRFToken(r)
		got := r.Header.Get(CSRFHeader)
		if got == "" {
			got = r.PostFormValue(CSRFField)
		}
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AccessLog logs one line per request with a request ID (also returned in
// X-Request-ID). It never logs query strings, which may carry OIDC codes.
func AccessLog(l *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()[:12]
		w.Header().Set("X-Request-ID", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		l.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("id", id), slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", rec.status), slog.Duration("duration", time.Since(start)),
			slog.String("client", ClientIP(r.Context()).String()), slog.String("scheme", Scheme(r.Context())))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush (SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
