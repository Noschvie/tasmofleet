package settings

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// The read-back after a write (#947): every value TasmoFleet wrote is read from the device again and compared with
// what was written, so a page can say "verified" or show the device's own answer. The Settings tab (Manager.Apply)
// and Make compliant (policy.Fixer.Verify) both run ReadBack; the comparison is Matches, which the policy's live
// check uses as well, so a value never counts as equal in one place and as different in the other.

// CheckState is what the read-back found for one written value.
type CheckState string

const (
	// CheckVerified: the device reports the written value.
	CheckVerified CheckState = "verified"
	// CheckDiffers: the device reports another value (Check.Have, unless the caller hides it).
	CheckDiffers CheckState = "differs"
	// CheckNotReported: the device answered the read but did not report this value.
	CheckNotReported CheckState = "not_reported"
	// CheckNotReadBack: the value was not read: a secret (never queried), a command that cannot be read, or a
	// read that failed or that another device may have answered (Check.Why says which).
	CheckNotReadBack CheckState = "not_read_back"
)

// Check is the read-back of one written value. The JSON form is what a policy job keeps with its done event.
type Check struct {
	Key   string     `json:"key"`
	State CheckState `json:"state"`
	// Have is the device's value when it differs. Empty when the caller shows the written value masked (Hidden):
	// the device's answer would show what the mask hides.
	Have   string `json:"have,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
	// Why says why a value was not read back (CheckNotReadBack); a short sentence, never device output.
	Why string `json:"why,omitempty"`
}

// Want is one written value to read back. Hidden says the caller shows the value masked (a policy rule whose value
// cmdpolicy.Redacted hides); the device's answer is then not shown either.
type Want struct {
	Key, Value string
	Hidden     bool
}

// ReadFunc reads keys from one device, one query at a time (never Status 0), and returns values only when the
// device that answered is the stored one (Manager.ReadVerified).
type ReadFunc func(ctx context.Context, keys []string) (map[string]string, error)

// ReadBackOptions tune the loop of ReadBack.
type ReadBackOptions struct {
	// Wait pauses between reads; nil waits for real.
	Wait func(ctx context.Context, d time.Duration) error
	// Attempts is how often a read is tried while the device does not answer (default 6).
	Attempts int
}

// Why texts of CheckNotReadBack.
const (
	WhySecret      = "a secret is never read back"
	WhyNotReadable = "this setting cannot be read from the device"
)

// restartWait is how long the read-back waits before its first read when a written value restarts the device.
// Tasmota restarts once its backlog is empty, on the once-a-second tick: restart_flag 2 saves every setting to flash
// and the next tick restarts (support_tasmota.ino 16.0.0, Every250mSeconds, lines 1569-1641), so the restart comes
// one to two seconds after the command. Five seconds lets the reads meet the device after it, where a read during the
// reboot would only cost an attempt. A first read that still meets the device before its restart counts as it is:
// the restart saves the values in RAM, so what it reports then is what it reports after the restart.
const restartWait = 5 * time.Second

// unreachableWait is the pause before the next read while the device does not answer (it may still be restarting).
const unreachableWait = 5 * time.Second

// settleReads and settleWait bound the wait for a Backlog to run. Over HTTP the reply to a Backlog comes before the
// device has run it (#559), so a value that differs is read again a few times before it counts. Tasmota has no cap on
// the queue (only 12.x had 30 entries), a plan can hold up to 100 lines, and the device runs one per SetOption34
// delay (200 ms by default, 255 ms at most; backlogLineWait) once it is left alone: each read pushes the queue back,
// so the first wait covers every line still wrong. A value the device really refuses costs about settleReads
// seconds plus a quarter second per differing value, no more; the Settings tab, which sends one command per field,
// pays the same.
const (
	settleReads     = 6
	settleWait      = time.Second
	backlogLineWait = 250 * time.Millisecond
)

// ReadsBack reports whether ReadBack queries key: never a secret (cmdpolicy.SecretName, #809), never a setting
// that cannot be read (Queryable). A writer that relies on the read-back to learn whether a write arrived asks this.
func ReadsBack(key string) bool { return !cmdpolicy.SecretName(key) && Queryable(key) }

// ReadBack reads the written values back and compares each with Matches. Secrets are never queried
// (cmdpolicy.SecretName, the one rule of #809). When a written value restarts the device, the first read waits
// restartWait. A read that fails is tried again while the device does not answer (Attempts, unreachableWait); a
// value that differs is read again until it settles (settleReads); a stop while it settles keeps the last read.
// The error is the failed read (or the stop before any read answered), and values is nil. When the first read
// failed, nothing was read back and the checks of the values to read have no state; when a later read (one while a
// value settles) failed, the checks keep what the last read that answered found, from the confirmed device. values
// are what the last read returned; read returns values only from a device it confirmed.
func ReadBack(ctx context.Context, read ReadFunc, wants []Want, o ReadBackOptions) (checks []Check, values map[string]string, err error) {
	checks = make([]Check, len(wants))
	skip := make([]bool, len(wants)) // never read: a secret or a setting that cannot be read
	var keys []string
	restarts := false
	for i, w := range wants {
		checks[i] = Check{Key: w.Key, Hidden: w.Hidden}
		restarts = restarts || RestartsDevice(w.Key, w.Value) // a secret that is not read restarts the device too
		if !ReadsBack(w.Key) {
			checks[i].State, checks[i].Why, skip[i] = CheckNotReadBack, WhyNotReadable, true
			if cmdpolicy.SecretName(w.Key) {
				checks[i].Why = WhySecret
			}
			continue
		}
		if !slices.Contains(keys, w.Key) {
			keys = append(keys, w.Key)
		}
	}
	if len(keys) == 0 {
		return checks, nil, nil
	}
	attempts := o.Attempts
	if attempts < 1 {
		attempts = 6
	}
	if restarts {
		if err := o.wait(ctx, restartWait); err != nil {
			return checks, nil, err
		}
	}
	var got map[string]string
settling:
	for settle := 0; ; settle++ {
		for i := 0; ; i++ {
			got, err = read(ctx, keys)
			if err == nil {
				break
			}
			if i+1 >= attempts {
				return checks, nil, err
			}
			if werr := o.wait(ctx, unreachableWait); werr != nil {
				return checks, nil, werr
			}
		}
		stale := 0 // values that differ (not absent): the device's queue may not have run them yet
		for i, w := range wants {
			if skip[i] {
				continue
			}
			checks[i].Have = ""
			have, ok := got[w.Key]
			switch {
			case !ok:
				checks[i].State = CheckNotReported
			case Matches(w.Key, have, w.Value):
				checks[i].State = CheckVerified
			default:
				checks[i].State = CheckDiffers
				if !w.Hidden {
					checks[i].Have = have
				}
				stale++
			}
		}
		if stale == 0 || settle >= settleReads {
			break
		}
		pause := settleWait
		if settle == 0 {
			pause = max(pause, time.Duration(stale+1)*backlogLineWait)
		}
		if !o.waited(ctx, pause) {
			break settling // stopped while it settles: the last read stands, it came from the confirmed device
		}
	}
	return checks, got, nil
}

// waited reports whether the pause ran to its end (not stopped by ctx).
func (o ReadBackOptions) waited(ctx context.Context, d time.Duration) bool {
	return o.wait(ctx, d) == nil
}

func (o ReadBackOptions) wait(ctx context.Context, d time.Duration) error {
	if o.Wait != nil {
		return o.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReadVerified reads keys like ReadKeys and then asks Options.Verify whether the device that answered is the stored
// one, over the transports the reads used (#810). Values come back only when it is: on any error they are nil, so
// a value of another device (a DHCP change, a restart onto another address) never counts as read back or reaches
// a cache. Without Options.Verify it is ReadKeys.
func (m *Manager) ReadVerified(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	for _, k := range keys {
		if !Queryable(k) {
			return nil, fmt.Errorf("%w: %q", ErrNotQueryable, k)
		}
		if f, ok := Lookup(k); ok && f.Secret {
			return nil, fmt.Errorf("%w: %q is write-only", ErrNotQueryable, k)
		}
	}
	return m.readVerified(ctx, id, keys)
}

func (m *Manager) readVerified(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	rctx, read := transport.WithTrace(ctx) // the check vouches for the transports these reads used (#810)
	vals, err := m.readKeys(rctx, id, keys)
	if err != nil {
		return nil, err
	}
	if m.o.Verify == nil {
		return vals, nil
	}
	d, err := m.o.Devices.GetDevice(ctx, id)
	if err == nil {
		vctx, vcancel := context.WithTimeout(ctx, m.o.ReadTimeout)
		err = m.o.Verify(vctx, d, read)
		vcancel()
	}
	if err != nil {
		return nil, &IdentityError{Err: err}
	}
	return vals, nil
}

// IdentityError says the values were read but the device that answered could not be confirmed as the stored one.
type IdentityError struct{ Err error }

func (e *IdentityError) Error() string { return e.Err.Error() }
func (e *IdentityError) Unwrap() error { return e.Err }

// ReadBack reads the written values back from the device (the package's ReadBack over ReadVerified).
func (m *Manager) ReadBack(ctx context.Context, id model.DeviceID, wants []Want) ([]Check, error) {
	checks, _, err := ReadBack(ctx, func(ctx context.Context, keys []string) (map[string]string, error) {
		return m.readVerified(ctx, id, keys)
	}, wants, ReadBackOptions{Wait: m.o.Wait})
	return checks, err
}

// notReadBack marks every value that was to be read but was not: the read failed or the device could not be
// confirmed. The values stay unknown; nothing is guessed.
func notReadBack(checks []Check, err error) {
	why := "could not be read back: " + readBackFailure(err)
	for i := range checks {
		if checks[i].State != "" {
			continue // a secret or an unreadable setting keeps its own reason
		}
		checks[i] = Check{Key: checks[i].Key, State: CheckNotReadBack, Hidden: checks[i].Hidden, Why: why}
	}
}

// readBackFailure is the short reason of a failed read-back, never device output.
func readBackFailure(err error) string {
	var ie *IdentityError
	switch {
	case errors.As(err, &ie) && errors.Is(err, ErrNoMAC):
		return "the device answers without a MAC, so it could not be confirmed as this device"
	case errors.As(err, &ie):
		return "the device that answered could not be confirmed as this device"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "the read was stopped"
	}
	return errorText(err)
}

// Matches reports whether a value the device reports (have) is the value that was written or a rule wants (want).
// It is the one comparison of written and reported values: the read-back and the policy's live check both use it.
// The firmware answers some values in a form of its own, and both sides are brought to that form first:
// whitespace around a value does not count, ON/OFF are 1/0, Latitude and Longitude are compared with four decimals
// (CanonicalWant), and Timezone is compared as the firmware reports it (timezoneForm).
func Matches(key, have, want string) bool { return comparable(key, have) == comparable(key, want) }

func comparable(key, v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToUpper(v) {
	case "ON":
		return "1"
	case "OFF":
		return "0"
	}
	if _, coord := isCoordinate(key); coord {
		if c, err := CanonicalWant(key, v); err == nil {
			return c
		}
		return v
	}
	if strings.EqualFold(key, "Timezone") {
		if c, ok := timezoneForm(v); ok {
			return c
		}
	}
	return v
}

var timezoneRe = regexp.MustCompile(`^[+-]?[0-9]{1,2}(:[0-9]{1,2})?$`)

// timezoneForm is a Timezone value as Tasmota reports it (support_command.ino 16.0.0, CmndTimezone, lines
// 2772-2797): 99, or "%+03d:%02d" of the hours and the minutes after the ':' (at most 59). A value written as 1
// reads back as "+01:00", -5 as "-05:00", and -00:00 as "+00:00". Hours of 15 and more set 99; hours below -13
// change nothing, so they keep their own form and never match a report. The firmware reads the hours with strtol of
// base 0 (support_command.ino 16.0.0, line 463), so a zero-padded 08 or 09 hour is octal and sets 0: such a write
// reads back as "+00:00" (or "+00:30") and shows as differing, which is what the device holds. This form is not
// modelled here on purpose: a match would call that wrong write verified.
func timezoneForm(v string) (string, bool) {
	if !timezoneRe.MatchString(v) {
		return "", false
	}
	hours, minutes, hasMinutes := strings.Cut(v, ":")
	h, err := strconv.Atoi(hours)
	if err != nil || h < -13 {
		return "", false
	}
	if h >= 15 {
		return "99", true
	}
	m := 0
	if hasMinutes {
		if m, err = strconv.Atoi(minutes); err != nil {
			return "", false
		}
		m = min(m, 59)
	}
	return fmt.Sprintf("%+03d:%02d", h, m), true
}
