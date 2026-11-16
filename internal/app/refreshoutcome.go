package app

import (
	"errors"
	"sync"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/web"
)

// Why the last background refresh of a device's data failed (#818). The reader drops the error once it has scheduled
// the retry, so nothing else could see it; this is the record the device page reads, in memory only.
//
//   - starts: a background data read fails, or is refused by the identity check (another device answers at the address
//     or on the topic), while the process is running. A shutdown, a busy device (nothing was sent) and a failure of a
//     step after the Settings values were read (laterStepError: the values on the page are fresh) record nothing.
//   - ends: the next data read that works deletes it, as does a failure after the Settings step; the pass deletes the
//     record of a device that is no longer listed; a restart of TasmoFleet ends it (the data read is due at once then).
//   - the page shows it only while it is newer than the values it shows, so a person's own successful read hides it
//     without any coupling between the two.
//
// next follows the reader's own schedule (dataNext): the code that moves that time moves it here too, and a device that
// is held by a job (due again at the next visit) has none.
type refreshOutcome struct {
	at, next time.Time
	reason   settings.FailReason
}

type refreshOutcomes struct {
	mu sync.Mutex
	m  map[model.DeviceID]refreshOutcome
}

func newRefreshOutcomes() *refreshOutcomes {
	return &refreshOutcomes{m: map[model.DeviceID]refreshOutcome{}}
}

func (o *refreshOutcomes) fail(id model.DeviceID, at, next time.Time, reason settings.FailReason) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[id] = refreshOutcome{at: at, next: next, reason: reason}
}

func (o *refreshOutcomes) clear(id model.DeviceID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, id)
}

// moveNext follows a change of the data schedule of a device whose last refresh failed (zero: not known).
func (o *refreshOutcomes) moveNext(id model.DeviceID, next time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f, ok := o.m[id]; ok {
		f.next = next
		o.m[id] = f
	}
}

// keep forgets the devices that are not listed any more.
func (o *refreshOutcomes) keep(listed map[model.DeviceID]model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id := range o.m {
		if _, ok := listed[id]; !ok {
			delete(o.m, id)
		}
	}
}

// Failure is web.Deps.RefreshFailure.
func (o *refreshOutcomes) Failure(id model.DeviceID) (web.RefreshFailure, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f, ok := o.m[id]
	return web.RefreshFailure{At: f.at, Next: f.next, Reason: f.reason}, ok
}

// refreshReason is the class of a failed data read. The identity refusals are errors of this package, so they are
// mapped here; everything else is the one mapping of the settings package (settings.ClassifyRead), which the page and
// the log use too.
func refreshReason(err error) settings.FailReason {
	var on anotherOnTopic
	var ref *RefreshRefusedError
	if errors.Is(err, errAnotherDevice) || errors.As(err, &on) || errors.As(err, &ref) {
		return settings.FailOtherDevice
	}
	return settings.ClassifyRead(err)
}

// laterStepError is a failure of a step of the data refresh that runs after the Settings values were read (the policy
// values, the template reading). The refresh is retried, but the values the Settings tab shows are fresh, so the tab
// has no reason to give.
type laterStepError struct{ err error }

func (e laterStepError) Error() string { return e.err.Error() }
func (e laterStepError) Unwrap() error { return e.err }
