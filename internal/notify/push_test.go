package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/store"
)

// fakePushStore keeps the subscriptions and the VAPID key in memory.
type fakePushStore struct {
	mu      sync.Mutex
	subs    map[string]store.PushSubscription
	key     string
	keyErr  error // returned by VAPIDKey when set
	listErr error
	replace int
}

func (f *fakePushStore) VAPIDKey(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keyErr != nil {
		return "", f.keyErr
	}
	if f.key == "" {
		return "", store.ErrNotFound
	}
	return f.key, nil
}

func (f *fakePushStore) ReplaceVAPIDKey(_ context.Context, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.keyErr, f.replace = k, nil, f.replace+1
	f.subs = map[string]store.PushSubscription{}
	return nil
}

func (f *fakePushStore) ListPushSubscriptions(context.Context) ([]store.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.PushSubscription
	for _, s := range f.subs {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.PushSubscription) int { return strings.Compare(a.Endpoint, b.Endpoint) })
	return out, nil
}

func (f *fakePushStore) SavePushSubscription(_ context.Context, s store.PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[s.Endpoint] = s
	return nil
}

func (f *fakePushStore) DeletePushSubscription(_ context.Context, endpoint string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.subs[endpoint]
	delete(f.subs, endpoint)
	return ok, nil
}

func (f *fakePushStore) RecordPushResult(_ context.Context, endpoint string, at time.Time, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[endpoint]
	if !ok {
		return nil
	}
	if failure == "" {
		s.LastOKAt, s.LastError = at, ""
	} else {
		s.LastError = failure
	}
	f.subs[endpoint] = s
	return nil
}

func (f *fakePushStore) get(endpoint string) (store.PushSubscription, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[endpoint]
	return s, ok
}

// pushService is a loopback push service: it answers each path with the status set for it and keeps
// what it received.
type pushService struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status map[string][]int // per path, the answers in order (the last repeats)
	got    map[string][]*http.Request
	bodies map[string][][]byte
}

func newPushService(t *testing.T) *pushService {
	ps := &pushService{status: map[string][]int{}, got: map[string][]*http.Request{}, bodies: map[string][][]byte{}}
	ps.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		n := len(ps.got[r.URL.Path])
		ps.got[r.URL.Path] = append(ps.got[r.URL.Path], r)
		ps.bodies[r.URL.Path] = append(ps.bodies[r.URL.Path], b)
		code := http.StatusCreated
		if st := ps.status[r.URL.Path]; len(st) > 0 {
			code = st[min(n, len(st)-1)]
		}
		ps.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *pushService) requests(path string) ([]*http.Request, [][]byte) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.got[path], ps.bodies[path]
}

type subscriber struct {
	sub        store.PushSubscription
	priv, auth []byte
}

func (ps *pushService) subscriber(t *testing.T, path, method, subject string) subscriber {
	priv, pub, auth := testSubscriber(t)
	return subscriber{priv: priv, auth: auth, sub: store.PushSubscription{Endpoint: ps.srv.URL + path,
		P256dh: base64.RawURLEncoding.EncodeToString(pub), Auth: base64.RawURLEncoding.EncodeToString(auth), OwnerMethod: method, OwnerSubject: subject}}
}

// pushEnv is a dispatcher with the push channel on a loopback push service; admins lists the owners
// that are admins, roleErr makes the role lookup fail.
func pushEnv(t *testing.T, c *config.Config, log *bytes.Buffer, admins map[string]bool, roleErr error) (*Dispatcher, *fakePushStore, *pushService) {
	t.Helper()
	ps := newPushService(t)
	st := &fakePushStore{subs: map[string]store.PushSubscription{}}
	key, err := NewVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPush(st, key, func(_ context.Context, method, subject string) (bool, error) {
		if roleErr != nil {
			return false, roleErr
		}
		return admins[method+"/"+subject], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p.client = ps.srv.Client()
	o := Options{Config: func() config.Config { return *c }, Now: func() time.Time { return t0 }, RetryDelay: time.Millisecond, Push: p}
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if log != nil {
		o.Log = slog.New(slog.NewTextHandler(log, nil))
	}
	return New(o), st, ps
}

func (s subscriber) add(t *testing.T, st *fakePushStore) {
	t.Helper()
	if err := st.SavePushSubscription(t.Context(), s.sub); err != nil {
		t.Fatal(err)
	}
}

// Why (#610): the point of Web Push is that the event arrives with no tab open. The push must carry what
// the push service needs to accept it (VAPID for the key the browser subscribed with, aes128gcm, a TTL)
// and a body only that browser can read, holding the tab notice's fields.
func TestPushDeliversTheEventEncryptedWithVAPID(t *testing.T) {
	c := baseConfig()
	c.BasePath = "/tf"
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	a := ps.subscriber(t, "/a", "local", "local:admin")
	a.add(t, st)
	d.Emit(BackupFailed(c, 3, []FailedDevice{{Name: "lamp", Reason: "timed out"}}, t0))
	d.Wait()
	reqs, bodies := ps.requests("/a")
	if len(reqs) != 1 {
		t.Fatalf("%d pushes, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "86400" || r.Header.Get("Urgency") != "high" {
		t.Errorf("headers %v", r.Header)
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") || !strings.HasSuffix(r.Header.Get("Authorization"), ", k="+d.o.Push.PublicKey()) {
		t.Errorf("Authorization %q does not name the subscribed key", r.Header.Get("Authorization"))
	}
	var m map[string]string
	if err := json.Unmarshal(decryptPush(t, a.priv, a.auth, bodies[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != EventBackupFailed || m["path"] != "/tf/devices/backups?filter=failed" || !strings.Contains(m["text"], "lamp") || m["id"] == "" {
		t.Errorf("payload %v", m)
	}
	if s, _ := st.get(a.sub.Endpoint); !s.LastOKAt.Equal(t0) || s.LastError != "" {
		t.Errorf("row after a delivered push: %+v", s)
	}
}

// Why: a push is the same notification as the tab notice (#540), so it follows the same settings; an
// event switched off must not reach a phone either.
func TestPushFollowsTheEventSettings(t *testing.T) {
	c := baseConfig()
	c.NotifyBackup, c.NotifyUpdateWhen = false, config.NotifyWhenProblems
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	ps.subscriber(t, "/a", "local", "local:admin").add(t, st)
	d.Emit(BackupFailed(c, 3, []FailedDevice{{Name: "lamp"}}, t0))
	d.Emit(UpdateFinished(c, 1, 3, 0, 0, "1", t0)) // ok, but only problems are wanted
	d.Emit(UpdateFinished(c, 2, 2, 1, 0, "1", t0)) // partial: a problem
	d.Wait()
	reqs, _ := ps.requests("/a")
	if len(reqs) != 1 || reqs[0].Header.Get("Urgency") != "high" {
		t.Fatalf("%d pushes, want only the partial update", len(reqs))
	}
}

// Why: Chrome shows a notification for every push, and an open tab shows the same event. Only an id that
// both carry lets the browser fold the two into one (the notification tag). The id is made once per event
// and differs between events.
func TestPushAndTabShareTheEventID(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	a := ps.subscriber(t, "/a", "local", "local:admin")
	a.add(t, st)
	var mu sync.Mutex
	var tab []string
	d.o.Browser = func(ev Event) { mu.Lock(); tab = append(tab, ev.ID); mu.Unlock() }
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	_, bodies := ps.requests("/a")
	if len(tab) != 2 || len(bodies) != 2 {
		t.Fatalf("tab %v, %d pushes", tab, len(bodies))
	}
	for i, b := range bodies {
		var m map[string]string
		if err := json.Unmarshal(decryptPush(t, a.priv, a.auth, b), &m); err != nil {
			t.Fatal(err)
		}
		if m["id"] != tab[i] {
			t.Errorf("push %d id %q, tab id %q", i, m["id"], tab[i])
		}
	}
	if tab[0] == tab[1] || !regexp.MustCompile(`^[0-9a-f]+-[0-9]+$`).MatchString(tab[0]) {
		t.Errorf("ids %v: want <boot hex>-<counter>, different per event", tab)
	}
}

// Why (#610, acceptance): 404 and 410 mean the browser unsubscribed or the subscription expired; a row kept
// after that would be pushed to forever. A server error is worth one retry and keeps the row, and so does a
// refusal (another 4xx), which is recorded for the Settings list.
func TestPushRemovesSubscriptionOn404And410(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	ps.status["/gone404"], ps.status["/gone410"] = []int{404}, []int{410}
	ps.status["/err500"], ps.status["/flaky"], ps.status["/bad400"] = []int{500}, []int{503, 201}, []int{400}
	for _, p := range []string{"/gone404", "/gone410", "/err500", "/flaky", "/bad400"} {
		ps.subscriber(t, p, "local", "local:admin").add(t, st)
	}
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	for _, p := range []string{"/gone404", "/gone410"} {
		if _, ok := st.get(ps.srv.URL + p); ok {
			t.Errorf("%s: subscription kept after the push service said it is gone", p)
		}
	}
	for p, want := range map[string]struct {
		tries int
		err   string
	}{"/err500": {2, "HTTP 500"}, "/flaky": {2, ""}, "/bad400": {1, "HTTP 400"}} {
		reqs, _ := ps.requests(p)
		s, ok := st.get(ps.srv.URL + p)
		if !ok || len(reqs) != want.tries || s.LastError != want.err {
			t.Errorf("%s: kept %v, %d tries, error %q; want %d tries, error %q", p, ok, len(reqs), s.LastError, want.tries, want.err)
		}
	}
}

// Why: notifications are for admins (#540). A subscription outlives a demotion, a disabled or deleted
// account; at send time the owner's role decides, and a former admin's subscription is removed so it does
// not keep receiving events about the fleet.
func TestPushGoesOnlyToCurrentAdmins(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	ps.subscriber(t, "/admin", "local", "local:admin").add(t, st)
	ps.subscriber(t, "/demoted", "oidc", "sso-op").add(t, st)
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	if reqs, _ := ps.requests("/demoted"); len(reqs) != 0 {
		t.Fatal("a push reached a subscriber who is no longer an admin")
	}
	if _, ok := st.get(ps.srv.URL + "/demoted"); ok {
		t.Error("the former admin's subscription was kept")
	}
	if reqs, _ := ps.requests("/admin"); len(reqs) != 1 {
		t.Errorf("admin got %d pushes", len(reqs))
	}
}

// Why: a database hiccup while reading the role is no answer. Sending would fail open, deleting would wipe
// every subscription on one bad moment; the push is skipped and the row kept.
func TestPushKeepsTheSubscriptionWhenTheRoleCannotBeRead(t *testing.T) {
	c := baseConfig()
	var log bytes.Buffer
	d, st, ps := pushEnv(t, &c, &log, nil, errors.New("database is locked"))
	ps.subscriber(t, "/a", "local", "local:admin").add(t, st)
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	if reqs, _ := ps.requests("/a"); len(reqs) != 0 {
		t.Fatal("pushed although the role could not be read")
	}
	if _, ok := st.get(ps.srv.URL + "/a"); !ok {
		t.Fatal("subscription deleted on a store error")
	}
	if !strings.Contains(log.String(), "role could not be read") {
		t.Errorf("no warning: %s", log.String())
	}
}

// Why: an event that cannot fit a push (a base path so long that the uncut fields alone are over the limit)
// is not sent at all: an empty or broken push would show the admin nothing useful, and the log says why.
func TestPushSkipsAnEventThatDoesNotFit(t *testing.T) {
	c := baseConfig()
	c.BasePath = strings.Repeat("/x", 2100)
	var log bytes.Buffer
	d, st, ps := pushEnv(t, &c, &log, map[string]bool{"local/local:admin": true}, nil)
	ps.subscriber(t, "/a", "local", "local:admin").add(t, st)
	d.Emit(BackupFailed(c, 3, []FailedDevice{{Name: "lamp", Reason: "timed out"}}, t0))
	done := make(chan struct{})
	go func() { d.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the push of an event that cannot fit never ended")
	}
	if reqs, _ := ps.requests("/a"); len(reqs) != 0 {
		t.Fatalf("%d pushes of an event that does not fit", len(reqs))
	}
	if !strings.Contains(log.String(), "does not fit a push") {
		t.Errorf("no warning: %s", log.String())
	}
}

// Why: the endpoint's path is the browser's capability; anyone who has it (and the VAPID key) can push to
// it. A log line names the push service's host, never the endpoint.
func TestPushFailureLogNamesNoEndpoint(t *testing.T) {
	c := baseConfig()
	var log bytes.Buffer
	d, st, ps := pushEnv(t, &c, &log, map[string]bool{"local/local:admin": true}, nil)
	ps.status["/secret-capability-xyz"] = []int{500}
	ps.status["/gone-capability-abc"] = []int{410}
	a := ps.subscriber(t, "/secret-capability-xyz", "local", "local:admin")
	a.add(t, st)
	ps.subscriber(t, "/gone-capability-abc", "local", "local:admin").add(t, st)
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	out := log.String()
	if !strings.Contains(out, "web push failed") || !strings.Contains(out, strings.TrimPrefix(ps.srv.URL, "https://")) {
		t.Fatalf("expected a warning naming the push service host: %s", out)
	}
	for _, leak := range []string{"capability", a.sub.P256dh, a.sub.Auth} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains %q: %s", leak, out)
		}
	}
}

// Why: the key is made once and kept, or every subscription breaks at the next start. A key that cannot be
// opened (secret.key replaced) cannot sign anything: it is replaced, which drops the subscriptions made for
// it, and the log says so. A store failure is not "no key": nothing is replaced then.
func TestLoadVAPIDKey(t *testing.T) {
	var log bytes.Buffer
	l := slog.New(slog.NewTextHandler(&log, nil))
	st := &fakePushStore{subs: map[string]store.PushSubscription{}}
	k1, err := LoadVAPIDKey(t.Context(), st, l)
	if err != nil || k1 == "" || st.replace != 1 {
		t.Fatalf("first start: %q %v, %d replaces", k1, err, st.replace)
	}
	if k2, err := LoadVAPIDKey(t.Context(), st, l); err != nil || k2 != k1 || st.replace != 1 {
		t.Fatalf("second start made a new key: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("warning on a normal start: %s", log.String())
	}
	st.subs["https://push.example/a"] = store.PushSubscription{Endpoint: "https://push.example/a"}
	st.keyErr = store.ErrUnreadable
	k3, err := LoadVAPIDKey(t.Context(), st, l)
	if err != nil || k3 == k1 || len(st.subs) != 0 || !strings.Contains(log.String(), "cannot be opened") {
		t.Fatalf("unreadable key: %v, subs %d, log %q", err, len(st.subs), log.String())
	}
	st.key = "damaged"
	if k4, err := LoadVAPIDKey(t.Context(), st, l); err != nil || k4 == "damaged" {
		t.Fatalf("damaged key kept: %v", err)
	}
	st.keyErr = errors.New("disk I/O error")
	n := st.replace
	if _, err := LoadVAPIDKey(t.Context(), st, l); err == nil || st.replace != n {
		t.Fatal("a store error replaced the key")
	}
}

// Why: Subscribe is the door from the browser into the table; it must apply the check, not only the web
// handler.
func TestSubscribeChecksTheSubscription(t *testing.T) {
	st := &fakePushStore{subs: map[string]store.PushSubscription{}}
	key, _ := NewVAPIDKey()
	p, err := NewPush(st, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, pub, auth := testSubscriber(t)
	good := store.PushSubscription{Endpoint: "https://push.example/a", P256dh: base64.RawURLEncoding.EncodeToString(pub), Auth: base64.RawURLEncoding.EncodeToString(auth)}
	bad := good
	bad.Endpoint = "http://push.example/a"
	if err := p.Subscribe(t.Context(), bad); !errors.Is(err, ErrBadSubscription) {
		t.Fatalf("plain http subscription: %v", err)
	}
	if err := p.Subscribe(t.Context(), good); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.Unsubscribe(t.Context(), good.Endpoint); err != nil || !ok || len(st.subs) != 0 {
		t.Fatalf("unsubscribe: %v %v", ok, err)
	}
	if p.PublicKey() == "" || strings.Contains(p.PublicKey(), key) {
		t.Fatal("public key empty or carrying the private key")
	}
}

// ownerRows holds one subscription of an admin on the loopback push service.
func ownerRows(t *testing.T, ps *pushService, st *fakePushStore, paths ...string) []subscriber {
	t.Helper()
	var out []subscriber
	for _, p := range paths {
		s := ps.subscriber(t, p, "local", "local:admin")
		s.add(t, st)
		out = append(out, s)
	}
	return out
}

// Why (#611): "Send test" must show the person that a push reaches that browser: the test arrives
// encrypted for the one browser chosen, through the same sender as an event, and its outcome is on the
// row like an event's. Another subscription must not get it.
func TestPushTestSendsToOneSubscriptionAndRecordsTheResult(t *testing.T) {
	c := baseConfig()
	c.BasePath = "/tf"
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	subs := ownerRows(t, ps, st, "/a", "/b")
	a, err := d.TestPush(t.Context(), SubscriptionID(subs[0].sub.Endpoint))
	if err != nil || !a.OK {
		t.Fatalf("TestPush = %+v, %v", a, err)
	}
	reqs, bodies := ps.requests("/a")
	if len(reqs) != 1 {
		t.Fatalf("%d pushes to the chosen browser, want 1", len(reqs))
	}
	if other, _ := ps.requests("/b"); len(other) != 0 {
		t.Fatalf("the test went to another browser too: %d", len(other))
	}
	var m map[string]string
	if err := json.Unmarshal(decryptPush(t, subs[0].priv, subs[0].auth, bodies[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != EventTest || m["id"] == "" || m["path"] != "/tf/settings/system?tab=notifications" {
		t.Errorf("payload %v: want the test event with an id and a click target below the base path", m)
	}
	if s, _ := st.get(subs[0].sub.Endpoint); !s.LastOKAt.Equal(t0) || s.LastError != "" {
		t.Errorf("row after the test: %+v, want the delivery recorded", s)
	}
	if s, _ := st.get(subs[1].sub.Endpoint); !s.LastOKAt.IsZero() {
		t.Errorf("another row was marked: %+v", s)
	}
}

// Why (W6): a test must not become a way around the owner check. A subscription whose admin was demoted
// gets nothing, not even when someone presses "Send test" on it, and is removed like at an event.
func TestPushTestRefusesWhenOwnerIsNoLongerAdminAndRemovesTheRow(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{}, nil)
	row := ownerRows(t, ps, st, "/a")[0]
	_, err := d.TestPush(t.Context(), SubscriptionID(row.sub.Endpoint))
	if !errors.Is(err, ErrPushOwnerGone) {
		t.Fatalf("TestPush = %v, want ErrPushOwnerGone", err)
	}
	if reqs, _ := ps.requests("/a"); len(reqs) != 0 {
		t.Fatalf("a push went to a former admin's browser")
	}
	if _, ok := st.get(row.sub.Endpoint); ok {
		t.Fatalf("the former admin's subscription was kept")
	}
}

// Why: a role that cannot be read is not "no": the row stays (one database hiccup must not wipe it), and
// nothing is sent (fail closed).
func TestPushTestKeepsTheRowWhenTheRoleCannotBeRead(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, nil, errors.New("database is locked"))
	row := ownerRows(t, ps, st, "/a")[0]
	if _, err := d.TestPush(t.Context(), SubscriptionID(row.sub.Endpoint)); err == nil || errors.Is(err, ErrPushOwnerGone) {
		t.Fatalf("TestPush = %v, want a role error", err)
	}
	if reqs, _ := ps.requests("/a"); len(reqs) != 0 {
		t.Fatalf("a push was sent although the role could not be read")
	}
	if _, ok := st.get(row.sub.Endpoint); !ok {
		t.Fatalf("the row was removed for a failed read")
	}
}

// Why: the test is rate-limited like the channel tests (a double click must not send twice), and the
// gap is the dispatcher's, so a later test passes.
func TestPushTestTooSoon(t *testing.T) {
	c := baseConfig()
	now := t0
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	d.o.Now = func() time.Time { return now }
	row := ownerRows(t, ps, st, "/a")[0]
	id := SubscriptionID(row.sub.Endpoint)
	if _, err := d.TestPush(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TestPush(t.Context(), id); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("second test at once: %v, want ErrTooSoon", err)
	}
	if reqs, _ := ps.requests("/a"); len(reqs) != 1 {
		t.Fatalf("%d pushes, want 1", len(reqs))
	}
	now = now.Add(testGap)
	if _, err := d.TestPush(t.Context(), id); err != nil {
		t.Fatalf("after the gap: %v", err)
	}
}

// Why: a subscription the push service no longer knows must go at once, also when the person found out by
// pressing "Send test": the answer says so and the row is gone, as at an event (W9).
func TestPushTestRemovesTheRowOn410(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	ps.status["/gone"] = []int{410}
	row := ownerRows(t, ps, st, "/gone")[0]
	a, err := d.TestPush(t.Context(), SubscriptionID(row.sub.Endpoint))
	if err != nil || a.OK || !strings.Contains(a.Detail, "removed") {
		t.Fatalf("TestPush = %+v, %v, want a failed attempt that says the subscription was removed", a, err)
	}
	if _, ok := st.get(row.sub.Endpoint); ok {
		t.Fatalf("the row was kept after a 410")
	}
}

// Why: a refusal of the push service is shown with its reason and recorded on the row, never swallowed, and
// never carries the endpoint (its path is the browser's capability, W10).
func TestPushTestReportsARefusalWithoutTheEndpoint(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	ps.status["/secret-capability-path"] = []int{400}
	row := ownerRows(t, ps, st, "/secret-capability-path")[0]
	a, err := d.TestPush(t.Context(), SubscriptionID(row.sub.Endpoint))
	if err != nil || a.OK || a.Detail != "HTTP 400" {
		t.Fatalf("TestPush = %+v, %v", a, err)
	}
	if s, _ := st.get(row.sub.Endpoint); s.LastError != "HTTP 400" {
		t.Errorf("row error %q", s.LastError)
	}
}

// Why: a removed or unknown id is "not found", so the page can say so, and it does not use up the gap.
func TestPushTestUnknownID(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{"local/local:admin": true}, nil)
	row := ownerRows(t, ps, st, "/a")[0]
	if _, err := d.TestPush(t.Context(), "nope"); !errors.Is(err, ErrPushNotFound) {
		t.Fatalf("TestPush(unknown) = %v, want ErrPushNotFound", err)
	}
	if _, err := d.TestPush(t.Context(), SubscriptionID(row.sub.Endpoint)); err != nil {
		t.Fatalf("a refused unknown id used up the rate limit: %v", err)
	}
}

// Why: the id is a hash of the endpoint, so a page and a route can name a row without the endpoint's path
// (the browser's capability). push.js computes the same hash to find "this browser"; this vector is the
// one scripts/push-js.test.mjs checks, so the two sides cannot drift apart.
func TestSubscriptionIDIsSHA256OfEndpoint(t *testing.T) {
	const endpoint = "https://push.example/send/abc123"
	const want = "qB6dm6Gh1VhQw65vM85YGzBtiCPxGKw9rJkTClwwb6E"
	if got := SubscriptionID(endpoint); got != want {
		t.Fatalf("SubscriptionID = %q, want %q", got, want)
	}
	if strings.Contains(SubscriptionID(endpoint), "abc123") {
		t.Fatal("the id carries the endpoint")
	}
}

// Why: the list must show who subscribed and how the last push went, and Remove (any admin may, W6's remedy)
// must delete exactly the named row and say what it removed (the audit names the host).
func TestPushListAndRemoveByID(t *testing.T) {
	c := baseConfig()
	d, st, ps := pushEnv(t, &c, nil, map[string]bool{}, nil)
	rows := ownerRows(t, ps, st, "/a", "/b")
	st.subs[rows[0].sub.Endpoint] = store.PushSubscription{Endpoint: rows[0].sub.Endpoint, P256dh: rows[0].sub.P256dh, Auth: rows[0].sub.Auth,
		OwnerMethod: "oidc", OwnerSubject: "sso-bob", Label: "Firefox on Windows", LastError: "HTTP 500"}
	list, err := d.o.Push.List(t.Context())
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	var a PushInfo
	for _, i := range list {
		if i.ID == SubscriptionID(rows[0].sub.Endpoint) {
			a = i
		}
		if strings.Contains(i.ID+i.Label+i.Host, "https://") || strings.Contains(i.ID, "/") {
			t.Errorf("an info carries the endpoint: %+v", i)
		}
	}
	if a.Label != "Firefox on Windows" || a.OwnerSubject != "sso-bob" || a.LastError != "HTTP 500" || a.Host == "" || strings.Contains(a.Host, "/") {
		t.Errorf("info %+v", a)
	}
	gone, ok, err := d.o.Push.Remove(t.Context(), a.ID)
	if err != nil || !ok || gone.Host != a.Host {
		t.Fatalf("Remove = %+v, %v, %v", gone, ok, err)
	}
	if _, ok := st.get(rows[0].sub.Endpoint); ok {
		t.Error("the row is still there")
	}
	if _, ok := st.get(rows[1].sub.Endpoint); !ok {
		t.Error("another row was removed")
	}
	if _, ok, _ := d.o.Push.Remove(t.Context(), a.ID); ok {
		t.Error("a second Remove found the row again")
	}
}
