// Package clock abstracts time so that timeouts in the OTA engine, health
// tracker and session handling can be tested without sleeping.
package clock

import "time"

// Clock is the time source used by every package with timeouts.
type Clock interface {
	Now() time.Time
	// After behaves like time.After.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
