package firmware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stepClock hands every After(d) call to the test, which then decides when the
// wait ends. No real sleeping, and the sequence of waits is observable.
type stepClock struct {
	*fakeClock
	waits chan time.Duration
	fire  chan time.Time
}

func newStepClock() *stepClock {
	return &stepClock{fakeClock: newFakeClock(), waits: make(chan time.Duration, 1), fire: make(chan time.Time)}
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.waits <- d
	return c.fire
}

// nextWait returns the duration of the next wait and ends it.
func (c *stepClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.waits:
		c.fire <- c.Now()
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("refresher did not wait")
		return 0
	}
}

type scriptedRefresher struct {
	mu      sync.Mutex
	results []error // consumed in order; the last one repeats
	calls   int
	called  chan struct{}
}

func (s *scriptedRefresher) Refresh(context.Context) error {
	s.mu.Lock()
	i := s.calls
	s.calls++
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	err := s.results[i]
	s.mu.Unlock()
	s.called <- struct{}{}
	return err
}

func runRefresher(t *testing.T, r Refresher, o RefreshOptions) (cancel func()) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunRefresher(ctx, r, o); close(done) }()
	return func() {
		cancelCtx()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunRefresher did not return after cancel")
		}
	}
}

// The whole point of H-2: nobody has to click "Refresh index" after a start.
func TestRunRefresherRefreshesImmediately(t *testing.T) {
	r := &scriptedRefresher{results: []error{nil}, called: make(chan struct{}, 10)}
	clk := newStepClock()
	stop := runRefresher(t, r, RefreshOptions{Clock: clk, Interval: time.Hour, Logger: quietLog()})
	defer stop()
	select {
	case <-r.called:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh without a click")
	}
}

// A flaky network at boot must not leave the index empty forever, and must not
// hammer GitHub (rate limits): 30 s, 1 min, 2 min, ... capped at 30 min.
func TestRunRefresherBacksOffAndCaps(t *testing.T) {
	fail := errors.New("upstream down")
	r := &scriptedRefresher{results: []error{fail}, called: make(chan struct{}, 100)}
	clk := newStepClock()
	stop := runRefresher(t, r, RefreshOptions{Clock: clk, Interval: time.Hour, Logger: quietLog()})
	defer stop()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := clk.nextWait(t); got != w {
			t.Fatalf("wait %d = %v, want %v", i, got, w)
		}
	}
}

// After the first success the periodic interval applies; a later failure keeps
// retrying with backoff again and recovers to the interval on success.
func TestRunRefresherPeriodicAndRecovery(t *testing.T) {
	fail := errors.New("down")
	r := &scriptedRefresher{results: []error{fail, nil, fail, nil}, called: make(chan struct{}, 100)}
	clk := newStepClock()
	stop := runRefresher(t, r, RefreshOptions{Clock: clk, Interval: 6 * time.Hour, Logger: quietLog()})
	defer stop()
	for i, w := range []time.Duration{30 * time.Second, 6 * time.Hour, 30 * time.Second, 6 * time.Hour} {
		if got := clk.nextWait(t); got != w {
			t.Fatalf("wait %d = %v, want %v", i, got, w)
		}
	}
}

// Jitter spreads restarts of many instances; it must stay within +-20 %.
func TestRunRefresherJitterIsBounded(t *testing.T) {
	for _, tc := range []struct {
		rnd      float64
		min, max time.Duration
	}{{0, 24 * time.Second, 24 * time.Second}, {0.5, 30 * time.Second, 30 * time.Second}, {0.999999, 35 * time.Second, 36 * time.Second}} {
		r := &scriptedRefresher{results: []error{errors.New("x")}, called: make(chan struct{}, 100)}
		clk := newStepClock()
		rnd := tc.rnd
		stop := runRefresher(t, r, RefreshOptions{Clock: clk, Interval: time.Hour, Jitter: 0.2, Rand: func() float64 { return rnd }, Logger: quietLog()})
		got := clk.nextWait(t)
		stop()
		if got < tc.min || got > tc.max {
			t.Errorf("rand %v: wait %v not in [%v, %v]", tc.rnd, got, tc.min, tc.max)
		}
	}
}

// Interval 0 means "only the start-up refresh": after the first success the
// loop ends instead of busy-looping.
func TestRunRefresherZeroIntervalStopsAfterSuccess(t *testing.T) {
	r := &scriptedRefresher{results: []error{nil}, called: make(chan struct{}, 10)}
	done := make(chan struct{})
	go func() {
		RunRefresher(context.Background(), r, RefreshOptions{Clock: newStepClock(), Interval: 0, Logger: quietLog()})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRefresher did not end with Interval 0")
	}
}

// What the firmware page and the age gauge are fed from.
func TestIndexStatusTracksSuccessFailureAndKeepsOldIndex(t *testing.T) {
	u := newUpstream(t)
	clk := newFakeClock()
	c, _, _ := newTestCache(t, u, func(o *Options) { o.Clock = clk })
	sr := c.(StatusReporter)
	if s := sr.IndexStatus(); s.Loaded || !s.LastSuccess.IsZero() || s.LastError != "" {
		t.Fatalf("fresh status = %+v", s)
	}
	ctx := context.Background()
	u.setDown(true)
	if err := c.(Refresher).Refresh(ctx); err == nil {
		t.Fatal("expected failure")
	}
	if s := sr.IndexStatus(); s.Loaded || s.LastError == "" || s.LastAttempt != clk.Now() {
		t.Fatalf("after failure = %+v", s)
	}
	u.setDown(false)
	if err := c.(Refresher).Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	ok := clk.Now()
	if s := sr.IndexStatus(); !s.Loaded || s.LastSuccess != ok || s.LastError != "" {
		t.Fatalf("after success = %+v", s)
	}
	clk.Advance(time.Hour)
	u.setDown(true)
	if err := c.(Refresher).Refresh(ctx); err == nil {
		t.Fatal("expected failure")
	}
	// Why: a failed refresh must not wipe a good index (offline upstream, OTA still plans).
	s := sr.IndexStatus()
	if !s.Loaded || s.LastSuccess != ok || s.LastError == "" {
		t.Fatalf("after later failure = %+v", s)
	}
	rels, err := c.Releases(ctx)
	if err != nil || len(rels) == 0 {
		t.Fatalf("old index lost: %v %v", rels, err)
	}
}
