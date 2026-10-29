package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// File layout (0.18): <DataDir>/backups/<dir>/<stamp>_<host>.<kind>, where
// dir and host come from the device's hostname, so a person browsing the data
// directory sees which device a file belongs to. Every row stores its own
// relative path and all reads use it, so files of an older layout stay
// readable wherever they lie; a hostname change only affects new backups.

const (
	stampLayout = "20060102T150405Z"
	maxNameLen  = 63
)

// SanitizeName reduces s to [A-Za-z0-9._-]: every other rune becomes "_",
// leading dots are dropped (no hidden or "." / ".." names), the result is at
// most 63 bytes. "" means nothing usable is left.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > maxNameLen {
		out = out[:maxNameLen]
	}
	return out
}

// baseName is the device's name for directory and files: current hostname,
// else its inventory name, else device-<id>.
func baseName(d model.Device) string {
	if n := SanitizeName(d.Net.Hostname); n != "" {
		return n
	}
	if n := SanitizeName(d.Name); n != "" {
		return n
	}
	return fmt.Sprintf("device-%d", int64(d.ID))
}

// fileName is the name of one file of a run; kind "dmp" is the plain dump.
func fileName(taken string, base string, kind model.BackupKind) string {
	if kind == model.BackupConfigDump {
		return taken + "_" + base + ".dmp"
	}
	return taken + "_" + base + "." + string(kind) + ".json"
}

// resolveDir picks the backup directory (relative to DataDir, slash form) and
// the file base name for d. A directory that already holds another device's
// (or a deleted device's) backups is not shared: <base>_<id> is used. The
// caller holds m.dirMu.
func (m *Manager) resolveDir(ctx context.Context, d model.Device) (dir, base string, err error) {
	base = baseName(d)
	owners, err := m.o.Store.BackupDirOwners(ctx, base)
	if err != nil {
		return "", "", fmt.Errorf("backup: directory owners: %w", err)
	}
	for _, o := range owners {
		if o != d.ID {
			return path.Join("backups", fmt.Sprintf("%s_%d", base, int64(d.ID))), base, nil
		}
	}
	return path.Join("backups", base), base, nil
}

// backupsRoot is <DataDir>/backups.
func (m *Manager) backupsRoot() string { return filepath.Join(m.o.DataDir, "backups") }

// removeEmptyDirs removes the (now possibly empty) directories that held the
// given relative file paths. os.Remove only succeeds on an empty directory, so
// nothing with content can be lost; every other error is ignored on purpose.
func (m *Manager) removeEmptyDirs(rels ...string) {
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	seen := map[string]bool{}
	for _, rel := range rels {
		full, ok := m.safePath(rel)
		if !ok {
			continue
		}
		d := filepath.Dir(full)
		if seen[d] || filepath.Dir(d) != m.backupsRoot() {
			continue
		}
		seen[d] = true
		_ = os.Remove(d) // fails with ENOTEMPTY while files are left: that is the point
	}
}

// MigrationResult summarises MigrateLayout.
type MigrationResult struct {
	Moved    int // files renamed and re-indexed
	Repaired int // a crash had left the file moved: only the row was updated
	Current  int // already in the new layout, or not a 0.17 path
	Left     int // not touched (mismatch, missing, ambiguous); see warnings
}

var legacyPath = regexp.MustCompile(`^backups/[0-9]+/(\d{8}T\d{6}Z)-(dmp|snapshot|decoded)\.(dmp|json)$`)

// MigrateLayout moves the 0.17 files backups/<device-id>/<stamp>-<kind>.<ext>
// to the hostname layout. It is idempotent and crash-safe and never loses a
// backup:
//
//   - a file moves only after its SHA-256 equals the row's; os.Rename keeps the
//     bytes on one filesystem, then the row path is changed compare-and-set;
//   - a crash between rename and update is repaired on the next start (old file
//     gone, new file matches the row);
//   - anything unclear (checksum mismatch, both or neither file present, target
//     used by another row, rename error) is left exactly as it is and counted;
//   - nothing is ever deleted except empty numeric directories (os.Remove).
//
// Orphan rows (device deleted) are not touched.
func (m *Manager) MigrateLayout(ctx context.Context) (MigrationResult, error) {
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	var res MigrationResult
	rows, err := m.o.Store.ListAllBackups(ctx)
	if err != nil {
		return res, fmt.Errorf("backup: list for layout migration: %w", err)
	}
	used := make(map[string]model.BackupID, len(rows))
	for _, b := range rows {
		used[strings.ToLower(b.Path)] = b.ID
	}
	type target struct{ dir, base string }
	targets := map[model.DeviceID]*target{}
	warn := func(b model.Backup, why string, err error) {
		res.Left++
		m.o.Log.Warn("backup layout: left in place", "component", "backup", "backup", b.ID, "path", b.Path, "why", why, "err", err)
	}
	for _, b := range rows {
		mt := legacyPath.FindStringSubmatch(b.Path)
		if mt == nil || b.DeviceID == 0 {
			res.Current++
			continue
		}
		stamp, kind, ext := mt[1], model.BackupKind(mt[2]), mt[3]
		if (kind == model.BackupConfigDump) != (ext == "dmp") {
			warn(b, "kind and extension disagree", nil)
			continue
		}
		t := targets[b.DeviceID]
		if t == nil {
			dev, err := m.o.Devices.GetDevice(ctx, b.DeviceID)
			if err != nil {
				warn(b, "device not readable", err)
				continue
			}
			dir, base, err := m.resolveDir(ctx, dev)
			if err != nil {
				warn(b, "directory not resolvable", err)
				continue
			}
			t = &target{dir, base}
			targets[b.DeviceID] = t
		}
		newRel := path.Join(t.dir, fileName(stamp, t.base, kind))
		if other, taken := used[strings.ToLower(newRel)]; taken && other != b.ID {
			warn(b, "target path belongs to another backup", nil)
			continue
		}
		oldFull, ok1 := m.safePath(b.Path)
		newFull, ok2 := m.safePath(newRel)
		if !ok1 || !ok2 {
			warn(b, "unsafe path", nil)
			continue
		}
		switch m.migrateOne(ctx, b, newRel, oldFull, newFull) {
		case migMoved:
			res.Moved++
			used[strings.ToLower(newRel)] = b.ID
		case migRepaired:
			res.Repaired++
			used[strings.ToLower(newRel)] = b.ID
		case migLeft:
			res.Left++
		}
	}
	m.removeEmptyNumericDirs()
	m.o.Log.Info("backup layout", "component", "backup", "moved", res.Moved, "repaired", res.Repaired, "current", res.Current, "left", res.Left)
	return res, nil
}

type migOutcome int

const (
	migMoved migOutcome = iota
	migRepaired
	migLeft
)

func (m *Manager) migrateOne(ctx context.Context, b model.Backup, newRel, oldFull, newFull string) migOutcome {
	left := func(why string, err error) migOutcome {
		m.o.Log.Warn("backup layout: left in place", "component", "backup", "backup", b.ID, "path", b.Path, "why", why, "err", err)
		return migLeft
	}
	newSum, newErr := fileSHA(newFull)
	oldSum, oldErr := fileSHA(oldFull)
	switch {
	case newErr == nil:
		// The target exists. Only a crash between rename and row update looks
		// like this: the old file is gone and the target is the row's file.
		if newSum != b.SHA256 {
			return left("target exists with another checksum", nil)
		}
		if oldErr == nil {
			return left("old and new file both exist", nil)
		}
		if !errors.Is(oldErr, os.ErrNotExist) {
			return left("old file unreadable", oldErr)
		}
		if !m.setPath(ctx, b, newRel) {
			return migLeft
		}
		return migRepaired
	case !errors.Is(newErr, os.ErrNotExist):
		return left("target unreadable", newErr)
	}
	if oldErr != nil {
		return left("old file missing or unreadable", oldErr)
	}
	if oldSum != b.SHA256 {
		return left("old file does not match its checksum", nil)
	}
	if err := os.MkdirAll(filepath.Dir(newFull), 0o700); err != nil {
		return left("create directory", err)
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return left("rename", err)
	}
	if !m.setPath(ctx, b, newRel) {
		return migLeft // file is at newRel, row says old: the next start repairs it
	}
	return migMoved
}

// setPath updates the row compare-and-set and logs a failure.
func (m *Manager) setPath(ctx context.Context, b model.Backup, newRel string) bool {
	ok, err := m.o.Store.UpdateBackupPath(ctx, b.ID, b.Path, newRel)
	if err != nil || !ok {
		m.o.Log.Warn("backup layout: row not updated", "component", "backup", "backup", b.ID, "from", b.Path, "to", newRel, "changed", !ok, "err", err)
		return false
	}
	return true
}

// removeEmptyNumericDirs removes empty backups/<number>/ directories left by
// the migration. os.Remove refuses a directory with content.
func (m *Manager) removeEmptyNumericDirs() {
	ents, err := os.ReadDir(m.backupsRoot())
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && isDigits(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		_ = os.Remove(filepath.Join(m.backupsRoot(), n)) // ENOTEMPTY: still holds files, keep
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// fileSHA streams the file through SHA-256.
func fileSHA(full string) (string, error) {
	f, err := os.Open(full) //nolint:gosec // full comes from safePath
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
