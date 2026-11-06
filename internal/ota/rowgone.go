package ota

import (
	"errors"

	"github.com/hellwand/tasmofleet/internal/store"
)

// persistFailed logs a job transition (or event) that could not be stored; the caller stops the
// job either way. A vanished row (store.ErrNotFound) means the device was deleted under the job:
// the store refuses that while a job holds the device (#466), so it is only a job left over from a
// delete in an older version or by hand, and a WARN is all it deserves. Every other failure keeps
// msg as an ERROR: the job resumes from its stored state at the next start.
func (e *Engine) persistFailed(msg string, err error, args ...any) {
	args = append(args, "err", err)
	if errors.Is(err, store.ErrNotFound) {
		e.log.Warn("job row is gone, the device was deleted; the job stops here", append(args, "step", msg)...)
		return
	}
	e.log.Error(msg, args...)
}
