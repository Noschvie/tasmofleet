package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
)

// Attempt is one delivery or test and how it went. Detail is safe to show: the HTTP status or
// the reason, never an address with a token in it and never a secret.
type Attempt struct {
	At      time.Time // zero: it did not happen yet
	OK      bool
	Detail  string
	Context string // what was sent, e.g. "update job #41 (partial)"
}

// Status is what the settings page shows per channel. It lives in memory: after a restart it is
// empty until the next message or test.
type Status struct{ Test, Delivery Attempt }

// Options configure a Dispatcher.
type Options struct {
	// Config returns the effective settings at the moment of use.
	Config func() config.Config
	Log    *slog.Logger
	Now    func() time.Time
	// RetryDelay is the pause before the one retry; 0 means 2 s.
	RetryDelay time.Duration
	// Browser, if set, hears of every event the settings ask for, once, whether or not a channel
	// is switched on: the open browser tabs show it (#540). It must not block.
	Browser func(Event)
	// Push, if set, sends the same events to the browsers admins subscribed for Web Push (#610).
	Push *Push
	// TLS overrides the TLS settings of the SMTP sender (tests only).
	TLS *tls.Config
}

// Dispatcher sends events to the channels the settings turn on.
type Dispatcher struct {
	o      Options
	client *http.Client
	wg     sync.WaitGroup

	// boot and seq make the event ids: the start time of the process plus a counter, unique across
	// restarts (#540, #610).
	boot int64
	seq  atomic.Uint64

	mu     sync.Mutex
	status map[Channel]*Status
	last   map[Channel]time.Time // last test per channel, for the rate limit
	// lastPushTest is the time of the last Web Push test (any subscription), for the same rate limit. One
	// value, overwritten by the next test; a restart clears it.
	lastPushTest time.Time
}

// New returns a Dispatcher.
func New(o Options) *Dispatcher {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.RetryDelay == 0 {
		o.RetryDelay = 2 * time.Second
	}
	return &Dispatcher{o: o, client: newWebhookClient(), boot: time.Now().UnixNano(), status: map[Channel]*Status{Webhook: {}, Email: {}}, last: map[Channel]time.Time{}}
}

// Config is the effective configuration now.
func (d *Dispatcher) Config() config.Config { return d.o.Config() }

// Emit sends ev to every enabled channel, and hands it to the browser tabs and the Web Push channel, if the settings ask
// for the event. It sets ev.ID, the same for the tabs and the pushes. It returns at once; the outcome is recorded per
// channel (per subscription for Web Push). Safe to call from any goroutine.
func (d *Dispatcher) Emit(ev Event) {
	c := d.o.Config()
	if !Selected(c, ev) {
		return
	}
	ev.ID = d.newEventID()
	if d.o.Browser != nil {
		d.o.Browser(ev)
	}
	if d.o.Push != nil {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.pushEvent(c, ev)
		}()
	}
	for _, ch := range Enabled(c) {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.deliver(context.Background(), ch, c, ev, false)
		}()
	}
}

// newEventID makes an id that is unique across restarts.
func (d *Dispatcher) newEventID() string { return fmt.Sprintf("%x-%d", d.boot, d.seq.Add(1)) }

// Wait blocks until the deliveries started so far are done (tests and shutdown).
func (d *Dispatcher) Wait() { d.wg.Wait() }

// ErrTooSoon is returned by Test when the same channel was tested a moment ago.
var ErrTooSoon = errTooSoon{}

type errTooSoon struct{}

func (errTooSoon) Error() string { return "wait a few seconds between tests" }

const testGap = 3 * time.Second

// Test sends the test event to one channel with the settings c (the form, saved or not) and
// records it as the channel's last test. It sends even if the channel is switched off in c: the
// person pressed the button.
func (d *Dispatcher) Test(ctx context.Context, ch Channel, c config.Config) (Attempt, error) {
	d.mu.Lock()
	if d.o.Now().Sub(d.last[ch]) < testGap {
		d.mu.Unlock()
		return Attempt{}, ErrTooSoon
	}
	d.last[ch] = d.o.Now()
	d.mu.Unlock()
	return d.deliver(ctx, ch, c, TestEvent(c, d.o.Now()), true), nil
}

// Status returns what is known about a channel.
func (d *Dispatcher) Status(ch Channel) Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.status[ch]; s != nil {
		return *s
	}
	return Status{}
}

// deliver sends ev on one channel (the webhook with one retry) and records the outcome.
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, c config.Config, ev Event, test bool) Attempt {
	var detail string
	var err error
	switch ch {
	case Webhook:
		var retry bool
		detail, retry, err = sendWebhook(ctx, d.client, c, ev)
		if err != nil && retry {
			select {
			case <-ctx.Done():
			case <-time.After(d.o.RetryDelay):
				detail, _, err = sendWebhook(ctx, d.client, c, ev)
			}
		}
	case Email:
		detail, err = sendSMTP(ctx, c, ev, d.o.TLS)
	}
	a := Attempt{At: d.o.Now(), OK: err == nil, Detail: detail, Context: ev.Context}
	if err != nil {
		a.Detail = err.Error()
		d.o.Log.Warn("notification failed", "channel", string(ch), "event", ev.Name, "reason", a.Detail)
	}
	d.mu.Lock()
	if test {
		d.status[ch].Test = a
	} else {
		d.status[ch].Delivery = a
	}
	d.mu.Unlock()
	return a
}
