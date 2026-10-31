package firmware

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

const (
	defaultMaxSize  = 8 << 20
	defaultIndexTTL = 15 * time.Minute
	headerLen       = 24 // bytes of the uncompressed image handed to Inspect
	tmpPrefix       = ".tmp-"
)

// Refresher is implemented by the cache returned by New: an explicit index
// refresh that bypasses the TTL (web "refresh" button, background refresh).
type Refresher interface {
	Refresh(ctx context.Context) error
}

// Locator is implemented by the cache returned by New: it finds an indexed
// artifact and the path of its file without ever downloading (used by the
// device-facing server).
type Locator interface {
	Lookup(ctx context.Context, release, filename string) (model.Artifact, string, error)
}

// CustomLocator is implemented by the cache returned by New: Locator for
// uploads, found by their key (Artifact.Custom).
type CustomLocator interface {
	LookupCustom(ctx context.Context, key, release, filename string) (model.Artifact, string, error)
}

type cache struct {
	opts   Options
	store  store.ArtifactStore
	client *http.Client
	clk    clock.Clock
	log    *slog.Logger

	mu       sync.Mutex // guards the index fields and keyLocks
	idx      []srcIndex // parallel to opts.Sources; nil entry = source failed
	idxAt    time.Time
	idxValid bool
	metaMu   sync.Mutex // guards meta and snap; separate so Releases can read it while a refetch holds mu
	meta     []map[string]relMeta
	snap     []srcIndex // the last good index, for CachedReleases (never fetches)
	stMu     sync.Mutex // guards status; separate from mu so the page can read it during a slow fetch
	status   IndexStatus
	keyLocks map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// New creates the firmware cache. BaseURL must be plain http (ESP8266 OTA
// cannot do TLS) and Inspect is mandatory: there is no unchecked download.
// Leftover temp files of a crashed download are removed.
func New(o Options, st store.ArtifactStore) (Cache, error) {
	if o.Dir == "" {
		return nil, errors.New("firmware: Dir is required")
	}
	if o.Inspect == nil {
		return nil, errors.New("firmware: Inspect is required (the header check is not optional)")
	}
	bu, err := url.Parse(o.BaseURL)
	if err != nil || bu.Scheme != "http" || bu.Host == "" {
		return nil, fmt.Errorf("firmware: BaseURL %q must be a plain http:// URL (devices cannot do TLS OTA)", o.BaseURL)
	}
	if o.MaxSize <= 0 {
		o.MaxSize = defaultMaxSize
	}
	if o.IndexTTL <= 0 {
		o.IndexTTL = defaultIndexTTL
	}
	c := &cache{opts: o, store: st, keyLocks: map[string]*keyLock{}}
	c.opts.BaseURL = strings.TrimRight(o.BaseURL, "/")
	c.clk = o.Clock
	if c.clk == nil {
		c.clk = clock.Real{}
	}
	c.log = o.Logger
	if c.log == nil {
		c.log = slog.Default()
	}
	c.client = o.HTTPClient
	if c.client == nil {
		c.client = &http.Client{Timeout: 2 * time.Minute}
	}
	if err := os.MkdirAll(o.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("firmware: create cache dir: %w", err)
	}
	c.removeStaleTemps()
	return c, nil
}

func (c *cache) removeStaleTemps() {
	err := filepath.WalkDir(c.opts.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), tmpPrefix) {
			if rerr := os.Remove(p); rerr != nil { //nolint:gosec // p is a ".tmp-" file under our own cache dir; symlinks are not created there
				c.log.Warn("firmware: cannot remove stale temp file", "path", p, "err", rerr)
			}
		}
		return nil
	})
	if err != nil {
		c.log.Warn("firmware: scan for stale temp files failed", "err", err)
	}
}

func (c *cache) path(release, filename string) string {
	return filepath.Join(c.opts.Dir, release, filename)
}

// URLFor returns the URL a device uses to download the artifact.
func (c *cache) URLFor(a model.Artifact) string {
	if a.Custom != "" {
		return c.opts.BaseURL + "/c/" + a.Custom + "/fw/" + a.Release + "/" + a.Filename
	}
	return c.opts.BaseURL + "/fw/" + a.Release + "/" + a.Filename
}

// ---- index ----

// Releases lists the upstream releases, newest first, from the cached index.
func (c *cache) Releases(ctx context.Context) ([]Release, error) {
	idx, err := c.index(ctx, false)
	if err != nil {
		return nil, err
	}
	return c.releasesOf(idx), nil
}

// CachedReleases is Releases from the index already in memory: it never
// talks to a source, also when the index is older than its TTL. Nil before
// the first successful fetch. Pages that only show the feed (the release
// page, the dashboard counts) use it, so a view never triggers a download.
func (c *cache) CachedReleases() []Release {
	c.metaMu.Lock()
	idx := c.snap
	c.metaMu.Unlock()
	if idx == nil {
		return nil
	}
	return c.releasesOf(idx)
}

func (c *cache) releasesOf(idx []srcIndex) []Release {
	files := map[string]map[string]bool{}
	for _, si := range idx {
		for ver, fs := range si {
			if files[ver] == nil {
				files[ver] = map[string]bool{}
			}
			for f := range fs {
				files[ver][f] = true
			}
		}
	}
	c.metaMu.Lock()
	metas := c.meta
	c.metaMu.Unlock()
	out := make([]Release, 0, len(files))
	for ver, set := range files {
		r := Release{Version: ver}
		for _, sm := range metas { // the first source with a body wins
			if m, ok := sm[ver]; ok && (r.Notes == "" || r.URL == "") {
				if r.Notes == "" {
					r.Notes, r.NotesTruncated, r.Published = m.Notes, m.Truncated, m.Published
				}
				if r.URL == "" {
					r.URL = m.URL
				}
			}
		}
		for f := range set {
			r.Files = append(r.Files, f)
		}
		sort.Strings(r.Files)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return compareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

// Refresh re-reads every source's index regardless of the TTL.
func (c *cache) Refresh(ctx context.Context) error {
	_, err := c.index(ctx, true)
	return err
}

// index returns the per-source indexes. A source that fails is skipped with
// a warning as long as another one answered; if none answered the error is
// returned (unless an older index exists and force is off: that one is
// served). A partial result is reused for a minute only, then retried.
func (c *cache) index(ctx context.Context, force bool) ([]srcIndex, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.Now()
	if !force && c.idxValid && now.Sub(c.idxAt) < c.opts.IndexTTL {
		return c.idx, nil
	}
	idx := make([]srcIndex, len(c.opts.Sources))
	meta := make([]map[string]relMeta, len(c.opts.Sources))
	var errs []error
	ok := 0
	for i, s := range c.opts.Sources {
		si, sm, err := c.fetchIndex(ctx, s)
		if err != nil {
			c.log.Warn("firmware: index fetch failed", "source", s.Name, "err", err)
			errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
			continue
		}
		idx[i], meta[i] = si, sm
		ok++
	}
	if ok == 0 {
		if len(errs) == 0 {
			errs = []error{errors.New("firmware: no sources configured")}
		}
		c.recordIndex(now, false, errs)
		if !force && c.idxValid {
			// Stale beats nothing: an upstream outage must not take down
			// planning against an index we already have. The next call retries.
			c.log.Warn("firmware: serving the stale index after a failed refetch", "err", errors.Join(errs...))
			return c.idx, nil
		}
		return nil, errors.Join(errs...)
	}
	c.recordIndex(now, true, errs)
	c.idx, c.idxValid = idx, true
	c.metaMu.Lock()
	c.meta, c.snap = meta, idx
	c.metaMu.Unlock()
	c.idxAt = now
	if len(errs) > 0 {
		c.idxAt = now.Add(-c.opts.IndexTTL + time.Minute)
	}
	return idx, nil
}

// ---- ensure ----

func (c *cache) lock(key string) func() {
	c.mu.Lock()
	kl := c.keyLocks[key]
	if kl == nil {
		kl = &keyLock{}
		c.keyLocks[key] = kl
	}
	kl.refs++
	c.mu.Unlock()
	kl.mu.Lock()
	return func() {
		kl.mu.Unlock()
		c.mu.Lock()
		if kl.refs--; kl.refs == 0 {
			delete(c.keyLocks, key)
		}
		c.mu.Unlock()
	}
}

// Lookup returns an indexed artifact and its file path without downloading.
// It fails with ErrNotInIndex if the artifact is not indexed, is a safeboot
// image (never an OTA target), or its file is missing or has a different
// size than the index says (a truncated file must not reach a device).
func (c *cache) Lookup(ctx context.Context, release, filename string) (model.Artifact, string, error) {
	if !validRelease(release) {
		return model.Artifact{}, "", fmt.Errorf("%w: release %q", ErrBadName, release)
	}
	if _, err := parseName(filename); err != nil {
		return model.Artifact{}, "", err
	}
	a, err := c.store.FindArtifact(ctx, release, filename)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Artifact{}, "", ErrNotInIndex
		}
		return model.Artifact{}, "", err
	}
	if a.Safeboot {
		return model.Artifact{}, "", ErrNotInIndex
	}
	p := c.path(release, filename)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != a.Size {
		return model.Artifact{}, "", ErrNotInIndex
	}
	return a, p, nil
}

// LookupCustom is Lookup for an upload (/c/<key>/fw/<release>/<file>).
func (c *cache) LookupCustom(ctx context.Context, key, release, filename string) (model.Artifact, string, error) {
	cs, ok := c.store.(store.CustomFirmwareStore)
	if !ok || !validPathElement(key) || !validRelease(release) {
		return model.Artifact{}, "", ErrNotInIndex
	}
	if _, err := parseName(filename); err != nil {
		return model.Artifact{}, "", err
	}
	a, err := cs.FindCustomArtifact(ctx, key, release, filename)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Artifact{}, "", ErrNotInIndex
		}
		return model.Artifact{}, "", err
	}
	p := c.pathOf(a)
	fi, err := os.Stat(p)
	if a.Safeboot || err != nil || !fi.Mode().IsRegular() || fi.Size() != a.Size {
		return model.Artifact{}, "", ErrNotInIndex
	}
	return a, p, nil
}

// cached returns the artifact if both its index row and a matching file exist.
func (c *cache) cached(ctx context.Context, release, filename string) (model.Artifact, bool, error) {
	a, err := c.store.FindArtifact(ctx, release, filename)
	if errors.Is(err, store.ErrNotFound) {
		return model.Artifact{}, false, nil
	}
	if err != nil {
		return model.Artifact{}, false, err
	}
	fi, err := os.Stat(c.path(release, filename))
	if err == nil && fi.Mode().IsRegular() && fi.Size() == a.Size {
		return a, true, nil
	}
	// Row without a usable file: drop the row and fetch again.
	c.log.Warn("firmware: cached file missing or wrong size, refetching", "release", release, "file", filename)
	if err := c.store.DeleteArtifact(ctx, a.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return model.Artifact{}, false, err
	}
	return model.Artifact{}, false, nil
}

// Ensure returns the cached artifact, downloading it if necessary. The file
// is only indexed after the size limit, the cross-check and the header
// check all passed.
func (c *cache) Ensure(ctx context.Context, release, filename string) (model.Artifact, error) {
	if !validRelease(release) {
		return model.Artifact{}, fmt.Errorf("%w: release %q", ErrBadName, release)
	}
	info, err := parseName(filename)
	if err != nil {
		return model.Artifact{}, err
	}
	unlock := c.lock(release + "/" + filename)
	defer unlock()

	if a, ok, err := c.cached(ctx, release, filename); err != nil || ok {
		return a, err
	}
	idx, err := c.index(ctx, false)
	if err != nil {
		return model.Artifact{}, err
	}
	var from []int // indexes of the sources offering the file, in configured order
	for i, si := range idx {
		if si != nil && si[release][filename] != "" {
			from = append(from, i)
		}
	}
	if len(from) == 0 {
		return model.Artifact{}, fmt.Errorf("%w: %s/%s", ErrNotInIndex, release, filename)
	}
	if !c.opts.CrossCheck {
		from = from[:1]
	} else if len(from) == 1 && len(c.opts.Sources) > 1 {
		// A file only one source offers cannot be cross-checked: refuse unless
		// the operator explicitly accepted that (a tampered mirror could
		// otherwise serve a file the other source does not carry).
		if !c.opts.AllowSingleSource {
			return model.Artifact{}, fmt.Errorf("%w: %s/%s is offered by %s only", ErrNoCrossCheck, release, filename, c.opts.Sources[from[0]].Name)
		}
		c.log.Warn("firmware: cross-check not possible, only one source offers the file", "release", release, "file", filename, "source", c.opts.Sources[from[0]].Name)
	}

	relDir := filepath.Join(c.opts.Dir, release)
	if err := os.MkdirAll(relDir, 0o750); err != nil {
		return model.Artifact{}, fmt.Errorf("firmware: create release dir: %w", err)
	}
	var temps []string
	defer func() {
		for _, t := range temps {
			if err := os.Remove(t); err != nil && !errors.Is(err, fs.ErrNotExist) {
				c.log.Warn("firmware: cannot remove temp file", "path", t, "err", err)
			}
		}
	}()
	type got struct {
		tmp, sum string
		size     int64
		src      Source
		url      string
	}
	var downloads []got
	for _, i := range from {
		src, u := c.opts.Sources[i], idx[i][release][filename]
		tmp, sum, size, err := c.download(ctx, src, u, relDir)
		if tmp != "" {
			temps = append(temps, tmp)
		}
		if err != nil {
			return model.Artifact{}, fmt.Errorf("firmware: download %s from %s: %w", filename, src.Name, err)
		}
		downloads = append(downloads, got{tmp, sum, size, src, u})
	}
	primary := downloads[0]
	for _, d := range downloads[1:] {
		if d.sum != primary.sum {
			return model.Artifact{}, fmt.Errorf("%w: %s/%s: %s=%s %s=%s", ErrChecksumMismatch, release, filename,
				primary.src.Name, primary.sum, d.src.Name, d.sum)
		}
	}
	fam, err := c.inspect(primary.tmp, filename)
	if err != nil {
		return model.Artifact{}, err
	}
	if fam != info.Family {
		return model.Artifact{}, fmt.Errorf("%w: %s is named for %s but the header says %s", ErrImageMismatch, filename, info.Family, fam)
	}

	final := c.path(release, filename)
	if err := os.Rename(primary.tmp, final); err != nil {
		return model.Artifact{}, fmt.Errorf("firmware: install %s: %w", filename, err)
	}
	temps = temps[1:] // temps[0] is the primary, now renamed
	if err := syncDir(relDir); err != nil {
		c.log.Warn("firmware: fsync of release dir failed", "dir", relDir, "err", err)
	}
	a := model.Artifact{
		Release: release, Filename: filename, Family: fam, Variant: info.Variant, Language: info.Language,
		Safeboot: info.Safeboot, Size: primary.size, SHA256: primary.sum,
		Source: primary.src.Name, SourceURL: primary.url, FetchedAt: c.clk.Now(),
	}
	id, err := c.store.PutArtifact(ctx, a)
	if err != nil {
		if rerr := os.Remove(final); rerr != nil {
			c.log.Warn("firmware: cannot remove unindexed file", "path", final, "err", rerr)
		}
		return model.Artifact{}, fmt.Errorf("firmware: index %s: %w", filename, err)
	}
	a.ID = id
	c.log.Info("firmware: cached", "release", release, "file", filename, "family", fam, "size", a.Size, "source", a.Source)
	return a, nil
}

// download streams one URL into a temp file in dir, bounded by MaxSize,
// hashing as it goes, and fsyncs it. On error the temp file is removed.
func (c *cache) download(ctx context.Context, src Source, rawURL, dir string) (path, sum string, size int64, err error) {
	resp, err := c.get(ctx, src, rawURL)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = resp.Body.Close() }() // read-only body
	if resp.ContentLength > c.opts.MaxSize {
		return "", "", 0, fmt.Errorf("%w: %d bytes announced, limit %d", ErrTooLarge, resp.ContentLength, c.opts.MaxSize)
	}
	f, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return "", "", 0, err
	}
	name := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close() // already failing, the original error is returned
			if rerr := os.Remove(name); rerr != nil {
				c.log.Warn("firmware: cannot remove temp file", "path", name, "err", rerr)
			}
		}
	}()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, c.opts.MaxSize+1))
	if err != nil {
		return "", "", 0, err
	}
	if size > c.opts.MaxSize {
		return "", "", 0, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, c.opts.MaxSize)
	}
	if err = f.Chmod(0o644); err != nil { // served to devices; CreateTemp makes 0600
		return "", "", 0, err
	}
	if err = f.Sync(); err != nil {
		return "", "", 0, err
	}
	if err = f.Close(); err != nil {
		return "", "", 0, err
	}
	return name, hex.EncodeToString(h.Sum(nil)), size, nil
}

// inspect reads the first bytes of the (gunzipped) image and asks Inspect
// for the chip family.
func (c *cache) inspect(path, filename string) (model.ChipFamily, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from the validated release and file name
	if err != nil {
		return model.ChipUnknown, err
	}
	defer func() { _ = f.Close() }() // read-only
	var r io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return model.ChipUnknown, fmt.Errorf("%w: %s is not gzip: %w", ErrImageMismatch, filename, err)
		}
		r = zr
	}
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return model.ChipUnknown, fmt.Errorf("%w: %s too short or corrupt: %w", ErrImageMismatch, filename, err)
	}
	fam, err := c.opts.Inspect(hdr)
	if err != nil {
		return model.ChipUnknown, fmt.Errorf("%w: %s: %w", ErrImageMismatch, filename, err)
	}
	return fam, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is built from the validated release name
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only handle
	return d.Sync()
}

// ---- prune ----

// Prune deletes every release beyond the newest keep cached ones, except
// releases that contain an artifact used by an active or queued job. Uploads
// (custom firmware) are never touched. Every file goes through Delete, so it
// takes the per-file lock and re-checks "in use" at the moment of deletion.
// If the in-use lookup fails nothing is deleted. Rows whose names could point
// outside the cache directory are skipped, never deleted.
func (c *cache) Prune(ctx context.Context, keep int) (DeleteResult, error) {
	var res DeleteResult
	if keep < 0 {
		return res, fmt.Errorf("firmware: Prune keep=%d", keep)
	}
	all, err := c.store.ListArtifacts(ctx)
	if err != nil {
		return res, err
	}
	inUse, err := c.ArtifactsInUse(ctx)
	if err != nil {
		return res, fmt.Errorf("firmware: prune aborted, cannot determine artifacts in use: %w", err)
	}
	byRel := map[string][]model.Artifact{}
	for _, a := range all {
		if IsCustomSource(a.Source) || a.Custom != "" {
			continue // uploads are kept until an admin deletes them
		}
		if !c.confined(a) {
			c.log.Warn("firmware: prune skips an artifact whose name leaves the cache directory", "id", a.ID, "release", a.Release, "file", a.Filename)
			continue
		}
		byRel[a.Release] = append(byRel[a.Release], a)
	}
	rels := make([]string, 0, len(byRel))
	for r := range byRel {
		rels = append(rels, r)
	}
	sort.Slice(rels, func(i, j int) bool { return compareVersions(rels[i], rels[j]) > 0 })
	for i, rel := range rels {
		if i < keep {
			continue
		}
		busy := false
		for _, a := range byRel[rel] {
			_, held := inUse[a.ID]
			busy = busy || held
		}
		if busy {
			res.Kept += len(byRel[rel])
			continue
		}
		for _, a := range byRel[rel] {
			deleted, err := c.Delete(ctx, a.ID)
			var held *InUseError
			switch {
			case err == nil:
				res.Deleted = append(res.Deleted, deleted)
				res.Freed += deleted.Size
			case errors.As(err, &held):
				res.Kept++ // a job took it since the lookup above
			case errors.Is(err, store.ErrNotFound):
			default:
				return res, err
			}
		}
	}
	return res, nil
}

// confined reports whether the file of a cached (non-upload) artifact lies
// inside the cache directory with plain release and file names.
func (c *cache) confined(a model.Artifact) bool {
	if !validRelease(a.Release) || !validPathElement(a.Filename) {
		return false
	}
	rel, err := filepath.Rel(c.opts.Dir, c.path(a.Release, a.Filename))
	return err == nil && filepath.IsLocal(rel)
}

// Open returns the cached file of an artifact for a push upload to a device.
// The file is hashed first: only content that still matches the artifact's
// SHA-256 and size is ever sent to a device.
func (c *cache) Open(_ context.Context, a model.Artifact) (io.ReadCloser, error) {
	f, err := os.Open(c.pathOf(a))
	if err != nil {
		return nil, fmt.Errorf("firmware: open %s: %w", a.Filename, err)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err == nil && (n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA256) {
		err = errors.New("content does not match the cached artifact")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close() // the read error is what matters
		return nil, fmt.Errorf("firmware: %s: %w", a.Filename, err)
	}
	return f, nil
}
