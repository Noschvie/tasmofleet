package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/importer"
)

// ImportOptions control ImportTasmoAdmin.
type ImportOptions struct {
	Dir    string
	DryRun bool
	// NoProbe skips the per-device probe (MAC, identity); the rows are
	// imported as they are.
	NoProbe bool
}

// ImportResult counts what an import did.
type ImportResult struct {
	Created, Updated, Failed, Unprobed int
}

// Importer builds the TasmoAdmin importer used by the CLI and the web upload.
func (s *Services) Importer() *importer.Importer {
	return importer.New(importer.Options{Inventory: s.Inventory, Prober: s.Prober, Audit: s.Audit})
}

// ImportTasmoAdmin reads devices.csv (and MyConfig.json, optionally the
// .device-password.key) from opts.Dir and imports it through package importer
// (spec 11.2), the same code the web upload uses. A device that cannot be
// probed is still imported, without MAC and identity. Progress and problems go
// to out; passwords are never printed.
func ImportTasmoAdmin(ctx context.Context, svc *Services, opts ImportOptions, out io.Writer) (ImportResult, error) {
	var res ImportResult
	out = sanitizingWriter{out}                               // names come from a CSV or from the devices themselves
	f, err := os.Open(filepath.Join(opts.Dir, "devices.csv")) //nolint:gosec // operator-supplied import directory
	if err != nil {
		return res, err
	}
	defer func() { _ = f.Close() }() // read-only file
	cfg, closeCfg := openOptional(filepath.Join(opts.Dir, "MyConfig.json"))
	defer closeCfg()
	key, closeKey := openOptional(filepath.Join(opts.Dir, ".device-password.key"))
	defer closeKey()

	imp := svc.Importer()
	plan, err := imp.Plan(ctx, f, cfg, key, !opts.NoProbe && !opts.DryRun)
	if err != nil {
		return res, err
	}
	for _, e := range plan.Errors {
		_, _ = fmt.Fprintf(out, "warning: %v\n", &e)
	}
	for _, w := range plan.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", w)
	}
	if plan.Hint != "" {
		_, _ = fmt.Fprintf(out, "hint: %s\n", plan.Hint)
	}
	if opts.DryRun {
		for _, r := range plan.Rows {
			_, _ = fmt.Fprintf(out, "would %s: %s (%s)%s\n", r.Action, r.Device.Name, r.Device.Address, reason(r.Reason))
		}
		return res, nil
	}
	rep, err := imp.Apply(ctx, "cli", plan)
	for _, l := range rep.Lines {
		_, _ = fmt.Fprintf(out, "%s: %s (%s)%s\n", l.Status, l.Name, l.Address, reason(l.Detail))
	}
	res = ImportResult{Created: rep.Created, Updated: rep.Updated, Failed: rep.Failed, Unprobed: rep.Unprobed}
	if err != nil {
		return res, err
	}
	_, _ = fmt.Fprintf(out, "done: %d created, %d updated, %d failed, %d not probed, %d skipped\n", rep.Created, rep.Updated, rep.Failed, rep.Unprobed, rep.Skipped)
	return res, nil
}

func reason(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// openOptional opens an optional input; a missing file is nil, not an error.
// The returned reader is nil-safe as an io.Reader only when the file exists.
func openOptional(path string) (io.Reader, func()) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied import directory
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// An unreadable optional file must not be silently ignored.
			return errReader{err}, func() {}
		}
		return nil, func() {}
	}
	return f, func() { _ = f.Close() } // read-only file
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// sanitizingWriter replaces control characters (except newline) so that
// device or CSV supplied text cannot drive the terminal (ANSI, OSC 52).
type sanitizingWriter struct{ w io.Writer }

func (s sanitizingWriter) Write(p []byte) (int, error) {
	b := bytes.Map(func(r rune) rune {
		if r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return '?'
	}, p)
	if _, err := s.w.Write(b); err != nil {
		return 0, err
	}
	return len(p), nil
}
