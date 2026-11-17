package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Limits of the files step (spec 12.1). A file over a limit is listed in the
// manifest and not stored.
const (
	MaxFileBytes int64 = 1 << 20 // per file
	MaxRunBytes  int64 = 4 << 20 // stored bytes per run
	MaxFiles           = 100     // files read per run
	MaxFolders         = 100     // folders listed per run
	MaxDepth           = 4       // folder depth below the root
)

// ManifestName is the first entry of every files archive. Device dot files are
// never listed, so no device file can carry this name.
const ManifestName = ".tasmofleet-manifest.json"

// NoteFilesPrefix starts the sentence a run's dump note carries when the run
// has no files archive; the rest says why. See FilesNoteOf.
const NoteFilesPrefix = "files not backed up: "

// NoMACWhy is what a backup or a restore says about a device whose inventory entry holds no MAC: nothing can show that
// the host at its address is the device, so nothing is stored from it or sent to it (#846).
const NoMACWhy = "the inventory has no MAC for this device (probe it first)"

// ErrNoFilesArchive: the run has no files archive (see RunFiles.Note for why).
var ErrNoFilesArchive = errors.New("backup: the run has no files archive")

// ErrBadArchive: a files archive does not match its manifest or breaks the
// path rules (a hand-edited or damaged file). Nothing from it may be used.
var ErrBadArchive = errors.New("backup: files archive is damaged")

// Manifest is the first entry of a files archive. Paths are device paths
// ("/lib/x.be"); the tar entries are the same without the leading slash.
type Manifest struct {
	FSType   int            `json:"fs_type"`
	FSSizeKB int            `json:"fs_size_kb"`
	FSFreeKB int            `json:"fs_free_kb"`
	Files    []ManifestFile `json:"files"`   // stored in the archive
	Skipped  []ManifestSkip `json:"skipped"` // seen on the device, not stored
	// Via is the transport that read the listing and the files: "mqtt" (FileDownload Type 8, #924) or "http". It is
	// empty in archives from before #924, which were all read over HTTP.
	Via string `json:"via,omitempty"`
}

// ManifestFile is a stored file.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Stored bool   `json:"stored"` // always true here; kept for readers of the raw manifest
	// ModTime is the device's wall clock ("2006-01-02T15:04:05", no zone), empty if unknown.
	ModTime string `json:"mod_time,omitempty"`
	// PrivateKey marks /tlskey: the UI says "private key" and never shows content.
	PrivateKey bool `json:"private_key,omitempty"`
}

// ManifestSkip is a file or folder that is listed but not in the archive.
type ManifestSkip struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Reason     string `json:"reason"`
	PrivateKey bool   `json:"private_key,omitempty"`
}

// FilesArchive is a parsed, verified files archive.
type FilesArchive struct {
	Backup   model.Backup // the row of the archive
	Manifest Manifest
	contents map[string][]byte
}

// RunFiles describes what a run holds of the device's files.
type RunFiles struct {
	// Archive is the run's files row; nil when the run has none.
	Archive *model.Backup
	// Manifest is the parsed manifest of Archive; nil without an archive.
	Manifest *Manifest
	// Note says why a run has no archive ("no filesystem", "they need HTTP",
	// "firmware older than 14.6.0", ...), without NoteFilesPrefix; "" when
	// there is an archive or the run predates the files step.
	Note string
}

// privateKeyPath is the TLS private key Tasmota keeps in the root of its file system.
const privateKeyPath = "/tlskey"

// IsPrivateKey reports whether the device path is the TLS private key.
func IsPrivateKey(devicePath string) bool { return devicePath == privateKeyPath }

// FilesNoteOf extracts the reason a run has no files archive from its dump
// note (the sentence after NoteFilesPrefix, always the last part of the note);
// "" if the note has none.
func FilesNoteOf(dumpNote string) string {
	i := strings.LastIndex(dumpNote, NoteFilesPrefix)
	if i < 0 {
		return ""
	}
	return dumpNote[i+len(NoteFilesPrefix):]
}

// Reasons shown in the manifest and in notes. They are fixed sentences: error
// text can carry device URLs, and a URL can carry the web password.
const (
	whyTooLarge   = "larger than 1 MiB"
	whyRunCap     = "run limit of 4 MiB reached"
	whyCount      = "more than 100 files"
	whyFolders    = "more than 100 folders"
	whyDepth      = "deeper than 4 folders"
	whyName       = "name not accepted"
	whyGone       = "file no longer on the device"
	whyNotAFile   = "not a file"
	whyFailed     = "could not be read"
	whyList       = "folder could not be listed"
	whyDeviceGone = "not read: the device did not answer"
	whyAPIOff     = "not read: " + tasmota.APIOffName
	whyTimeout    = "not read: the device took too long to answer"
	whyAuth       = "not read: the device rejected the web password"
	whyOutside    = "not read: the device address is outside the configured device networks"
	whyTimeLimit  = "not read: time limit of the backup reached"
	whyRefused    = "not read: the device refused its MQTT password"
)

// Why the files of a run over MQTT went to HTTP instead (#924), for the run's reason when HTTP then fails too.
const (
	whyNoType8 = "this firmware sends no files over MQTT"
	whySDCard  = "an SD card is mounted"
)

// needHTTP is the reason a run has no files when MQTT could not carry them (mqttWhy) and HTTP then failed (httpReason,
// a reasonOf sentence). Without an mqttWhy the HTTP reason stands alone, as before #924.
func needHTTP(mqttWhy, httpReason string) string {
	if mqttWhy == "" {
		return httpReason
	}
	if httpReason == reasonOf(transport.ErrFilesNeedHTTP) {
		httpReason = "HTTP has not answered recently"
	}
	return "they need HTTP (" + mqttWhy + "), and HTTP could not be used: " + httpReason
}

// reasonOf chooses the sentence for a failed call from the error's kind.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, transport.ErrFilesNeedHTTP):
		return "they need HTTP"
	case errors.Is(err, tasmota.ErrAuth):
		return "the device rejected the web password"
	case errors.Is(err, tasmota.ErrHTTPAPIRefused): // before ErrUnreachable, which it wraps
		return tasmota.APIOffName
	case errors.Is(err, tasmota.ErrUnreachable):
		return "the device did not answer"
	case errors.Is(err, tasmota.ErrTimeout):
		return "the device took too long to answer"
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "the device address is outside the configured device networks"
	case errors.Is(err, tasmota.ErrNotImplemented):
		return "this connection cannot read files"
	case errors.Is(err, mqttcmd.ErrPasswordRefused):
		return strings.TrimPrefix(whyRefused, "not read: ")
	}
	return "the file system could not be read"
}

// fileReader is the part of a file system the walk uses: tasmota.FileSystem over HTTP, mqttFS over MQTT.
type fileReader interface {
	ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error)
	DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error)
}

// errNoType8 ends a walk over MQTT whose first published file start the device answered with Error 3
// (mqttcmd.ErrNoFileTransfer).
var errNoType8 = errors.New("backup: the firmware sends no files over MQTT")

// walker reads the device's file system into a manifest and file contents.
type walker struct {
	parent  context.Context // the caller's: only its error aborts the run
	ctx     context.Context // the walk's own, ending earlier
	fs      fileReader
	t       tasmota.Target
	man     Manifest
	data    map[string][]byte
	total   int64
	reads   int
	folders int
	rootErr error  // the root could not be listed: there is nothing to describe
	dead    string // set once the device stopped answering: the reason for everything left
	// overMQTT: the walk reads over MQTT (#924). It handles a folder's files before its subfolders, so a stock
	// firmware's Error 3 to the first file ends it after as few listings as possible, and that Error 3 ends the walk
	// (errNoType8) when no earlier start was published (started).
	overMQTT bool
	// started: a file call of this walk published its start, and the device did not answer it with Error 3. A call
	// refused before anything was published (a path the channel cannot send, a router refusal) does not count.
	started bool
}

func (w *walker) skip(p string, size int64, reason string) {
	w.man.Skipped = append(w.man.Skipped, ManifestSkip{Path: p, Size: size, Reason: reason, PrivateKey: IsPrivateKey(p)})
}

// dir lists one folder and handles its entries in name order. Only a context
// error is returned: everything else is recorded in the manifest.
func (w *walker) dir(dir string, depth int) error {
	w.folders++
	entries, err := w.fs.ListFiles(w.ctx, w.t, dir)
	var sk *tasmota.SkippedNamesError
	switch {
	case errors.As(err, &sk):
		for _, n := range sk.Names {
			w.skip(join(dir, cleanName(n)), 0, whyName)
		}
	case err != nil:
		if cerr := w.parent.Err(); cerr != nil {
			return cerr
		}
		w.noteDead(err)
		if dir == "/" {
			w.rootErr = err
			return nil
		}
		w.skip(dir, 0, whyList)
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if w.overMQTT {
		sort.SliceStable(entries, func(i, j int) bool { return !entries[i].Dir && entries[j].Dir })
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name] { // a repeated name would give the tar two entries of one path
			continue
		}
		seen[e.Name] = true
		p := join(dir, e.Name)
		var err error
		switch {
		case e.Dir:
			err = w.folder(p, depth+1)
		default:
			err = w.file(p, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) folder(p string, depth int) error {
	switch {
	case w.dead != "":
		w.skip(p, 0, w.dead)
	case depth > MaxDepth:
		w.skip(p, 0, whyDepth)
	case w.folders >= MaxFolders:
		w.skip(p, 0, whyFolders)
	default:
		return w.dir(p, depth)
	}
	return nil
}

func (w *walker) file(p string, e tasmota.FileEntry) error {
	switch {
	case w.dead != "":
		w.skip(p, e.Size, w.dead)
		return nil
	case e.Size > MaxFileBytes:
		w.skip(p, e.Size, whyTooLarge)
		return nil
	case w.reads >= MaxFiles:
		w.skip(p, e.Size, whyCount)
		return nil
	case w.total+e.Size > MaxRunBytes:
		w.skip(p, e.Size, whyRunCap)
		return nil
	}
	w.reads++
	// The limit of this download is what the run may still store: a file that grew since the
	// listing is refused by its Content-Length before a body byte is read.
	limit, runBound := MaxFileBytes, false
	if left := MaxRunBytes - w.total; left < limit {
		limit, runBound = left, true
	}
	if limit <= 0 { // nothing left (a client treats max <= 0 as "its default")
		w.skip(p, e.Size, whyRunCap)
		return nil
	}
	b, err := w.fs.DownloadFile(w.ctx, w.t, p, limit)
	if err != nil {
		if cerr := w.parent.Err(); cerr != nil {
			return cerr
		}
		// Error 3 to the first start that was published: the firmware has no file transfer over MQTT and sent nothing
		// of the file. After a published start it is that file's failure: the device took Type 8 already, and the
		// walk never moves to HTTP (T2/T3).
		if w.overMQTT && !w.started && errors.Is(err, mqttcmd.ErrNoFileTransfer) {
			return errNoType8
		}
		if errors.Is(err, mqttcmd.ErrPublished) {
			w.started = true
		}
		w.noteDead(err)
		reason := downloadReason(err, w.dead)
		if errors.Is(err, tasmota.ErrFileTooLarge) && runBound {
			reason = whyRunCap
		}
		w.skip(p, e.Size, reason)
		return nil
	}
	w.started = true
	if w.total+int64(len(b)) > MaxRunBytes { // the client let more through than asked for
		w.skip(p, int64(len(b)), whyRunCap)
		return nil
	}
	sum := sha256.Sum256(b)
	mt := ""
	if !e.ModTime.IsZero() {
		mt = e.ModTime.Format(modTimeLayout)
	}
	w.man.Files = append(w.man.Files, ManifestFile{Path: p, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Stored: true, ModTime: mt, PrivateKey: IsPrivateKey(p)})
	w.data[p] = b
	w.total += int64(len(b))
	return nil
}

const modTimeLayout = "2006-01-02T15:04:05"

// noteDead remembers that the device stopped answering, so the rest of the
// walk sends nothing more: every further call would wait for its own timeout.
func (w *walker) noteDead(err error) {
	if w.ctx.Err() != nil { // the walk's own deadline: the call was cut off by us, not by the device
		w.dead = whyTimeLimit
		return
	}
	switch {
	case errors.Is(err, mqttcmd.ErrPasswordRefused): // every further file would send the refused password again
		w.dead = whyRefused
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		w.dead = whyOutside
	case errors.Is(err, tasmota.ErrAuth):
		w.dead = whyAuth
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		w.dead = whyAPIOff
	case errors.Is(err, tasmota.ErrUnreachable):
		w.dead = whyDeviceGone
	case errors.Is(err, tasmota.ErrTimeout):
		w.dead = whyTimeout
	}
}

func downloadReason(err error, dead string) string {
	switch {
	case dead != "":
		return strings.TrimPrefix(dead, "not read: ")
	case errors.Is(err, tasmota.ErrFileNotFound):
		return whyGone
	case errors.Is(err, tasmota.ErrNotAFile):
		return whyNotAFile
	case errors.Is(err, tasmota.ErrFileTooLarge):
		return whyTooLarge
	case errors.Is(err, tasmota.ErrBadPath):
		return whyName
	}
	return whyFailed
}

func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// cleanName makes a name the client refused safe to put into a manifest:
// control characters become "?" and it is cut at 100 bytes.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > 100 {
		s = strings.ToValidUTF8(s[:100], "?")
	}
	return s
}

// filesResult is what the files step hands to the backup run.
type filesResult struct {
	archive     []byte // nil: no archive
	archiveNote string // Note of the archive row
	runNote     string // sentence for the dump note when there is no archive
	via         string // the transport the files came over (the walk's trace); set with archive
}

func notBacked(reason string) filesResult { return filesResult{runNote: NoteFilesPrefix + reason} }

// filesStep reads the device's flash files (best effort: only a cancelled
// context is an error). The caller holds the device lock; the client takes
// its own per-device lock for every call. After a dump over HTTP the listing and
// the files go over HTTP to that host. After a dump over MQTT (dumpOverMQTT, #607)
// the file system itself (UfsType, UfsSize, UfsFree) is asked on the topic first,
// so a device without a file system is asked nothing over HTTP (#855); a device
// without an SD card then sends its listing and its files over MQTT too (#924,
// FileDownload Type 8 where the firmware has USE_UFILESYS_MQTT; filesOverMQTT).
// Otherwise (a stock firmware's Error 3, an SD card, the file system question left
// unanswered) the files are read over HTTP only from a host that reports dev's MAC,
// since HTTP finds the device by its address and the broker by its topic.
//
// side collects the HTTP calls made beside a dump over MQTT, by name, for the audit entry of the run (nil: not collected).
func (m *Manager) filesStep(ctx context.Context, dev model.Device, t tasmota.Target, fwVersion string, at time.Time, dumpOverMQTT bool, side *[]string) (filesResult, error) {
	fs, ok := m.o.Client.(tasmota.FileSystem)
	if !ok {
		return filesResult{}, nil // a client without file access (tests with fake devices)
	}
	// The walk ends a few seconds before the caller's deadline (the pre-OTA backup has 30 s for
	// everything), so a stalling device costs the files, never the dump.
	wctx, cancel := walkContext(ctx)
	defer cancel()
	var info tasmota.FSInfo
	known := false // info came over MQTT
	mqttWhy := ""  // why the files go over HTTP although the dump came over MQTT, for the reason when HTTP fails too
	if dumpOverMQTT {
		// The device on its topic says whether it has a file system at all: no web password, no identity question.
		i, err := tasmota.ReadFSInfo(transport.WithPath(wctx, transport.MQTT), m.o.Client, t)
		switch {
		case err == nil:
			if why := filesUnsupported(fwVersion, i); why != "" {
				return notBacked(why), nil
			}
			info, known = i, true
		case ctx.Err() != nil:
			return filesResult{}, ctx.Err()
		default: // asked over HTTP below, after the identity question, as a read MQTT left unanswered (T2)
			m.o.Log.Info("backup: file system not read over MQTT", "component", "backup", "device", int64(t.DeviceID()), "reason", reasonOf(err))
		}
		if known {
			res, why, done, err := m.filesOverMQTT(ctx, wctx, dev, t, info, at)
			if done || err != nil {
				return res, err
			}
			mqttWhy = why
		}
		// The identity comes before the first request that carries the stored web password. Without a MAC in the
		// inventory there is no identity to ask for, whatever HTTP's state (#846): that is what the person can fix.
		if !hasMAC(dev) {
			return notBacked(needHTTP(mqttWhy, NoMACWhy)), nil
		}
		if g, ok := m.o.Client.(filesGate); ok {
			if err := g.FilesUsable(t); err != nil {
				return notBacked(needHTTP(mqttWhy, reasonOf(err))), nil
			}
		}
		mctx, mtr := transport.WithTrace(wctx)
		why, err := m.sameDeviceOverHTTP(ctx, mctx, dev, t)
		sideCall(side, "mac check", mtr)
		if why != "" || err != nil {
			return notBacked(needHTTP(mqttWhy, why)), err
		}
	}
	if !known {
		ictx, itr := transport.WithTrace(wctx)
		i, err := fs.FSInfo(ictx, t)
		sideCall(side, "file system", itr)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return filesResult{}, cerr
			}
			why := reasonOf(err)
			if wctx.Err() != nil {
				why = strings.TrimPrefix(whyTimeLimit, "not read: ")
			}
			m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "reason", why)
			return notBacked(why), nil
		}
		if why := filesUnsupported(fwVersion, i); why != "" {
			return notBacked(why), nil
		}
		info = i
	}

	fctx, ftr := transport.WithTrace(wctx)
	w := newWalker(ctx, fctx, fs, t, info)
	err := w.dir("/", 0)
	sideCall(side, "files", ftr)
	if err != nil {
		return filesResult{}, err
	}
	if w.rootErr != nil {
		why := reasonOf(w.rootErr)
		if w.dead == whyTimeLimit {
			why = strings.TrimPrefix(whyTimeLimit, "not read: ")
		}
		m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "reason", why)
		return notBacked(needHTTP(mqttWhy, why)), nil //nolint:nilerr // files are best effort: the reason goes into the run note, the run goes on
	}
	return m.archived(w, t, at, ftr.Via()), nil
}

func newWalker(parent, ctx context.Context, fs fileReader, t tasmota.Target, info tasmota.FSInfo) *walker {
	return &walker{parent: parent, ctx: ctx, fs: fs, t: t, data: map[string][]byte{},
		man: Manifest{FSType: info.Type, FSSizeKB: info.SizeKB, FSFreeKB: info.FreeKB, Files: []ManifestFile{}, Skipped: []ManifestSkip{}}}
}

// archived builds the files archive of a finished walk; via is the transport that carried it (the walk's trace).
func (m *Manager) archived(w *walker, t tasmota.Target, at time.Time, via string) filesResult {
	sort.Slice(w.man.Files, func(i, j int) bool { return w.man.Files[i].Path < w.man.Files[j].Path })
	sort.Slice(w.man.Skipped, func(i, j int) bool { return w.man.Skipped[i].Path < w.man.Skipped[j].Path })
	w.man.Via = via
	arc, err := writeArchive(w.man, w.data, at)
	if err != nil {
		m.o.Log.Error("backup: building the files archive failed", "component", "backup", "device", int64(t.DeviceID()), "err", err)
		return notBacked("the archive could not be built")
	}
	res := filesResult{archive: arc, via: via}
	if n := len(w.man.Skipped); n > 0 {
		res.archiveNote = fmt.Sprintf("files incomplete: %d %s not stored", n, plural(n, "file", "files"))
	}
	m.o.Log.Debug("backup: files read", "component", "backup", "device", int64(t.DeviceID()), "via", via, "stored", len(w.man.Files), "skipped", len(w.man.Skipped), "bytes", w.total)
	return res
}

// mqttFiles is the Router's file calls over MQTT (transport.Router.MQTTListFiles, MQTTDownloadFile): the device on
// its topic, under the transfer rule of the dump, never moved to HTTP.
type mqttFiles interface {
	MQTTListFiles(ctx context.Context, d model.Device, dir string) ([]tasmota.FileEntry, error)
	MQTTDownloadFile(ctx context.Context, d model.Device, path string, max int64) ([]byte, error)
}

// mqttFS is the walk's file system over MQTT for the device as the backup read it (dev): the router refuses a call
// once the device's MQTT settings differ from dev's, so the files come from the topic the dump came from.
type mqttFS struct {
	f   mqttFiles
	dev model.Device
}

func (m mqttFS) ListFiles(ctx context.Context, _ tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	return m.f.MQTTListFiles(ctx, m.dev, dir)
}

func (m mqttFS) DownloadFile(ctx context.Context, _ tasmota.Target, path string, max int64) ([]byte, error) {
	return m.f.MQTTDownloadFile(ctx, m.dev, path, max)
}

// filesOverMQTT reads the files of a device whose dump came over MQTT on its topic (#924): the listing (UfsList) and
// each file (FileDownload Type 8), with no identity question (the topic is the identity, as for the dump) and no HTTP
// request. done: the result stands (an archive, or why there is none). Otherwise the files go over HTTP, and why
// says what kept them off MQTT ("" when nothing can be said about the firmware): a device with an SD card (Type 8 and
// UfsList would reach the card, not the flash), a firmware that answered the first published file start with Error 3
// (nothing of the file was sent), or a router that sent nothing at all (transport.ErrFilesNotOverMQTT). Only a
// cancelled run is an error.
func (m *Manager) filesOverMQTT(ctx, wctx context.Context, dev model.Device, t tasmota.Target, info tasmota.FSInfo, at time.Time) (res filesResult, why string, done bool, err error) {
	mf, ok := m.o.Client.(mqttFiles)
	if !ok {
		return filesResult{}, "", false, nil
	}
	if info.HasSD {
		return filesResult{}, whySDCard, false, nil
	}
	fctx, ftr := transport.WithTrace(wctx)
	w := newWalker(ctx, fctx, mqttFS{f: mf, dev: dev}, t, info)
	w.overMQTT = true
	switch err := w.dir("/", 0); {
	case errors.Is(err, errNoType8):
		m.o.Log.Info("backup: the firmware sends no files over MQTT (FileDownload Error 3), reading them over HTTP", "component", "backup", "device", int64(t.DeviceID()))
		return filesResult{}, whyNoType8, false, nil
	case err != nil:
		return filesResult{}, "", false, err
	}
	if w.rootErr != nil {
		if errors.Is(w.rootErr, transport.ErrFilesNotOverMQTT) { // nothing was published: HTTP as before #924
			m.o.Log.Info("backup: files not read over MQTT", "component", "backup", "device", int64(t.DeviceID()), "err", w.rootErr)
			return filesResult{}, "", false, nil
		}
		why := reasonOf(w.rootErr)
		if w.dead == whyTimeLimit {
			why = strings.TrimPrefix(whyTimeLimit, "not read: ")
		}
		m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "via", "mqtt", "reason", why)
		return notBacked(why), "", true, nil
	}
	return m.archived(w, t, at, ftr.Via()), "", true, nil
}

// filesGate is the Router's check whether file calls for a device go out at all (transport.Router.FilesUsable).
type filesGate interface {
	FilesUsable(t tasmota.Target) error
}

// devMACs is every MAC the inventory holds for dev, normalised (transport.NormMAC); entries that normalise to "" are
// left out. It is the one list the host at dev's address is compared with, so "has a MAC" and "is this MAC" agree.
func devMACs(dev model.Device) []string {
	var out []string
	for _, m := range append([]string{dev.MAC}, dev.MACs...) {
		if n := transport.NormMAC(m); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// hasMAC reports whether the inventory holds a MAC for dev, the only thing the host at its address can be compared with.
func hasMAC(dev model.Device) bool { return len(devMACs(dev)) > 0 }

// sideCall lists name among the HTTP calls beside a dump over MQTT when HTTP carried calls made under tr. A call the
// router stopped before it sent anything (HTTP known to fail) is not listed, nor are calls that went over MQTT (#924).
func sideCall(side *[]string, name string, tr *transport.Trace) {
	if v := tr.Via(); side != nil && (v == "http" || v == "http+mqtt") {
		*side = append(*side, name)
	}
}

// filesUnsupported says why the files of a device with info on firmware fwVersion are not read, "" when they are.
func filesUnsupported(fwVersion string, info tasmota.FSInfo) string {
	switch {
	case tasmota.FilesSupported(fwVersion, info):
		return ""
	case !info.HasFlash():
		return "no filesystem"
	}
	return "firmware older than " + tasmota.MinFilesFirmware
}

// sameDeviceOverHTTP asks the host at the device's address for its MAC (transport.AskMAC: Status 5 over HTTP, without
// the stored web credentials unless the host asks for them) and says why its files must not be stored with the dump,
// "" when it is this device: one of dev's MACs. Only a cancelled run is an error.
func (m *Manager) sameDeviceOverHTTP(ctx, wctx context.Context, dev model.Device, t tasmota.Target) (string, error) {
	got, err := transport.AskMAC(transport.WithPath(wctx, transport.HTTP), m.o.Client, dev)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		if errors.Is(err, tasmota.ErrBadResponse) {
			return "the device at its address did not report its MAC", nil
		}
		why := reasonOf(err)
		if wctx.Err() != nil {
			why = strings.TrimPrefix(whyTimeLimit, "not read: ")
		}
		m.o.Log.Info("backup: files not read", "component", "backup", "device", int64(t.DeviceID()), "reason", why)
		return why, nil
	}
	if slices.Contains(devMACs(dev), got) {
		return "", nil
	}
	m.o.Log.Warn("backup: the host at the device's address reports another MAC; its files are not stored", "component", "backup", "device", int64(t.DeviceID()))
	return "the device at its address is not this device", nil
}

// walkMargin is how long before the caller's deadline the files walk ends, so the dump and the
// snapshot already read can still be stored.
const walkMargin = 5 * time.Second

// walkContext returns ctx limited to end walkMargin (at most half of the time left) before its
// deadline; without a deadline ctx itself.
func walkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	margin := min(walkMargin, time.Until(dl)/2)
	return context.WithDeadline(ctx, dl.Add(-margin))
}

// WithoutFilesNote removes the files sentence (always last) from a dump note, for views that
// do not show it yet.
func WithoutFilesNote(note string) string {
	i := strings.LastIndex(note, NoteFilesPrefix)
	if i < 0 {
		return note
	}
	return strings.TrimSuffix(strings.TrimSpace(note[:i]), ";")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writeArchive builds the tar: the manifest first, then the files by path.
func writeArchive(man Manifest, data map[string][]byte, at time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := putEntry(tw, ManifestName, mb, at); err != nil {
		return nil, err
	}
	for _, f := range man.Files {
		mt := at
		if f.ModTime != "" {
			if t, err := time.Parse(modTimeLayout, f.ModTime); err == nil {
				mt = t
			}
		}
		if err := putEntry(tw, strings.TrimPrefix(f.Path, "/"), data[f.Path], mt); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func putEntry(tw *tar.Writer, name string, b []byte, mt time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(b)), ModTime: mt.UTC()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// ParseFilesArchive verifies and indexes a files archive. It is strict because
// a restore writes what it returns to a device: the manifest must be the first
// entry, every manifest path must obey tasmota.CheckFSPath, every stored file
// must be a regular entry of the right size and SHA-256, and the archive may
// hold nothing the manifest does not list. Violations are ErrBadArchive.
func ParseFilesArchive(data []byte) (*FilesArchive, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	hd, err := tr.Next()
	if err != nil || hd.Name != ManifestName || hd.Typeflag != tar.TypeReg {
		return nil, badArchive("the manifest is not the first entry")
	}
	mb, err := io.ReadAll(io.LimitReader(tr, 1<<20+1))
	if err != nil || len(mb) > 1<<20 {
		return nil, badArchive("the manifest cannot be read")
	}
	var man Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return nil, badArchive("the manifest is not valid JSON")
	}
	listed := map[string]int{}
	for i := range man.Files {
		f := &man.Files[i]
		if err := tasmota.CheckFSPath(f.Path); err != nil {
			return nil, badArchive("a manifest path is not allowed")
		}
		if _, dup := listed[f.Path]; dup {
			return nil, badArchive("a path is listed twice")
		}
		if f.Size < 0 || f.Size > MaxFileBytes {
			return nil, badArchive("a file is larger than the limit")
		}
		f.PrivateKey = IsPrivateKey(f.Path)
		f.Stored = true
		listed[f.Path] = i
	}
	for i := range man.Skipped {
		man.Skipped[i].PrivateKey = IsPrivateKey(man.Skipped[i].Path)
	}
	contents := map[string][]byte{}
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badArchive("the tar cannot be read")
		}
		p := "/" + hd.Name
		i, ok := listed[p]
		if !ok || hd.Typeflag != tar.TypeReg {
			return nil, badArchive("an entry is not in the manifest or not a regular file")
		}
		if _, dup := contents[p]; dup {
			return nil, badArchive("an entry appears twice")
		}
		b, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil || int64(len(b)) != man.Files[i].Size {
			return nil, badArchive("a file does not have the size the manifest says")
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != man.Files[i].SHA256 {
			return nil, badArchive("a file does not match its SHA-256")
		}
		contents[p] = b
	}
	if len(contents) != len(man.Files) {
		return nil, badArchive("a listed file is missing")
	}
	return &FilesArchive{Manifest: man, contents: contents}, nil
}

func badArchive(why string) error { return fmt.Errorf("%w: %s", ErrBadArchive, why) }

// Read returns the content of a stored file by device path ("/lib/x.be").
func (a *FilesArchive) Read(devicePath string) ([]byte, error) {
	b, ok := a.contents[devicePath]
	if !ok {
		return nil, fmt.Errorf("backup: %q is not stored in the archive", devicePath)
	}
	return append([]byte(nil), b...), nil
}

// RunFiles reports the files part of the run id belongs to: the archive row
// and manifest, or why the run has none. Any file of the run (dump, snapshot,
// archive) names it.
func (m *Manager) RunFiles(ctx context.Context, id model.BackupID) (RunFiles, error) {
	row, files, err := m.runFilesRow(ctx, id)
	if err != nil {
		return RunFiles{}, err
	}
	if row == nil {
		rf := RunFiles{}
		for _, b := range files {
			if b.Kind == model.BackupConfigDump {
				rf.Note = FilesNoteOf(b.Note)
			}
		}
		return rf, nil
	}
	a, err := m.parseRow(ctx, *row)
	if err != nil {
		return RunFiles{}, err
	}
	return RunFiles{Archive: &a.Backup, Manifest: &a.Manifest}, nil
}

// OpenFilesArchive returns the verified files archive of the run id belongs
// to (ErrNoFilesArchive if it has none; ErrIntegrity if the file changed on
// disk; ErrBadArchive if it does not parse). The contents are secret: do not
// log them.
func (m *Manager) OpenFilesArchive(ctx context.Context, id model.BackupID) (*FilesArchive, error) {
	row, _, err := m.runFilesRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNoFilesArchive
	}
	return m.parseRow(ctx, *row)
}

func (m *Manager) parseRow(ctx context.Context, row model.Backup) (*FilesArchive, error) {
	data, b, err := m.Open(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	a, err := ParseFilesArchive(data)
	if err != nil {
		return nil, err
	}
	a.Backup = b
	return a, nil
}

// runFilesRow finds the files row among the files of id's run.
func (m *Manager) runFilesRow(ctx context.Context, id model.BackupID) (*model.Backup, []model.Backup, error) {
	b, err := m.o.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	all, err := m.ownerFiles(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	files := runsOf(all, b.TakenAt)
	for i := range files {
		if files[i].Kind == model.BackupFiles {
			return &files[i], files, nil
		}
	}
	return nil, files, nil
}
