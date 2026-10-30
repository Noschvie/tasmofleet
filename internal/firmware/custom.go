package firmware

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Sources of artifacts that do not come from an upstream. A custom release
// is the numeric version the admin gave the upload; it holds the uploaded
// image (CustomSource) and, for ESP8266, a copy of an upstream minimal image
// (customMinimalSource) so the two-step update works from the same directory.
const (
	CustomSource        = "custom"
	customMinimalSource = "custom-minimal"
)

// Upload limits, measured on the uncompressed image. A Tasmota image is far
// below these (ESP8266 about 0.6 MiB, ESP32 variants up to about 3 MiB), the caps
// only bound disk use and decompression bombs.
const (
	MaxCustomESP8266 int64 = 4 << 20
	MaxCustomESP32   int64 = 8 << 20
	// MaxCustomUpload is the largest request body the web layer accepts: a
	// gzip file can only be larger than its content by a few bytes per block.
	MaxCustomUpload = MaxCustomESP32 + 256<<10

	minCustomSize  = 1024
	maxLabelRunes  = 64
	maxNameRunes   = 128
	maxVersionLen  = 32
	partitionTable = 0x8000 // flash offset of the partition table; a full-flash image has its magic here
	prefixLen      = partitionTable + 2
)

// ErrCustomImage is wrapped by every refusal of an uploaded image or of its
// metadata (wrong chip, safeboot or full-flash image, corrupt, bad version).
var ErrCustomImage = errors.New("firmware: custom image refused")

// CustomInput is one upload. Body is read once and never trusted: container
// (gzip or not) and chip family come from its bytes, never from FileName or
// the browser's content type.
type CustomInput struct {
	Body       io.Reader
	FileName   string // as the browser sent it; display and audit only
	Label      string // admin-given name
	Version    string // dotted numeric, becomes the release directory
	Variant    string // build name, default "tasmota"; decides the stored file name
	SingleCore bool   // classic ESP32 only: a tasmota32solo1 build
	Actor      string
}

// CustomImage is an uploaded image with its provenance.
type CustomImage struct {
	Artifact     model.Artifact
	Label        string
	OriginalName string
	UploadedBy   string
	UploadedAt   time.Time
}

// Custom is implemented by the cache returned by New when its store keeps
// custom metadata (store.CustomFirmwareStore).
type Custom interface {
	// AddCustom checks and stores an uploaded image. Nothing is stored on any
	// refusal. Deleting goes through Manager.Delete like any cached file.
	AddCustom(ctx context.Context, in CustomInput) (CustomImage, error)
	// CustomImages lists the uploads, newest first.
	CustomImages(ctx context.Context) ([]CustomImage, error)
	// EnsureCustom returns a file of the upload with the given key (the image
	// or, for ESP8266, its minimal image), never an upstream file.
	EnsureCustom(ctx context.Context, key, release, filename string) (model.Artifact, error)
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCustomImage}, args...)...)
}

// IsCustomSource reports whether an artifact source belongs to a custom release.
func IsCustomSource(s string) bool { return s == CustomSource || s == customMinimalSource }

// CleanFileName reduces a browser-sent file name to something safe to show
// and audit: base name only, no control characters, bounded length.
func CleanFileName(s string) string {
	s = path.Base(strings.ReplaceAll(s, "\\", "/"))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxNameRunes {
		s = string(r[:maxNameRunes])
	}
	if s == "." || s == "/" {
		return ""
	}
	return s
}

func validCustomVersion(v string) bool {
	if v == "" || len(v) > maxVersionLen || strings.Count(v, ".") > 4 {
		return false
	}
	for _, f := range strings.Split(v, ".") {
		if f == "" || len(f) > 6 {
			return false
		}
		for _, r := range f {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func validLabel(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > maxLabelRunes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// customFilename is the stored file name: exactly what fwselect.Select returns
// for a device that keeps (or is told to switch to) this variant, so the Guard
// accepts the artifact for the matching devices and for no others.
func customFilename(fam model.ChipFamily, singleCore bool, variant string) (string, error) {
	// No device: nothing to read a language from, and the variant choice carries none.
	id := model.Identity{Family: fam, Version: "1", Variant: "tasmota", SingleCore: singleCore && fam == model.ChipESP32, LanguageKnown: true}
	sel, err := fwselect.Select(id, fwselect.Choice{Variant: variant, AllowVariantChange: true})
	if err != nil {
		return "", refuse("variant %q is not a Tasmota build for %s", variant, fam)
	}
	return sel.Filename, nil
}

// refusedName catches images whose own file name says they are not an OTA
// app image. The header cannot: safeboot and the app image share it.
func refusedName(name string) string {
	l := strings.ToLower(name)
	for _, w := range []string{"safeboot", "factory", "bootloader", "partition"} {
		if strings.Contains(l, w) {
			return w
		}
	}
	return ""
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *cache) AddCustom(ctx context.Context, in CustomInput) (CustomImage, error) {
	cs, ok := c.store.(store.CustomFirmwareStore)
	if !ok {
		return CustomImage{}, errors.New("firmware: this store cannot keep custom images")
	}
	in.Label, in.Version = strings.TrimSpace(in.Label), strings.TrimSpace(in.Version)
	in.FileName = CleanFileName(in.FileName)
	if in.Variant = strings.TrimSpace(in.Variant); in.Variant == "" {
		in.Variant = "tasmota"
	}
	switch {
	case !validLabel(in.Label):
		return CustomImage{}, refuse("the label must be 1 to %d printable characters", maxLabelRunes)
	case !validCustomVersion(in.Version) || !validRelease(in.Version):
		return CustomImage{}, refuse("the version must be dotted numbers such as 16.0.0.1")
	}
	if w := refusedName(in.FileName); w != "" {
		return CustomImage{}, refuse("%q looks like a %s image, which is never an OTA target", in.FileName, w)
	}
	br := bufio.NewReader(in.Body)
	var src io.Reader = br
	if magic, _ := br.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) { // a short body fails below
		zr, err := gzip.NewReader(br)
		if err != nil {
			return CustomImage{}, refuse("not a valid gzip file: %v", err)
		}
		src = zr
	}
	prefix := make([]byte, prefixLen)
	n, err := io.ReadFull(src, prefix)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return CustomImage{}, fmt.Errorf("%w: the file is unreadable or corrupt: %w", ErrCustomImage, err)
	}
	prefix = prefix[:n]
	if n < headerLen {
		return CustomImage{}, refuse("the file is too short to be a firmware image")
	}
	fam, err := c.opts.Inspect(prefix[:headerLen])
	if err != nil {
		return CustomImage{}, refuse("not a Tasmota/ESP firmware image: %v", err)
	}
	if n >= prefixLen && prefix[partitionTable] == 0xAA && prefix[partitionTable+1] == 0x50 {
		return CustomImage{}, refuse("this is a full-flash image (it contains a partition table), not an OTA app image")
	}
	filename, err := customFilename(fam, in.SingleCore, in.Variant)
	if err != nil {
		return CustomImage{}, err
	}
	limit := MaxCustomESP32
	if fam == model.ChipESP8266 {
		limit = MaxCustomESP8266
	}

	customDir := filepath.Join(c.opts.Dir, customDirName)
	if err := os.MkdirAll(customDir, 0o750); err != nil {
		return CustomImage{}, fmt.Errorf("firmware: create custom dir: %w", err)
	}
	tmp, sum, size, err := c.writeCustom(customDir, fam, prefix, src, limit)
	dropTemp := func() {
		if tmp != "" {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				c.log.Warn("firmware: cannot remove temp file", "path", tmp, "err", rerr)
			}
		}
	}
	defer dropTemp()
	if err != nil {
		return CustomImage{}, err
	}

	// The key is part of the URL a device fetches; the same version number as
	// an upstream release or another upload is fine, the same bytes are not.
	key := sum[:customKeyLen]
	unlock := c.lock("custom/" + key)
	defer unlock()
	all, err := c.store.ListArtifacts(ctx)
	if err != nil {
		return CustomImage{}, err
	}
	for _, o := range all {
		if o.Custom == key && o.Source == CustomSource {
			return CustomImage{}, refuse("this exact file is already stored (version %s)", o.Release)
		}
	}
	a := model.Artifact{Release: in.Version, Filename: filename, Family: fam, Variant: in.Variant, Size: size, SHA256: sum,
		Source: CustomSource, Custom: key, FetchedAt: c.clk.Now()}
	relDir := filepath.Dir(c.pathOf(a))
	if err := os.MkdirAll(relDir, 0o750); err != nil {
		return CustomImage{}, fmt.Errorf("firmware: create release dir: %w", err)
	}
	final := c.pathOf(a)
	if err := os.Rename(tmp, final); err != nil {
		return CustomImage{}, fmt.Errorf("firmware: install %s: %w", filename, err)
	}
	if err := syncDir(relDir); err != nil {
		c.log.Warn("firmware: fsync of release dir failed", "dir", relDir, "err", err)
	}
	id, err := c.store.PutArtifact(ctx, a)
	if err == nil {
		a.ID = id
		meta := store.CustomMeta{ArtifactID: id, Label: in.Label, OriginalName: in.FileName, UploadedBy: in.Actor, UploadedAt: a.FetchedAt}
		if err = cs.PutCustomMeta(ctx, meta); err != nil {
			if derr := c.store.DeleteArtifact(ctx, id); derr != nil {
				c.log.Warn("firmware: cannot drop the index row of a failed upload", "id", id, "err", derr)
			}
		}
	}
	if err != nil {
		if rerr := os.Remove(final); rerr != nil {
			c.log.Warn("firmware: cannot remove unindexed file", "path", final, "err", rerr)
		}
		c.removeCustomDirs(a)
		return CustomImage{}, fmt.Errorf("firmware: index %s: %w", filename, err)
	}
	c.log.Info("firmware: custom image stored", "release", in.Version, "key", key, "file", filename, "family", fam, "size", size, "by", in.Actor)
	return CustomImage{Artifact: a, Label: in.Label, OriginalName: in.FileName, UploadedBy: in.Actor, UploadedAt: a.FetchedAt}, nil
}

// writeCustom streams prefix and the rest of src into a temp file in dir,
// normalised to the container the Guard expects (gzip for ESP8266, raw for
// ESP32), bounded by limit uncompressed bytes. It returns the temp path (also
// on error, so the caller removes it), the SHA-256 and the size of the stored file.
func (c *cache) writeCustom(dir string, fam model.ChipFamily, prefix []byte, src io.Reader, limit int64) (tmp, sum string, size int64, err error) {
	f, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return "", "", 0, err
	}
	tmp = f.Name()
	defer func() {
		if err != nil {
			_ = f.Close() // already failing, the original error is returned
		}
	}()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	var w io.Writer = cw
	var gw *gzip.Writer
	if fam == model.ChipESP8266 {
		gw, _ = gzip.NewWriterLevel(cw, gzip.BestCompression) // the level is valid
		w = gw
	}
	total := int64(len(prefix))
	if _, err = w.Write(prefix); err != nil {
		return tmp, "", 0, err
	}
	rest, err := io.Copy(w, io.LimitReader(src, limit-total+1))
	if err != nil {
		return tmp, "", 0, fmt.Errorf("%w: the file is unreadable or corrupt: %w", ErrCustomImage, err)
	}
	if total += rest; total > limit {
		err = fmt.Errorf("%w: a %s image may be at most %d bytes uncompressed", ErrTooLarge, fam, limit)
		return tmp, "", 0, err
	}
	if total < minCustomSize {
		err = refuse("the image is only %d bytes, too small to be firmware", total)
		return tmp, "", 0, err
	}
	if gw != nil {
		if err = gw.Close(); err != nil {
			return tmp, "", 0, err
		}
	}
	if err = f.Chmod(0o644); err != nil { // served to devices; CreateTemp makes 0600
		return tmp, "", 0, err
	}
	if err = f.Sync(); err != nil {
		return tmp, "", 0, err
	}
	if err = f.Close(); err != nil {
		return tmp, "", 0, err
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

// pathOf is where the file of a lives: <dir>/<release>/<file> upstream,
// <dir>/custom/<key>/<release>/<file> for an upload. "custom" is not a valid
// upstream path element here because upstream releases are digits and dots.
func (c *cache) pathOf(a model.Artifact) string {
	if a.Custom == "" {
		return c.path(a.Release, a.Filename)
	}
	return filepath.Join(c.opts.Dir, customDirName, a.Custom, a.Release, a.Filename)
}

// removeCustomDirs removes the (empty) directories of an upload; a directory
// that still holds a file is left alone.
func (c *cache) removeCustomDirs(a model.Artifact) {
	rel := filepath.Dir(c.pathOf(a))
	for _, d := range []string{rel, filepath.Dir(rel)} {
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Debug("firmware: directory kept", "dir", d, "err", err)
			return
		}
	}
}

func (c *cache) CustomImages(ctx context.Context) ([]CustomImage, error) {
	cs, ok := c.store.(store.CustomFirmwareStore)
	if !ok {
		return nil, nil
	}
	metas, err := cs.ListCustomMeta(ctx)
	if err != nil {
		return nil, err
	}
	var out []CustomImage
	for _, m := range metas {
		a, err := c.store.GetArtifact(ctx, m.ArtifactID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, CustomImage{Artifact: a, Label: m.Label, OriginalName: m.OriginalName, UploadedBy: m.UploadedBy, UploadedAt: m.UploadedAt})
	}
	return out, nil
}

// EnsureCustom returns a file of an upload: the image itself, or for an
// ESP8266 upload the minimal image kept with it (copied on first use). It
// never reaches upstream and never returns an upstream file.
func (c *cache) EnsureCustom(ctx context.Context, key, release, filename string) (model.Artifact, error) {
	if !validPathElement(key) || !validRelease(release) {
		return model.Artifact{}, fmt.Errorf("%w: custom %q release %q", ErrBadName, key, release)
	}
	if _, err := parseName(filename); err != nil {
		return model.Artifact{}, err
	}
	unlock := c.lock("custom/" + key)
	defer unlock()
	cs, ok := c.store.(store.CustomFirmwareStore)
	if !ok {
		return model.Artifact{}, errors.New("firmware: this store cannot keep custom images")
	}
	a, err := cs.FindCustomArtifact(ctx, key, release, filename)
	if err == nil {
		if fi, serr := os.Stat(c.pathOf(a)); serr == nil && fi.Mode().IsRegular() && fi.Size() == a.Size {
			return a, nil
		}
		c.log.Warn("firmware: custom file missing or wrong size", "key", key, "file", filename)
		return model.Artifact{}, fmt.Errorf("%w: custom %s/%s is damaged", ErrNotInIndex, release, filename)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Artifact{}, err
	}
	if filename != minimalFile {
		return model.Artifact{}, fmt.Errorf("%w: custom %s/%s", ErrNotInIndex, release, filename)
	}
	// Only a release that has an upload may borrow a minimal image.
	all, err := c.store.ListArtifacts(ctx)
	if err != nil {
		return model.Artifact{}, err
	}
	for _, o := range all {
		if o.Custom == key && o.Release == release && o.Source == CustomSource {
			return c.customMinimal(ctx, key, release)
		}
	}
	return model.Artifact{}, fmt.Errorf("%w: custom %s/%s", ErrNotInIndex, release, filename)
}

// customMinimal gives a custom release its tasmota-minimal.bin.gz: a copy of
// the newest upstream minimal image, so the staged update (and Tasmota's own
// fallback, which looks in the OtaUrl directory) finds it next to the upload.
// The copy is re-hashed and re-inspected; Guard still checks it as usual.
func (c *cache) customMinimal(ctx context.Context, key, release string) (model.Artifact, error) {
	srcArt, srcPath, err := c.newestUpstreamMinimal(ctx)
	if err != nil {
		return model.Artifact{}, err
	}
	a := model.Artifact{Release: release, Filename: minimalFile, Custom: key}
	relDir := filepath.Dir(c.pathOf(a))
	if err := os.MkdirAll(relDir, 0o750); err != nil {
		return model.Artifact{}, err
	}
	in, err := os.Open(srcPath) //nolint:gosec // path of an indexed upstream file under our cache dir
	if err != nil {
		return model.Artifact{}, err
	}
	defer func() { _ = in.Close() }() // read-only
	f, err := os.CreateTemp(relDir, tmpPrefix+"*")
	if err != nil {
		return model.Artifact{}, err
	}
	tmp := f.Name()
	defer func() {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			c.log.Warn("firmware: cannot remove temp file", "path", tmp, "err", rerr)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(in, c.opts.MaxSize+1))
	if err == nil && (n != srcArt.Size || hex.EncodeToString(h.Sum(nil)) != srcArt.SHA256) {
		err = fmt.Errorf("firmware: cached %s/%s no longer matches its checksum", srcArt.Release, srcArt.Filename)
	}
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return model.Artifact{}, err
	}
	fam, err := c.inspect(tmp, minimalFile)
	if err != nil {
		return model.Artifact{}, err
	}
	if fam != model.ChipESP8266 {
		return model.Artifact{}, fmt.Errorf("%w: %s is not an ESP8266 image", ErrImageMismatch, minimalFile)
	}
	if err := os.Rename(tmp, c.pathOf(a)); err != nil {
		return model.Artifact{}, err
	}
	a.Family, a.Variant, a.Size, a.SHA256 = fam, "minimal", n, srcArt.SHA256
	a.Source, a.SourceURL, a.FetchedAt = customMinimalSource, "copy of "+srcArt.Release+"/"+minimalFile, c.clk.Now()
	id, err := c.store.PutArtifact(ctx, a)
	if err != nil {
		if rerr := os.Remove(c.pathOf(a)); rerr != nil {
			c.log.Warn("firmware: cannot remove unindexed file", "release", release, "err", rerr)
		}
		return model.Artifact{}, err
	}
	a.ID = id
	return a, nil
}

const customDirName = "custom" // below the cache dir: <key>/<release>/<file>

// customKeyLen is the number of SHA-256 hex digits that name an upload in its URL.
const customKeyLen = 10

const minimalFile = "tasmota-minimal.bin.gz"

// newestUpstreamMinimal finds the newest cached upstream minimal image, or
// downloads the newest release's (through Ensure: size limit, cross-check and
// header check unchanged).
func (c *cache) newestUpstreamMinimal(ctx context.Context) (model.Artifact, string, error) {
	all, err := c.store.ListArtifacts(ctx)
	if err != nil {
		return model.Artifact{}, "", err
	}
	var cands []model.Artifact
	for _, a := range all {
		if a.Filename == minimalFile && a.Custom == "" {
			cands = append(cands, a)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return compareVersions(cands[i].Release, cands[j].Release) > 0 })
	for _, a := range cands {
		if a, ok, err := c.cached(ctx, a.Release, minimalFile); err == nil && ok {
			return a, c.path(a.Release, minimalFile), nil
		}
	}
	rels, err := c.Releases(ctx)
	if err != nil {
		return model.Artifact{}, "", fmt.Errorf("firmware: no cached minimal image and the index is unavailable: %w", err)
	}
	for _, r := range rels {
		for _, f := range r.Files {
			if f != minimalFile {
				continue
			}
			a, err := c.Ensure(ctx, r.Version, minimalFile)
			if err != nil {
				return model.Artifact{}, "", err
			}
			return a, c.path(a.Release, minimalFile), nil
		}
	}
	return model.Artifact{}, "", fmt.Errorf("%w: no upstream release offers %s to stage the custom image", ErrNotInIndex, minimalFile)
}
