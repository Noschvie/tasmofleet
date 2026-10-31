// Package snapshot makes consistent copies of TasmoFleet's own database: a download for an admin
// (optionally with secret.key in a .tar), and scheduled copies in <DATA_DIR>/snapshots that are
// pruned to a count. A copy is made with SQLite's VACUUM INTO, which is consistent while the
// application keeps writing (WAL). Device backups (.dmp), the firmware cache and the key are not
// part of the database file.
//
// Stored snapshots are addressed by file name only. The name never reaches a path directly: a
// name is looked up among the entries of the snapshots directory, so nothing a client sends can
// point outside it.
package snapshot

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/secretbox"
)

// DirName is the folder of the stored snapshots inside the data directory.
const DirName = "snapshots"

// nameRe is the only file name a stored snapshot has: tasmofleet-YYYYMMDD-HHMM.db.
var nameRe = regexp.MustCompile(`^tasmofleet-[0-9]{8}-[0-9]{4}\.db$`)

// ErrNotFound is returned for a name that is no stored snapshot (malformed, or not there).
var ErrNotFound = errors.New("snapshot: not found")

// Source writes a consistent copy of the live database to a file that does not exist yet.
type Source interface {
	VacuumInto(ctx context.Context, dst string) error
}

// Info describes a stored snapshot.
type Info struct {
	Name   string
	Size   int64
	Taken  time.Time // the file's modification time
	Schema int       // the database's schema version (PRAGMA user_version); 0 = unreadable
}

// Status is what the settings card shows about the scheduled snapshots.
type Status struct {
	Attempted time.Time // the last scheduled attempt since this process started; zero = none yet
	Failed    bool      // that attempt failed
	File      string    // the file it tried to write
	Reason    string    // why it failed
	LastGood  time.Time // the newest stored snapshot (also found after a restart); zero = none
}

// Manager makes and keeps snapshots.
type Manager struct {
	src     Source
	dataDir string
	dir     string
	now     func() time.Time

	mu   sync.Mutex // one copy or prune at a time
	last Status
}

// New returns a Manager for the data directory.
func New(src Source, dataDir string) *Manager {
	return &Manager{src: src, dataDir: dataDir, dir: filepath.Join(dataDir, DirName), now: time.Now}
}

// Dir is the folder of the stored snapshots.
func (m *Manager) Dir() string { return m.dir }

// ValidName says whether name has the form of a stored snapshot's file name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// nameFor is the file name of a snapshot taken at t.
func nameFor(t time.Time) string { return "tasmofleet-" + t.Format("20060102-1504") + ".db" }

// CleanTemp removes scratch directories a crash left behind. Call it once at start.
func (m *Manager) CleanTemp() {
	for _, dir := range []string{m.dataDir, m.dir} {
		matches, err := filepath.Glob(filepath.Join(dir, ".snapshot-*"))
		if err != nil {
			continue // only a malformed pattern; ours is constant
		}
		for _, p := range matches {
			os.RemoveAll(p) //nolint:errcheck // scratch directory, best effort
		}
	}
}

// Run takes a scheduled snapshot into the snapshots directory, prunes to keep and records the
// outcome for Status. A failed copy leaves everything that is stored untouched.
func (m *Manager) Run(ctx context.Context, keep int) (Info, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := nameFor(m.now())
	info, err := m.take(ctx, name)
	st := Status{Attempted: m.now(), File: name}
	if err != nil {
		st.Failed, st.Reason = true, reasonOf(err)
		m.last = st
		return Info{}, 0, err
	}
	m.last = st
	pruned, perr := m.prune(keep)
	return info, pruned, perr
}

// take writes the copy next to its final name and renames it into place, so a half-written
// file never carries a snapshot's name.
func (m *Manager) take(ctx context.Context, name string) (Info, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return Info{}, err
	}
	tmp, err := os.MkdirTemp(m.dir, ".snapshot-")
	if err != nil {
		return Info{}, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // scratch directory, best effort
	part := filepath.Join(tmp, "copy.db")
	if err := m.src.VacuumInto(ctx, part); err != nil {
		return Info{}, err
	}
	if err := os.Chmod(part, 0o600); err != nil {
		return Info{}, err
	}
	dst := filepath.Join(m.dir, name)
	if err := os.Rename(part, dst); err != nil {
		return Info{}, err
	}
	return statInfo(dst, name)
}

// Prune deletes all but the newest keep snapshots and returns how many went.
func (m *Manager) Prune(keep int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prune(keep)
}

func (m *Manager) prune(keep int) (int, error) {
	list, err := m.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, in := range list[min(len(list), max(keep, 1)):] {
		if err := os.Remove(filepath.Join(m.dir, in.Name)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Due says whether a scheduled snapshot is due: none is stored, or the newest one is as old as
// the interval (less a minute, so the timer that fires one interval after the last run counts).
func (m *Manager) Due(every time.Duration) bool {
	list, err := m.List()
	if err != nil || len(list) == 0 {
		return true
	}
	return m.now().Sub(list[0].Taken) >= every-time.Minute
}

// Status is the scheduled snapshots' last attempt and the newest stored snapshot.
func (m *Manager) Status() Status {
	m.mu.Lock()
	st := m.last
	m.mu.Unlock()
	if list, err := m.List(); err == nil && len(list) > 0 {
		st.LastGood = list[0].Taken
	}
	return st
}

// List returns the stored snapshots, newest first. Only regular files with a snapshot's name
// count; a symbolic link or anything else in the folder is ignored. A missing folder is an
// empty list.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidName(e.Name()) {
			continue
		}
		in, err := statInfo(filepath.Join(m.dir, e.Name()), e.Name())
		if err != nil {
			continue // removed while listing
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b Info) int { return -compareName(a.Name, b.Name) })
	return out, nil
}

func compareName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// find returns the listed entry called name. The returned path is built from the directory's own
// entry, never from name.
func (m *Manager) find(name string) (string, error) {
	if !ValidName(name) {
		return "", ErrNotFound
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	for _, e := range entries {
		if e.Name() == name && e.Type().IsRegular() {
			return filepath.Join(m.dir, e.Name()), nil
		}
	}
	return "", ErrNotFound
}

// Open opens a stored snapshot for download. The caller closes the file.
func (m *Manager) Open(name string) (*os.File, Info, error) {
	p, err := m.find(name)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(p) //nolint:gosec // p comes from the directory listing of the snapshots folder
	if err != nil {
		return nil, Info{}, ErrNotFound
	}
	in, err := statInfo(p, filepath.Base(p))
	if err != nil {
		f.Close() //nolint:errcheck // read-only handle, the stat error is the one to report
		return nil, Info{}, err
	}
	return f, in, nil
}

// Delete removes a stored snapshot.
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.find(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Export is a snapshot made for one download. It lives in a scratch directory of the data
// directory until Close.
type Export struct {
	File        *os.File
	Name        string // the download's file name
	ContentType string
	Size        int64
	Taken       time.Time
	dir         string
}

// Close removes the scratch directory with the copy.
func (e *Export) Close() error {
	e.File.Close() //nolint:errcheck // read-only handle
	return os.RemoveAll(e.dir)
}

// Export makes a consistent copy for a download: the database file, or with withKey a .tar of
// the database and secret.key (named as they are in the data directory, so unpacking into it
// restores both). The copy is not stored.
func (m *Manager) Export(ctx context.Context, withKey bool) (*Export, error) {
	dir, err := os.MkdirTemp(m.dataDir, ".snapshot-")
	if err != nil {
		return nil, err
	}
	ex, err := m.export(ctx, dir, withKey)
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck // scratch directory, best effort
		return nil, err
	}
	return ex, nil
}

func (m *Manager) export(ctx context.Context, dir string, withKey bool) (*Export, error) {
	taken := m.now()
	base := nameFor(taken)
	db := filepath.Join(dir, "tasmofleet.db")
	if err := m.src.VacuumInto(ctx, db); err != nil {
		return nil, err
	}
	out, name, ctype := db, base, "application/vnd.sqlite3"
	if withKey {
		out, name, ctype = filepath.Join(dir, "export.tar"), base[:len(base)-len(".db")]+".tar", "application/x-tar"
		if err := writeTar(out, db, filepath.Join(m.dataDir, secretbox.KeyFile), taken); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(out) //nolint:gosec // a file we just wrote in our scratch directory
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // read-only handle, the stat error is the one to report
		return nil, err
	}
	return &Export{File: f, Name: name, ContentType: ctype, Size: st.Size(), Taken: taken, dir: dir}, nil
}

// writeTar writes tasmofleet.db and secret.key (mode 0600) into a new tar at dst.
func writeTar(dst, db, key string, mod time.Time) (err error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // our scratch directory
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(f)
	for _, e := range []struct{ path, name string }{{db, "tasmofleet.db"}, {key, secretbox.KeyFile}} {
		if err := addFile(tw, e.path, e.name, mod); err != nil {
			return err
		}
	}
	return tw.Close()
}

func addFile(tw *tar.Writer, path, name string, mod time.Time) error {
	src, err := os.Open(path) //nolint:gosec // the scratch copy and <data dir>/secret.key
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only handle
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: mod, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, src)
	return err
}

func statInfo(path, name string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	return Info{Name: name, Size: st.Size(), Taken: st.ModTime(), Schema: schemaOf(path)}, nil
}

// schemaOf reads PRAGMA user_version from the SQLite file header (4 bytes at offset 60), so
// listing needs no database connection. 0 when the header cannot be read.
func schemaOf(path string) int {
	f, err := os.Open(path) //nolint:gosec // a listed snapshot
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var hdr [100]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[:15]) != "SQLite format 3" {
		return 0
	}
	return int(binary.BigEndian.Uint32(hdr[60:64]))
}

// reasonOf is the short cause of a failed copy: the operating system's message when there is one
// ("no space left on device"), else the error text, clipped.
func reasonOf(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
