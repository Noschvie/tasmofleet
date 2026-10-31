package app

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// What the status bar (#307) reads from the running server: the broker connection and the
// scheduled backup run. Both are small in-memory records; the bar shows them, nothing stores them.

// brokerPoll is how often the broker connection is compared with what the bar was last told. The
// bus has no change callback; this is a server-side compare of two booleans and a string, not
// a poll by the browser.
const brokerPoll = 2 * time.Second

// busState is the part of the MQTT bus switch the watch reads.
type busState interface {
	Enabled() bool
	Connected() bool
}

// brokerWatch keeps the broker state the status bar shows and tells the event stream when it
// changes (a connection that came up or went down, MQTT turned on or off, another broker).
type brokerWatch struct {
	bus busState
	hub *events.Hub
	now func() time.Time

	mu   sync.Mutex
	host string
	cur  web.BrokerInfo
}

func newBrokerWatch(bus busState, hub *events.Hub) *brokerWatch {
	w := &brokerWatch{bus: bus, hub: hub, now: time.Now}
	w.cur = w.read(w.now())
	return w
}

// SetHost records which broker is configured: host and port from the URL, never the user, the
// password or the path. "" (MQTT off) or an unparsable URL leaves no host.
func (w *brokerWatch) SetHost(rawURL string) {
	h := ""
	if u, err := url.Parse(rawURL); err == nil {
		h = u.Host
	}
	w.mu.Lock()
	w.host = h
	w.mu.Unlock()
}

// read is the state now; Since stays the time of the last change (set by check).
func (w *brokerWatch) read(now time.Time) web.BrokerInfo {
	return web.BrokerInfo{Enabled: w.bus.Enabled(), Connected: w.bus.Connected(), Host: w.host, Since: now}
}

// Status is the state for the bar.
func (w *brokerWatch) Status() web.BrokerInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// check compares the bus with the last state and publishes when it differs.
func (w *brokerWatch) check() {
	w.mu.Lock()
	now := w.now()
	next := w.read(now)
	changed := next.Enabled != w.cur.Enabled || next.Connected != w.cur.Connected || next.Host != w.cur.Host
	if changed {
		w.cur = next
	}
	w.mu.Unlock()
	if changed && w.hub != nil {
		w.hub.Publish(events.Event{Topic: events.TopicBroker, At: now, Payload: events.BrokerState{Connected: next.Connected}})
	}
}

// Run checks every interval until ctx ends.
func (w *brokerWatch) Run(ctx context.Context, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.check()
		}
	}
}

// scheduledRun is the scheduled backup run (BACKUP_INTERVAL): the one in progress and the last
// one that finished. It is in memory; after a restart the bar has no last run until the next ends.
type scheduledRun struct {
	hub *events.Hub
	now func() time.Time

	mu                  sync.Mutex
	running             bool
	done, total, failed int // of the run in progress
	last                web.BackupRunInfo
}

func newScheduledRun(hub *events.Hub) *scheduledRun { return &scheduledRun{hub: hub, now: time.Now} }

// Start begins a run over total devices.
func (r *scheduledRun) Start(total int) {
	r.mu.Lock()
	r.running, r.done, r.total, r.failed = true, 0, total, 0
	r.mu.Unlock()
	r.publish()
}

// Step records one device done, failed or not.
func (r *scheduledRun) Step(failed bool) {
	r.mu.Lock()
	r.done++
	if failed {
		r.failed++
	}
	r.mu.Unlock()
	r.publish()
}

// Finish ends the run. A run cut short by shutdown (completed false) is no result: the last
// finished run stays as it was.
func (r *scheduledRun) Finish(completed bool) {
	r.mu.Lock()
	if completed {
		r.last = web.BackupRunInfo{Finished: r.now(), LastTotal: r.total, LastFailed: r.failed}
	}
	r.running = false
	r.mu.Unlock()
	r.publish()
}

// Status is the state for the bar.
func (r *scheduledRun) Status() web.BackupRunInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.last
	out.Running, out.Done, out.Total = r.running, r.done, r.total
	return out
}

// publish tells the event stream the bar has something new. It reuses the backup topic: the
// payload is empty, the stream only says "backup" and the bar re-reads.
func (r *scheduledRun) publish() {
	if r.hub != nil {
		r.hub.Publish(events.Event{Topic: events.TopicBackup, Payload: model.Backup{}})
	}
}
