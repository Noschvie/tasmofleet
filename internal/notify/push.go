package notify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Web Push (#610, spec 14.5): every event the settings select is also pushed to the browsers admins
// subscribed, through their vendors' push services, so it shows when no TasmoFleet tab is open. The
// payload is encrypted for the browser (RFC 8291) and carries only the fields of the tab notice.

// pushTTL is how long a push service keeps a message for a browser that is offline.
const pushTTL = "86400"

// PushStore is the part of the store the push channel uses.
type PushStore interface {
	ListPushSubscriptions(ctx context.Context) ([]store.PushSubscription, error)
	SavePushSubscription(ctx context.Context, s store.PushSubscription) error
	DeletePushSubscription(ctx context.Context, endpoint string) (bool, error)
	RecordPushResult(ctx context.Context, endpoint string, at time.Time, failure string) error
}

// OwnerIsAdmin reports whether the owner of a subscription (sign-in method and subject) is an admin now.
// A definite answer (an operator, an account that is gone or disabled) is (false, nil); an error means it
// could not be told (the store failed), and the subscription is kept.
type OwnerIsAdmin func(ctx context.Context, method, subject string) (bool, error)

// Push is the Web Push channel. It is safe for concurrent use.
type Push struct {
	store   PushStore
	key     *ecdsa.PrivateKey
	isAdmin OwnerIsAdmin
	client  *http.Client // nil: the dispatcher's webhook client (tests set a client that trusts their server)
}

// NewPush returns the push channel for the stored VAPID key (see LoadVAPIDKey).
func NewPush(st PushStore, vapidKey string, isAdmin OwnerIsAdmin) (*Push, error) {
	k, err := parseVAPIDKey(vapidKey)
	if err != nil {
		return nil, err
	}
	return &Push{store: st, key: k, isAdmin: isAdmin}, nil
}

// PublicKey is the browser's applicationServerKey: the VAPID public key, base64url.
func (p *Push) PublicKey() string { return publicKeyB64(p.key) }

// Subscribe stores a browser's subscription for its owner after CheckSubscription; the same endpoint
// again replaces its row.
func (p *Push) Subscribe(ctx context.Context, s store.PushSubscription) error {
	if err := CheckSubscription(s.Endpoint, s.P256dh, s.Auth); err != nil {
		return err
	}
	return p.store.SavePushSubscription(ctx, s)
}

// Unsubscribe removes the subscription of endpoint; false when there was none.
func (p *Push) Unsubscribe(ctx context.Context, endpoint string) (bool, error) {
	return p.store.DeletePushSubscription(ctx, endpoint)
}

// SubscriptionID is the name of a subscription on a page and in a route: the SHA-256 of its endpoint,
// base64url. The endpoint's path is the browser's capability to receive pushes (W10) and never reaches
// HTML; the hash identifies the row without giving the capability away. web/static/push.js computes the
// same value to find "this browser" in the list (scripts/push-js.test.mjs holds the shared test vector).
func SubscriptionID(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// PushInfo is a subscription as the Settings list shows it: everything but the endpoint's path and the
// browser's keys.
type PushInfo struct {
	ID    string // SubscriptionID of the endpoint
	Label string // browser and system, as named when subscribing
	Host  string // the push service (the endpoint's host)

	OwnerMethod, OwnerSubject string
	CreatedAt, LastOKAt       time.Time
	LastError                 string
}

func pushInfoOf(s store.PushSubscription) PushInfo {
	return PushInfo{ID: SubscriptionID(s.Endpoint), Label: s.Label, Host: endpointHost(s.Endpoint),
		OwnerMethod: s.OwnerMethod, OwnerSubject: s.OwnerSubject, CreatedAt: s.CreatedAt, LastOKAt: s.LastOKAt, LastError: s.LastError}
}

// List returns every subscription, in the store's order.
func (p *Push) List(ctx context.Context) ([]PushInfo, error) {
	subs, err := p.store.ListPushSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PushInfo, 0, len(subs))
	for _, s := range subs {
		out = append(out, pushInfoOf(s))
	}
	return out, nil
}

// find returns the subscription with the given id; false when there is none.
func (p *Push) find(ctx context.Context, id string) (store.PushSubscription, bool, error) {
	subs, err := p.store.ListPushSubscriptions(ctx)
	if err != nil {
		return store.PushSubscription{}, false, err
	}
	for _, s := range subs {
		if SubscriptionID(s.Endpoint) == id {
			return s, true, nil
		}
	}
	return store.PushSubscription{}, false, nil
}

// Remove deletes the subscription with the given id, whoever owns it (any admin may: that is how the
// pushes to an SSO or header admin removed at the identity provider are stopped, W6). It returns what was
// removed; false when there was no such subscription.
func (p *Push) Remove(ctx context.Context, id string) (PushInfo, bool, error) {
	s, ok, err := p.find(ctx, id)
	if err != nil || !ok {
		return PushInfo{}, false, err
	}
	if _, err := p.store.DeletePushSubscription(ctx, s.Endpoint); err != nil {
		return PushInfo{}, false, err
	}
	return pushInfoOf(s), true, nil
}

// VAPIDStore is where the VAPID key lives (store.PushStore).
type VAPIDStore interface {
	VAPIDKey(ctx context.Context) (string, error)
	ReplaceVAPIDKey(ctx context.Context, key string) error
}

// LoadVAPIDKey returns the stored VAPID key and makes it on the first start. A stored key that cannot be
// opened (secret.key was replaced) or read is replaced, which drops every subscription (they belong to the
// old key; browsers subscribe again), with a warning. A store failure is returned: push stays off.
func LoadVAPIDKey(ctx context.Context, st VAPIDStore, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	k, err := st.VAPIDKey(ctx)
	switch {
	case err == nil:
		if _, perr := parseVAPIDKey(k); perr == nil {
			return k, nil
		}
		log.Warn("web push: the stored VAPID key is damaged; a new one is made and every push subscription is dropped (browsers subscribe again)")
	case errors.Is(err, store.ErrUnreadable):
		log.Warn("web push: the stored VAPID key cannot be opened with this secret.key; a new one is made and every push subscription is dropped (browsers subscribe again)")
	case errors.Is(err, store.ErrNotFound):
	default:
		return "", err
	}
	k, err = NewVAPIDKey()
	if err != nil {
		return "", err
	}
	if err := st.ReplaceVAPIDKey(ctx, k); err != nil {
		return "", err
	}
	return k, nil
}

// ownerVerdict is the answer of OwnerIsAdmin for one owner.
type ownerVerdict struct {
	admin bool
	err   error
}

// ErrPushOwnerGone: the owner of the subscription is no longer an admin; the subscription was removed.
var ErrPushOwnerGone = errors.New("the admin who subscribed this browser is no longer an admin; its subscription was removed")

// pushGate decides for one subscription whether anything may be sent to it now: an owner who is an admin
// (nil), a definite "no" (the row is removed: ErrPushOwnerGone) or a role that could not be read (the row is
// kept and nothing is sent, fail closed). The events and the test use it, so a test is no way around W6.
func (d *Dispatcher) pushGate(ctx context.Context, s store.PushSubscription, v ownerVerdict, event string) error {
	host := endpointHost(s.Endpoint)
	switch {
	case v.err != nil:
		d.o.Log.Warn("web push: not sent, the subscriber's role could not be read", "event", event, "push_service", host)
		return fmt.Errorf("the role of the admin who subscribed this browser could not be read: %w", v.err)
	case !v.admin:
		if _, err := d.o.Push.store.DeletePushSubscription(ctx, s.Endpoint); err != nil {
			d.o.Log.Warn("web push: removing the subscription of a former admin failed", "push_service", host, "err", err)
			return fmt.Errorf("removing the subscription of a former admin failed: %w", err)
		}
		d.o.Log.Info("web push: subscription removed, its owner is no longer an admin", "push_service", host)
		return ErrPushOwnerGone
	}
	return nil
}

// pushEvent sends ev to every subscription whose owner is an admin now. It runs in the background
// (Emit); every send is part of the dispatcher's WaitGroup.
func (d *Dispatcher) pushEvent(c config.Config, ev Event) {
	p := d.o.Push
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	subs, err := p.store.ListPushSubscriptions(ctx)
	if err != nil {
		d.o.Log.Warn("web push: listing the subscriptions failed", "event", ev.Name, "err", err)
		return
	}
	payload := pushPayload(c, ev)
	if payload == nil {
		d.o.Log.Warn("web push: the event does not fit a push", "event", ev.Name)
		return
	}
	type owner struct{ method, subject string }
	known := map[owner]ownerVerdict{}
	for _, s := range subs {
		o := owner{s.OwnerMethod, s.OwnerSubject}
		v, ok := known[o]
		if !ok {
			v.admin, v.err = p.isAdmin(ctx, o.method, o.subject)
			known[o] = v
		}
		if d.pushGate(ctx, s, v, ev.Name) != nil {
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // two attempts of 10 s, the pause and the row
			defer cancel()
			d.pushDeliver(ctx, s, ev, payload)
		}()
	}
}

// pushOutcome is how one push went: the push service's status (0 without an answer), the error (nil when
// it was accepted) and whether the answer 404/410 removed the subscription.
type pushOutcome struct {
	status  int
	err     error
	removed bool
}

// pushDeliver sends payload to one subscription (one retry on a network error, 5xx or 429) and records the
// outcome on its row; 404 and 410 (the browser unsubscribed or the subscription expired) remove the row.
// The events and the test push use it.
func (d *Dispatcher) pushDeliver(ctx context.Context, s store.PushSubscription, ev Event, payload []byte) pushOutcome {
	p, host := d.o.Push, endpointHost(s.Endpoint)
	cl := p.client
	if cl == nil {
		cl = d.client
	}
	status, retry, err := d.sendPush(ctx, cl, s, ev, payload)
	if err != nil && retry {
		select {
		case <-ctx.Done():
		case <-time.After(d.o.RetryDelay):
			status, _, err = d.sendPush(ctx, cl, s, ev, payload)
		}
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		if _, derr := p.store.DeletePushSubscription(ctx, s.Endpoint); derr != nil {
			d.o.Log.Warn("web push: removing an expired subscription failed", "push_service", host, "err", derr)
			return pushOutcome{status: status, err: errors.New("the push service no longer knows this browser, but its subscription could not be removed")}
		}
		d.o.Log.Info("web push: subscription removed, the push service no longer knows it", "push_service", host, "status", status)
		return pushOutcome{status: status, err: err, removed: true}
	case err != nil:
		d.o.Log.Warn("web push failed", "event", ev.Name, "push_service", host, "reason", err.Error())
		if rerr := p.store.RecordPushResult(ctx, s.Endpoint, d.o.Now(), err.Error()); rerr != nil {
			d.o.Log.Warn("web push: recording the result failed", "push_service", host, "err", rerr)
		}
		return pushOutcome{status: status, err: err}
	}
	if rerr := p.store.RecordPushResult(ctx, s.Endpoint, d.o.Now(), ""); rerr != nil {
		d.o.Log.Warn("web push: recording the result failed", "push_service", host, "err", rerr)
	}
	return pushOutcome{status: status}
}

// Errors of TestPush.
var (
	// ErrPushOff: Web Push is not set up (no VAPID key).
	ErrPushOff = errors.New("web push is off")
	// ErrPushNotFound: no subscription has this id (it was removed meanwhile).
	ErrPushNotFound = errors.New("no such subscription")
)

// pushTestPath is the page a click on the test notification opens.
const pushTestPath = "/settings/system?tab=notifications"

// TestPush sends the test notification to one subscription, whoever owns it (testing a phone from the desktop
// is the point). It passes the same gate as an event (an owner who is no longer an admin gets nothing and the
// row is removed), uses the same sender and records the same outcome on the row. It is rate-limited like the
// channel tests (ErrTooSoon). It runs until the push service answers; ctx bounds it.
func (d *Dispatcher) TestPush(ctx context.Context, id string) (Attempt, error) {
	p := d.o.Push
	if p == nil {
		return Attempt{}, ErrPushOff
	}
	s, ok, err := p.find(ctx, id)
	if err != nil {
		return Attempt{}, err
	}
	if !ok {
		return Attempt{}, ErrPushNotFound
	}
	d.mu.Lock()
	if d.o.Now().Sub(d.lastPushTest) < testGap {
		d.mu.Unlock()
		return Attempt{}, ErrTooSoon
	}
	d.lastPushTest = d.o.Now()
	d.mu.Unlock()
	if err := d.pushGate(ctx, s, d.verdictOf(ctx, s), EventTest); err != nil {
		return Attempt{}, err
	}
	c := d.o.Config()
	ev := TestEvent(c, d.o.Now())
	ev.ID, ev.Path = d.newEventID(), pushTestPath
	payload := pushPayload(c, ev)
	if payload == nil {
		return Attempt{}, errors.New("the test notification does not fit a push")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := d.pushDeliver(ctx, s, ev, payload)
	a := Attempt{At: d.o.Now(), OK: out.err == nil, Context: ev.Context}
	switch {
	case out.removed:
		a.OK, a.Detail = false, "the push service no longer knows this browser; its subscription was removed"
	case out.err != nil:
		a.Detail = out.err.Error()
	default:
		a.Detail = "the push service accepted it"
	}
	return a, nil
}

func (d *Dispatcher) verdictOf(ctx context.Context, s store.PushSubscription) ownerVerdict {
	var v ownerVerdict
	v.admin, v.err = d.o.Push.isAdmin(ctx, s.OwnerMethod, s.OwnerSubject)
	return v
}

// sendPush POSTs one encrypted push. status is the answer's status (0 without one); retry says a second
// attempt may help. An error text never carries the endpoint (its path is the browser's capability).
func (d *Dispatcher) sendPush(ctx context.Context, cl *http.Client, s store.PushSubscription, ev Event, payload []byte) (status int, retry bool, err error) {
	pub, err := decodeB64URL(s.P256dh)
	if err != nil {
		return 0, false, errors.New("the stored browser key is not base64url")
	}
	auth, err := decodeB64URL(s.Auth)
	if err != nil {
		return 0, false, errors.New("the stored auth secret is not base64url")
	}
	body, err := encryptPayload(pub, auth, payload)
	if err != nil {
		return 0, false, err
	}
	vapid, err := vapidAuth(d.o.Push.key, s.Endpoint, d.o.Now())
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, false, errors.New("the push address is not valid")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", pushTTL)
	req.Header.Set("Urgency", pushUrgency(ev))
	req.Header.Set("Authorization", vapid)
	req.Header.Set("User-Agent", "TasmoFleet")
	resp, err := cl.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectElsewhere) {
			return 0, false, errRedirectElsewhere
		}
		return 0, true, errors.New(scrubNet(err, ""))
	}
	defer func() { _ = resp.Body.Close() }() // the status is all that counts
	// A little of the answer is read so the connection can be reused; it is never logged or kept.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // best effort, see above
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return code, false, nil
	case code >= 300 && code < 400:
		return code, false, fmt.Errorf("HTTP %d (a redirect that was not followed)", code)
	default:
		return code, code >= 500 || code == http.StatusTooManyRequests, fmt.Errorf("HTTP %d", code)
	}
}

// pushUrgency asks the push service to wake a sleeping phone for problems only (RFC 8030).
func pushUrgency(ev Event) string {
	switch ev.Result {
	case ResultFailed, ResultPartial, ResultNotStarted:
		return "high"
	}
	return "normal"
}

// endpointHost is the push service's host, the only part of an endpoint a log line names.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "?"
	}
	return u.Host
}
