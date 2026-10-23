package firmware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
)

// Item states of a prefetch run.
const (
	PrefetchPending  = "pending"
	PrefetchFetching = "fetching"
	PrefetchCached   = "cached" // downloaded and verified, or already in the cache
	PrefetchFailed   = "failed"
)

const (
	maxPrefetchRuns = 10
	// prefetchTimeout bounds one run. The run is detached from the request
	// that started it (the browser may leave), so it needs its own limit.
	prefetchTimeout = 30 * time.Minute
)

// PrefetchItem is one file of a run.
type PrefetchItem struct {
	File    string
	State   string
	Already bool // was cached before the run started
	Size    int64
	Err     string
}

// PrefetchRun is a snapshot of one prefetch.
type PrefetchRun struct {
	ID         string
	Release    string
	Actor      string
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	Items      []PrefetchItem
}

// Running reports whether the run still has work.
func (r PrefetchRun) Running() bool { return r.FinishedAt.IsZero() }

// Counts returns how many items are done (cached), failed and in total.
func (r PrefetchRun) Counts() (done, failed, total int) {
	for _, it := range r.Items {
		switch it.State {
		case PrefetchCached:
			done++
		case PrefetchFailed:
			failed++
		}
	}
	return done, failed, len(r.Items)
}

// Prefetcher downloads a list of files of one release into the cache, one at
// a time, through Cache.Ensure: the size limit, the cross-check and the
// image-header check apply exactly as for a download started by an OTA job.
// Only one run is active at a time; asking for another returns the active
// one instead of starting a parallel download.
type Prefetcher struct {
	cache Cache
	clk   clock.Clock

	mu     sync.Mutex
	runs   []*PrefetchRun // oldest first, at most maxPrefetchRuns
	active *PrefetchRun
	wg     sync.WaitGroup
}

// NewPrefetcher returns a Prefetcher over c; clk nil = real time.
func NewPrefetcher(c Cache, clk clock.Clock) *Prefetcher {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Prefetcher{cache: c, clk: clk}
}

// Start begins a run for the files of release in the background. The run
// outlives ctx (a request) but is bounded by its own timeout. If a run is
// active, that run is returned and started is false. files must not be empty.
func (p *Prefetcher) Start(ctx context.Context, actor, release string, files []string) (run PrefetchRun, started bool, err error) {
	if len(files) == 0 {
		return PrefetchRun{}, false, errors.New("firmware: nothing to prefetch")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != nil {
		return *p.snapshot(p.active), false, nil
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return PrefetchRun{}, false, err
	}
	r := &PrefetchRun{ID: hex.EncodeToString(id[:]), Release: release, Actor: actor, StartedAt: p.clk.Now()}
	for _, f := range files {
		r.Items = append(r.Items, PrefetchItem{File: f, State: PrefetchPending})
	}
	p.runs = append(p.runs, r)
	if len(p.runs) > maxPrefetchRuns {
		p.runs = p.runs[len(p.runs)-maxPrefetchRuns:]
	}
	p.active = r
	p.wg.Add(1)
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prefetchTimeout)
	go func() {
		defer cancel()
		p.run(runCtx, r)
	}()
	return *p.snapshot(r), true, nil
}

// Get returns a snapshot of the run with the given id.
func (p *Prefetcher) Get(id string) (PrefetchRun, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.runs {
		if r.ID == id {
			return *p.snapshot(r), true
		}
	}
	return PrefetchRun{}, false
}

// Active returns the running run, if any.
func (p *Prefetcher) Active() (PrefetchRun, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		return PrefetchRun{}, false
	}
	return *p.snapshot(p.active), true
}

// Wait blocks until no run is active (shutdown and tests).
func (p *Prefetcher) Wait() { p.wg.Wait() }

// snapshot copies r; the caller holds p.mu.
func (p *Prefetcher) snapshot(r *PrefetchRun) *PrefetchRun {
	c := *r
	c.Items = append([]PrefetchItem(nil), r.Items...)
	return &c
}

func (p *Prefetcher) set(r *PrefetchRun, i int, f func(*PrefetchItem)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(&r.Items[i])
}

func (p *Prefetcher) run(ctx context.Context, r *PrefetchRun) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		r.FinishedAt = p.clk.Now()
		p.active = nil
		p.mu.Unlock()
	}()
	loc, _ := p.cache.(Locator)
	for i := range r.Items {
		file := r.Items[i].File
		if err := ctx.Err(); err != nil {
			p.set(r, i, func(it *PrefetchItem) { it.State, it.Err = PrefetchFailed, "not started: "+err.Error() })
			continue
		}
		if loc != nil {
			if a, _, err := loc.Lookup(ctx, r.Release, file); err == nil {
				p.set(r, i, func(it *PrefetchItem) { it.State, it.Already, it.Size = PrefetchCached, true, a.Size })
				continue
			}
		}
		p.set(r, i, func(it *PrefetchItem) { it.State = PrefetchFetching })
		a, err := p.cache.Ensure(ctx, r.Release, file)
		p.set(r, i, func(it *PrefetchItem) {
			if err != nil {
				it.State, it.Err = PrefetchFailed, err.Error()
				return
			}
			it.State, it.Size = PrefetchCached, a.Size
		})
	}
}
