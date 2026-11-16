package notify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
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
	type verdict struct {
		admin bool
		err   error
	}
	known := map[owner]verdict{}
	for _, s := range subs {
		o := owner{s.OwnerMethod, s.OwnerSubject}
		v, ok := known[o]
		if !ok {
			v.admin, v.err = p.isAdmin(ctx, o.method, o.subject)
			known[o] = v
		}
		host := endpointHost(s.Endpoint)
		switch {
		case v.err != nil:
			// The store could not tell: keep the subscription, send nothing this time (fail closed).
			d.o.Log.Warn("web push: not sent, the subscriber's role could not be read", "event", ev.Name, "push_service", host)
			continue
		case !v.admin:
			if _, err := p.store.DeletePushSubscription(ctx, s.Endpoint); err != nil {
				d.o.Log.Warn("web push: removing the subscription of a former admin failed", "push_service", host, "err", err)
				continue
			}
			d.o.Log.Info("web push: subscription removed, its owner is no longer an admin", "push_service", host)
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.pushOne(s, ev, payload)
		}()
	}
}

// pushOne sends payload to one subscription (one retry on a network error, 5xx or 429) and records the
// outcome on its row; 404 and 410 (the browser unsubscribed or the subscription expired) remove the row.
func (d *Dispatcher) pushOne(s store.PushSubscription, ev Event, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // two attempts of 10 s, the pause and the row
	defer cancel()
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
			return
		}
		d.o.Log.Info("web push: subscription removed, the push service no longer knows it", "push_service", host, "status", status)
		return
	case err != nil:
		d.o.Log.Warn("web push failed", "event", ev.Name, "push_service", host, "reason", err.Error())
		if rerr := p.store.RecordPushResult(ctx, s.Endpoint, d.o.Now(), err.Error()); rerr != nil {
			d.o.Log.Warn("web push: recording the result failed", "push_service", host, "err", rerr)
		}
		return
	}
	if rerr := p.store.RecordPushResult(ctx, s.Endpoint, d.o.Now(), ""); rerr != nil {
		d.o.Log.Warn("web push: recording the result failed", "push_service", host, "err", rerr)
	}
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
