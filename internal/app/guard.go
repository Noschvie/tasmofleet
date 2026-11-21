package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/importer"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web"
)

// maxFormBody caps request bodies of the UI: it posts a few short fields.
const maxFormBody = 1 << 20

// The TasmoAdmin import accepts three files of at most importer.MaxFileBytes.
const (
	importUploadPath = "/import/tasmoadmin"
	maxImportBody    = 3*importer.MaxFileBytes + 64<<10
)

// uploadLimits maps the multipart upload routes to their body caps: the
// TasmoAdmin import (memory only) and the custom firmware upload (streamed to
// a temp file by the firmware package).
var uploadLimits = map[string]int64{
	importUploadPath:     maxImportBody,
	web.CustomUploadPath: firmware.MaxCustomUpload,
}

// guardBodies runs before the CSRF middleware, which parses form bodies. On
// every unsafe request it:
//   - answers 403 at once when no CSRF token can exist for the caller
//     (anonymous clients never get their body read), and
//   - refuses multipart bodies except the upload routes in uploadLimits (a file
//     part would spill to disk), and
//   - caps the body at maxFormBody before anything parses it.
func guardBodies(tokens httpx.TokenSource, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if tokens.CSRFToken(r) == "" {
			httpx.CSRFRefused(w, r)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
			if limit, ok := uploadLimits[r.URL.Path]; ok && r.Header.Get(httpx.CSRFHeader) != "" {
				// An upload route: its handler reads the parts as a stream
				// (multipart.Reader, no ParseMultipartForm) and the CSRF token
				// must come in the header, so the CSRF middleware never parses
				// the multipart form itself.
				r.Body = http.MaxBytesReader(w, r.Body, limit)
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "multipart bodies are not accepted", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
		next.ServeHTTP(w, r)
	})
}

// logoutPath is the sign-out route of package auth (relative to the base path).
const logoutPath = "/logout"

// sessionTokens is what guardUnsafe asks of package auth: the CSRF token and
// whether the request carries a valid session.
type sessionTokens interface {
	httpx.TokenSource
	Principal(r *http.Request) (model.Principal, error)
}

// guardUnsafe chains guardBodies and the CSRF middleware around next, with one
// exception (#508): a same-origin POST /logout without a valid session goes
// straight to next. An expired session has no CSRF token, so the chain would
// answer 403 and the cookie would never be cleared. Only the token check is
// skipped, the origin check (cop, the one of the CSRF middleware) stays: a form
// that another site posts never carries the session cookie (SameSite=Lax), so
// it always looks signed out, but the cookie-clearing answer would still end a
// valid session in that browser. Only this one exact route is exempt; with a
// valid session (header principals included) sign-out keeps its CSRF check.
// The body is capped like everywhere else.
//
// Every other unsafe htmx request without a valid session gets Require's
// answer for htmx (auth.SignIn: 401 with HX-Redirect to the sign-in page)
// before guardBodies (#717): its 403 "missing or invalid CSRF token" is swapped
// into the page by htmx, and a tab left open past the session's end then shows
// that text where a row or card was. The body is never read. A valid session
// with a missing or wrong token is still refused with 403, requests that are
// not htmx keep the 403, and so does a reserved header subject (Require answers
// it with 403, not with the sign-in page).
//
// A form that a browser posts as a page (acceptsPage) without any session and
// without any token (none can exist, so guardBodies would refuse it) is sent to
// the sign-in page that says the session expired (#1084), again without reading
// the body. Like the #508 exemption it needs the origin check: a form another
// site posts never gets the redirect. A pre-session cookie (the login form)
// carries a token, so the login post still goes through the token check.
func guardUnsafe(a sessionTokens, cop *http.CrossOriginProtection, csrf func(http.Handler) http.Handler, next http.Handler) http.Handler {
	guarded := guardBodies(a, csrf(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPlainLogout(r) && cop.Check(r) == nil {
			if _, err := a.Principal(r); errors.Is(err, auth.ErrUnauthenticated) {
				r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
				next.ServeHTTP(w, r)
				return
			}
		}
		if isUnsafe(r) && r.Header.Get("HX-Request") != "" {
			if _, err := a.Principal(r); errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrReservedSubject) {
				auth.SignIn(w, r, err)
				return
			}
		}
		if isUnsafe(r) && acceptsPage(r) && a.CSRFToken(r) == "" && cop.Check(r) == nil {
			if _, err := a.Principal(r); errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrReservedSubject) {
				http.Redirect(w, r, httpx.BasePath(r.Context())+"/login?expired=1", http.StatusSeeOther)
				return
			}
		}
		guarded.ServeHTTP(w, r)
	})
}

// newUnsafeGuard builds the guard of every unsafe request as the app uses it:
// guardUnsafe around the CSRF middleware, both origin checks with the trusted
// origins (PUBLIC_URL), and the stale-form page as the answer to a refused
// token (#1084).
func newUnsafeGuard(a sessionTokens, pages *web.Pages, origins []string) (func(http.Handler) http.Handler, error) {
	csrf, err := httpx.NewCSRF(a, origins...)
	if err != nil {
		return nil, err
	}
	csrf.Refused = staleForm(pages, origins)
	// The sign-out exemption (#508) and the redirect without a session skip
	// only the token: they keep the origin check of csrf, with the same
	// trusted origins.
	cop := http.NewCrossOriginProtection()
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil {
			return nil, err
		}
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			return nil, err
		}
	}
	return func(next http.Handler) http.Handler { return guardUnsafe(a, cop, csrf.Middleware, next) }, nil
}

// staleForm answers a same-origin request whose CSRF token is missing or wrong
// (#1084): a form left open past a sign-in change. A browser loading the answer
// as a page gets the styled page with Reload and Dashboard; htmx and API
// callers keep the plain 403. Both answers are 403.
func staleForm(pages *web.Pages, origins []string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acceptsPage(r) {
			httpx.CSRFRefused(w, r)
			return
		}
		pages.StaleForm(w, r, reloadTarget(r, origins))
	}
}

// acceptsPage reports whether r is a browser loading the answer as a page: not
// htmx (it swaps the answer into the page), and Accept names text/html with a
// q above 0. "*/*" alone (htmx, fetch, curl) does not count.
func acceptsPage(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "" {
		return false
	}
	for _, v := range r.Header.Values("Accept") {
		for part := range strings.SplitSeq(v, ",") {
			mt, params, err := mime.ParseMediaType(part)
			if err != nil || mt != "text/html" {
				continue
			}
			if q, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(q, 64); err != nil || f <= 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// reloadTarget is where the stale-form page's Reload leads: the page the form
// was on, taken from the Referer, but only as a local path of this app. The
// Referer must be an http(s) address of this host or of a trusted origin
// (PUBLIC_URL), its path under the base path, and the path and query must pass
// httpx.LocalPath; otherwise it is the base path. The result is never an
// absolute address, so the page cannot lead off-site.
func reloadTarget(r *http.Request, origins []string) string {
	base := httpx.BasePath(r.Context())
	home := base + "/"
	ref, err := url.Parse(r.Referer())
	if err != nil || (ref.Scheme != "http" && ref.Scheme != "https") || ref.User != nil || ref.Host == "" {
		return home
	}
	if !strings.EqualFold(ref.Host, r.Host) && !slices.ContainsFunc(origins, func(o string) bool {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Scheme+"://"+u.Host, ref.Scheme+"://"+ref.Host)
	}) {
		return home
	}
	p := ref.EscapedPath()
	if p != base && !strings.HasPrefix(p, base+"/") {
		return home
	}
	if ref.RawQuery != "" {
		p += "?" + ref.RawQuery
	}
	if httpx.LocalPath(p) != p {
		return home
	}
	return p
}

// isUnsafe is true for the methods guardBodies checks: all but GET, HEAD and OPTIONS.
func isUnsafe(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// isPlainLogout is true only for POST to exactly /logout: no query, no encoded
// path (RawPath is set when the path was escaped), no trailing or doubled slash.
// Anything else, however the router might still match it, stays in the full chain.
func isPlainLogout(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == logoutPath && r.URL.RawPath == "" && r.URL.RawQuery == "" && !r.URL.ForceQuery
}

// metricsSwitch is the /metrics master switch (N19): while the setting is off
// the endpoint does not exist (404), before any token or peer check, so an
// unauthorised scraper learns nothing about the configuration. It is read on
// every request, so a change on the settings page applies at once.
func metricsSwitch(enabled func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// metricsGate protects /metrics and is the only place that decides. token is
// read ONCE per request (a snapshot), so the decision cannot straddle a change on
// the settings page: with a token, the request must carry exactly that bearer
// token (constant-time compare); without one, only local scrapers are served: a
// loopback TCP peer that did not come through a proxy (no forwarding headers), so
// a reverse proxy that forwards /metrics cannot publish the fleet inventory.
func metricsGate(token func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := token(); want != "" {
			scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
			if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, perr := netip.ParseAddr(host)
		local := err == nil && perr == nil && ip.Unmap().IsLoopback() &&
			r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("Forwarded") == "" && r.Header.Get("X-Real-IP") == ""
		if !local {
			http.Error(w, "metrics need a scrape token (settings page, Metrics) when scraped remotely", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
