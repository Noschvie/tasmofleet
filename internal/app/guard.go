package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/importer"
	"github.com/nofuturekid/tasmofleet/internal/web"
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
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
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
