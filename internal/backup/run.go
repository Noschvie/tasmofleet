package backup

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Run says why a backup is taken and how it is stored. Callers attach it to
// the context (WithRun) so the Service contract stays unchanged; without one
// a backup is a manual one.
type Run struct {
	Trigger model.BackupTrigger
	Ref     string // TriggerUpdate: the target release
	Keep    bool   // store the run as kept ("Keep these backups" of a bulk backup)
	Label   string // free text stored with the dump ("Label" of a bulk backup)
}

type runKey struct{}

// WithRun returns ctx carrying r for the next Backup or BackupOf.
func WithRun(ctx context.Context, r Run) context.Context {
	return context.WithValue(ctx, runKey{}, r)
}

// maxLabel bounds a label; it is shown on one line in the backup list.
const maxLabel = 120

// RunFrom returns the Run a backup taken with ctx uses: the one attached by
// WithRun, a manual one otherwise, with the label cleaned.
func RunFrom(ctx context.Context) Run { return runOf(ctx) }

func runOf(ctx context.Context) Run {
	r, _ := ctx.Value(runKey{}).(Run) // absent: the zero Run, completed below
	if r.Trigger == "" {
		r.Trigger = model.TriggerManual
	}
	r.Label = CleanLabel(r.Label)
	return r
}

// CleanLabel makes a user label safe to store and show on one line: control
// characters become spaces, runs of space collapse, and it is cut to 120
// runes.
func CleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxLabel {
		s = strings.TrimSpace(string(r[:maxLabel]))
	}
	return s
}

// AttemptReason is the text stored for a failed backup. It is chosen from the
// error's kind, never copied from the error text: device errors can carry
// URLs, and a URL can carry the web password.
func AttemptReason(err error) string {
	switch {
	case errors.Is(err, tasmota.ErrAuth):
		return "HTTP 401: the device rejected the web password"
	case errors.Is(err, tasmota.ErrUnreachable):
		return "The device did not answer"
	case errors.Is(err, tasmota.ErrTimeout):
		return "The device took too long to answer"
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "The device address is outside the configured device networks"
	case errors.Is(err, tasmota.ErrBadResponse):
		return "The device sent an answer that could not be read"
	case errors.Is(err, errEmptyDump):
		return "The device returned an empty configuration"
	}
	return "The device did not hand over its configuration"
}
