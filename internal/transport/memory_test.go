package transport

import (
	"testing"
	"time"
)

// Why (#818): routing forgets an HTTP failure after ttl (a device that failed once must not be avoided for ever), but
// the facts must not call an HTTP answer from before that failure fresh. So the time of the last failing call outlives
// ttl and ends only with an answer; the routing window and this time are two separate facts, not one that is cut short.
func TestLastFailureOutlivesTheRoutingWindowAndEndsWithAnAnswer(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	m := NewMemory(DefaultFailFor, func() time.Time { return now })
	if _, ok := m.LastFailure(1); ok {
		t.Fatal("a device that never failed has a last failure")
	}
	m.Fail(1, HTTPFailUnreachable)
	failedAt := now
	now = now.Add(DefaultFailFor + time.Minute)
	if _, _, ok := m.HTTPFailure(1); ok {
		t.Error("routing still avoids HTTP after the ttl")
	}
	if at, ok := m.LastFailure(1); !ok || !at.Equal(failedAt) {
		t.Errorf("last failure after the ttl = %v, %v; want %v", at, ok, failedAt)
	}
	now = now.Add(time.Hour)
	m.Fail(1, HTTPFailAuth) // a failure after the expiry starts a new series and moves the last time
	if at, _ := m.LastFailure(1); !at.Equal(now) {
		t.Errorf("last failure = %v after a new failure at %v", at, now)
	}
	m.Clear(1)
	if _, ok := m.LastFailure(1); ok {
		t.Error("an answered call leaves a last failure behind")
	}
}
