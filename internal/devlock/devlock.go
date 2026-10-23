// Package devlock is the per-device operation lock. A restore, an OTA job, a
// settings write or a console command each change the device and may reboot
// it; two of them at the same moment corrupt the result of both. Every such
// writer takes the device's lock for the duration of its operation and gives
// up (never queues) when somebody else holds it.
package devlock

import (
	"errors"
	"sync"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Locks is a set of per-device locks. The zero value is not usable; use New.
type Locks struct {
	mu   sync.Mutex
	held map[model.DeviceID]string
}

// New returns an empty lock set.
func New() *Locks { return &Locks{held: map[model.DeviceID]string{}} }

// TryAcquire takes the lock of a device for owner (a short word such as
// "restore", shown to the user who is refused). It returns the release
// function, or ok=false and the current holder. Release is idempotent.
func (l *Locks) TryAcquire(id model.DeviceID, owner string) (release func(), holder string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, busy := l.held[id]; busy {
		return nil, h, false
	}
	l.held[id] = owner
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.held, id)
			l.mu.Unlock()
		})
	}, "", true
}

// ErrBusy is matched (errors.Is) by every BusyError.
var ErrBusy = errors.New("another operation on this device is running")

// BusyError says which operation holds the device.
type BusyError struct{ Holder string }

func (e BusyError) Error() string { return ErrBusy.Error() + " (" + e.Holder + ")" }

// Is makes errors.Is(err, ErrBusy) true.
func (e BusyError) Is(target error) bool { return target == ErrBusy }
