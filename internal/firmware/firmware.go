// Package firmware fetches Tasmota firmware files from configured upstream
// sources into a local cache and serves cached files to devices over plain
// HTTP on a separate listener (ESP8266 OTA cannot do HTTPS).
//
// SSRF rule: downloads only go to the configured sources; every request and
// every redirect hop must match a source's allowed hosts, use https (except
// sources explicitly configured as http, e.g. ota.tasmota.com), and stay
// under MaxSize. Each downloaded file is checked with fwselect.InspectImage
// before it is indexed: a file whose header family does not match its file
// name is rejected and deleted.
//
// The device-facing server is read-only, serves only indexed artifacts at
// /fw/<release>/<filename> (no listing, GET/HEAD only), and shares nothing
// with the UI listener. Keeping all files of one release in one directory
// means Tasmota's own fallback (it derives tasmota-minimal.bin.gz from the
// directory of OtaUrl) also resolves.
package firmware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors.
var (
	ErrSourceNotAllowed = errors.New("firmware: URL not allowed by any configured source")
	ErrTooLarge         = errors.New("firmware: file exceeds size limit")
	ErrImageMismatch    = errors.New("firmware: image header does not match file name")
	ErrNotImplemented   = errors.New("firmware: not implemented")
	ErrNoCrossCheck     = errors.New("firmware: cross-check impossible, only one source offers the file")
)

// Source is one configured upstream.
type Source struct {
	Name         string   // "github" or "ota.tasmota.com"
	Kind         string   // "github-releases" or "ota-tasmota"
	BaseURL      string   // e.g. https://api.github.com/repos/arendst/Tasmota, http://ota.tasmota.com
	AllowedHosts []string // hosts a request or redirect may reach
	AllowHTTP    bool
}

// Release is one upstream release with the files it offers.
type Release struct {
	Version string   // "16.0.0"
	Files   []string // upstream file names

	// Notes is the release body (changelog) as plain Markdown text, cut to
	// MaxNotesBytes; empty when no source provides one. Render it only with
	// ParseNotes. URL is the release page (a SafeLink or empty).
	Notes          string
	NotesTruncated bool
	URL            string
	Published      time.Time
}

// Cache is the firmware cache used by ota (lookup) and web (management).
type Cache interface {
	// Releases lists upstream releases (cached for a few minutes).
	Releases(ctx context.Context) ([]Release, error)
	// Ensure returns the cached artifact, downloading it first if needed.
	Ensure(ctx context.Context, release, filename string) (model.Artifact, error)
	// URLFor returns the device-facing URL of a cached artifact, built from
	// the configured firmware base URL (e.g. http://172.31.12.61/fw/...).
	URLFor(a model.Artifact) string
	// Prune deletes artifacts not referenced by an active job and older than
	// the newest keep releases.
	Prune(ctx context.Context, keep int) (int, error)
}

// CachedLister is implemented by the cache returned by New: the releases of
// the index already in memory, without a request to any source (nil before
// the first successful fetch).
type CachedLister interface {
	CachedReleases() []Release
}

// Options configure the cache. Inspect is fwselect.InspectImage in
// production (injected so this package's tests do not depend on fwselect's
// progress); it receives the first bytes of the uncompressed image.
type Options struct {
	Dir        string // <DATA_DIR>/firmware
	Sources    []Source
	BaseURL    string // FIRMWARE_BASE_URL
	MaxSize    int64  // default 8 MiB
	CrossCheck bool   // download from every source and require equal SHA-256
	// AllowSingleSource lets a download proceed (with a warning) when
	// CrossCheck is on but only one source offers the file. Default: refuse.
	AllowSingleSource bool
	Inspect           func(header []byte) (model.ChipFamily, error)
	HTTPClient        *http.Client // nil = default with timeouts; tests pass an httptest client

	// Additions by the firmware package (all optional).

	// Clock is the time source for the index TTL; nil = real time.
	Clock clock.Clock
	// IndexTTL is how long an upstream index is reused; default 15 minutes.
	IndexTTL time.Duration
	// InUse reports the artifacts referenced by active jobs (wired by app
	// from the job store). Prune never deletes a release that contains one,
	// and deletes nothing if the lookup fails. nil = nothing is in use.
	InUse func(ctx context.Context) (map[model.ArtifactID]bool, error)
	// InUseBy is like InUse but names the active jobs, so a refused delete
	// can say "in use by job N". When set it is used for Delete and
	// DeleteUnused (and for Prune if InUse is nil).
	InUseBy func(ctx context.Context) (map[model.ArtifactID][]model.JobID, error)
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server returns the device-facing handler (mounted on the firmware
// listener only) with default ServerOptions. It needs a cache that can look
// up indexed files (the one returned by New can); for any other Cache it
// answers 404 to everything.
func Server(c Cache) http.Handler { return NewServer(c, ServerOptions{}) }
