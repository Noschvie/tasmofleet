package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// The sentences of #826, as the product owner's comment of 2026-11-14 words them: over MQTT the command is unencrypted
// and the broker delivers it to every subscriber of the device's commands unless an ACL prevents that; with a TLS
// broker URL only TasmoFleet's leg to the broker is encrypted; over HTTP it is unencrypted too.
const (
	sayMQTTPlain = "The command goes to the device over the MQTT broker, unencrypted; the broker also delivers it to every client subscribed to this device's commands, and without a broker ACL that is every client of the broker."
	sayMQTTTLS   = "The command goes to the device over the MQTT broker, encrypted from TasmoFleet to the broker; the broker also delivers it to every client subscribed to this device's commands, and without a broker ACL that is every client of the broker."
	sayHTTP      = "The command goes to the device over HTTP, unencrypted."
)

// routeStub is Deps.TypedRoutes: the route of a typed command per device (HTTP unless set), and the broker's TLS.
type routeStub struct {
	mu      sync.Mutex
	mqtt    map[model.DeviceID]bool
	nowhere map[model.DeviceID]bool // transport.None: the router refuses it (MQTT does not carry it, HTTP fails)
	tls     bool
	asks    int
}

func (s *routeStub) routes(_ context.Context, _ string, ids ...model.DeviceID) (map[model.DeviceID]transport.Path, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks++
	out := map[model.DeviceID]transport.Path{}
	for _, id := range ids {
		out[id] = transport.HTTP
		if s.mqtt[id] {
			out[id] = transport.MQTT
		}
		if s.nowhere[id] {
			out[id] = transport.None
		}
	}
	return out, s.tls
}

func (s *routeStub) set(tls bool, overMQTT ...model.DeviceID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mqtt, s.tls = map[model.DeviceID]bool{}, tls
	for _, id := range overMQTT {
		s.mqtt[id] = true
	}
}

func noticeEnv(t *testing.T, rs *routeStub) (*env, *fakeCmd) {
	t.Helper()
	f := &fakeCmd{reply: `{"Done":"yes"}`}
	return setupWith(t, func(d *Deps) { d.DeviceCmd = f; d.TypedRoutes = rs.routes; d.SysConfig = aclBrokerService(t) }), f
}

// The page escapes the apostrophe of "device's".
func page(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

// Why (#826, the product owner's rule of 2026-11-14): a console command whose arguments the audit hides may carry a
// secret, and it crosses the network in plain text over either transport. Its confirmation names the transport the
// router will take, honestly (worded by the broker URL for MQTT), before anything is sent; the confirmed send carries
// that route so that the router keeps to it; the secret is never on the page.
func TestConsoleNamesTheTransportOfAHiddenCommandBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mqtt  bool
		tls   bool
		want  string
		route transport.Path
	}{
		{"over HTTP", false, false, sayHTTP, transport.HTTP},
		{"over MQTT, mqtt://", true, false, sayMQTTPlain, transport.MQTT},
		{"over MQTT, mqtts://", true, true, sayMQTTTLS, transport.MQTT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			if tc.mqtt {
				rs.set(tc.tls, 1)
			} else {
				rs.set(tc.tls)
			}
			e, f := noticeEnv(t, rs)
			for i, cmd := range []string{"PowerDelta 10", "WebPassword hunter2-826"} {
				resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(cmd))
				if resp.StatusCode != 200 || !strings.Contains(body, "Confirm sending") || !strings.Contains(body, page(tc.want)) || len(f.ran()) != i {
					t.Fatalf("%q: status %d, ran %v: want the sentence before anything is sent: %s", cmd, resp.StatusCode, f.ran(), body)
				}
				for _, other := range []string{sayHTTP, sayMQTTPlain, sayMQTTTLS} {
					if other != tc.want && strings.Contains(body, page(other)) {
						t.Fatalf("%q: names another route too: %s", cmd, body)
					}
				}
				if strings.Contains(body, "hunter2-826") {
					t.Fatalf("%q: the secret is on the page", cmd)
				}
				resp, body = e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
				if resp.StatusCode != 200 || strings.Contains(body, "hunter2-826") {
					t.Fatalf("%q: confirmed send: status %d: %s", cmd, resp.StatusCode, body)
				}
			}
			if len(f.routes) != 2 || f.routes[0] != tc.route || f.routes[1] != tc.route {
				t.Fatalf("the confirmed sends must carry the route %s: %v", tc.route, f.routes)
			}
		})
	}
}

// Why (#826): nothing changes for a command the audit shows in full: it asks nothing, carries no route, and the route
// is not even looked up.
func TestConsoleNamesNoRouteForACommandShownInFull(t *testing.T) {
	rs := &routeStub{}
	rs.set(false, 1)
	e, f := noticeEnv(t, rs)
	if _, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("TelePeriod 60")); strings.Contains(body, "Confirm sending") || len(f.ran()) != 1 ||
		rs.asks != 0 || f.routes[0] != "" {
		t.Fatalf("ran %v, asked %d, routes %v: %s", f.ran(), rs.asks, f.routes, body)
	}
}

// Why (#826): a command TasmoFleet never sends (a file transfer, also inside a Backlog) goes over no transport, so its
// refusal must not wait behind a confirmation that names one.
func TestConsoleNamesNoRouteForACommandNeverSent(t *testing.T) {
	for _, cmd := range []string{"FileUpload x", "Backlog Power 1; FileDownload y"} {
		rs := &routeStub{}
		rs.set(false, 1)
		e, _ := noticeEnv(t, rs)
		if _, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(cmd)); strings.Contains(body, "Confirm sending") || rs.asks != 0 {
			t.Fatalf("%q: asked %d: %s", cmd, rs.asks, body)
		}
	}
}

// Why (#826): the person may cancel; then nothing is sent.
func TestConsoleCancelledRouteConfirmationSendsNothing(t *testing.T) {
	rs := &routeStub{}
	rs.set(false, 1)
	e, f := noticeEnv(t, rs)
	_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("PowerDelta 10"))
	if !strings.Contains(body, page(sayMQTTPlain)) {
		t.Fatalf("no sentence: %s", body)
	}
	if resp, _ := e.do(t, "GET", "/devices/1/console", "admin", nil); resp.StatusCode != 200 || len(f.ran()) != 0 {
		t.Fatalf("Cancel: status %d, ran %v", resp.StatusCode, f.ran())
	}
}

// flipCmd is fakeCmd whose Runs first call flip (the router's facts change while the command is sent).
type flipCmd struct {
	*fakeCmd
	flip func()
}

func (c flipCmd) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	c.flip()
	return c.fakeCmd.Run(ctx, p, id, cmd)
}

// Why (#826): when the router refuses a send because its route is no longer the one the confirmation named (HTTP
// proved it sent nothing, MQTT published nothing), nothing was sent; the console asks again with the route that
// holds now, and that confirmation's send carries the new route.
func TestConsoleAsksAgainWhenTheRouterRefusesTheRoute(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, then []model.DeviceID
		want        string
		route       transport.Path
	}{
		{"HTTP failed at the send", nil, []model.DeviceID{1}, sayMQTTPlain, transport.MQTT},
		{"MQTT published nothing at the send", []model.DeviceID{1}, nil, sayHTTP, transport.HTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			rs.set(false, tc.first...)
			f := &fakeCmd{reply: `{"Done":"yes"}`, runErrs: []error{fmt.Errorf("%w: the route changed", transport.ErrRouteChanged)}}
			e := setupWith(t, func(d *Deps) {
				d.DeviceCmd = flipCmd{f, func() { rs.set(false, tc.then...) }}
				d.TypedRoutes = rs.routes
			})
			_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("PowerDelta 10"))
			resp, body := e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
			if len(f.ran()) != 1 || resp.StatusCode != http.StatusConflict {
				t.Fatalf("status %d, ran %v", resp.StatusCode, f.ran())
			}
			if !strings.Contains(body, "Confirm sending") || !strings.Contains(body, "the route changed") || !strings.Contains(body, page(tc.want)) {
				t.Fatalf("want the confirmation again, naming the route that holds now: %s", body)
			}
			e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
			if len(f.routes) != 2 || f.routes[1] != tc.route {
				t.Fatalf("routes per send %v, want the second over %s", f.routes, tc.route)
			}
		})
	}
}

// Why (#826): the sentence the person confirmed is one of the facts the page stated. When the route or the broker URL
// (mqtts:// to a plain one) changed before the send, that confirmation does not cover it: nothing is sent and the
// console asks again with the sentence that holds now.
func TestConsoleAsksAgainWhenTheSentenceChangedBeforeTheSend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		then       func(rs *routeStub)
		want       string
		wantRoute  transport.Path
		firstRoute string
	}{
		{"mqtts:// became mqtt://", func(rs *routeStub) { rs.set(false, 1) }, sayMQTTPlain, transport.MQTT, sayMQTTTLS},
		{"HTTP works again", func(rs *routeStub) { rs.set(true) }, sayHTTP, transport.HTTP, sayMQTTTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			rs.set(true, 1)
			e, f := noticeEnv(t, rs)
			_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("PowerDelta 10"))
			if !strings.Contains(body, page(tc.firstRoute)) {
				t.Fatalf("first sentence missing: %s", body)
			}
			tc.then(rs)
			resp, body := e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
			if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "Confirm sending") || !strings.Contains(body, page(tc.want)) || len(f.ran()) != 0 {
				t.Fatalf("status %d, ran %v: want the new sentence asked again, nothing sent: %s", resp.StatusCode, f.ran(), body)
			}
			e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
			if len(f.routes) != 1 || f.routes[0] != tc.wantRoute {
				t.Fatalf("routes %v, want %s", f.routes, tc.wantRoute)
			}
		})
	}
}

// Why (#826): bulk commands name the transport on their preview, per transport with how many of the chosen devices,
// and each device's send keeps to the route named for it.
func TestBulkPreviewNamesTheTransportPerDevice(t *testing.T) {
	rs := &routeStub{}
	rs.set(true, 2)
	fb := &fakeBatch{}
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = fb; d.TypedRoutes = rs.routes })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	resp, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
	wantMQTT := "1 of 2 devices. The command goes over the MQTT broker, encrypted from TasmoFleet to the broker; the broker also delivers it to every client subscribed to each device's commands, and without a broker ACL that is every client of the broker."
	wantHTTP := "1 of 2 devices. The command goes over HTTP, unencrypted."
	if resp.StatusCode != 200 || !strings.Contains(body, "<dt>Over MQTT</dt>") || !strings.Contains(body, page(wantMQTT)) ||
		!strings.Contains(body, "<dt>Over HTTP</dt>") || !strings.Contains(body, page(wantHTTP)) || len(fb.reqs) != 0 {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}
	if resp, _ = e.do(t, "POST", "/devices/bulk/command/run", "admin", bulkForm("token", confirmToken(t, body))); resp.StatusCode != http.StatusSeeOther || len(fb.reqs) != 1 {
		t.Fatalf("run: %d %v", resp.StatusCode, fb.reqs)
	}
	for _, r := range fb.reqs[0] {
		want := transport.HTTP
		if r.DeviceID == 2 {
			want = transport.MQTT
		}
		if r.Route != want {
			t.Errorf("device %d: Route %q, want %q", r.DeviceID, r.Route, want)
		}
	}
	// a command the audit shows in full, and a read, name no route
	for _, cmd := range []string{"TelePeriod 60", "Status 2"} {
		if _, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", cmd)); strings.Contains(body, "Over MQTT") || strings.Contains(body, "Over HTTP") {
			t.Errorf("%q: %s", cmd, body)
		}
	}
}

// Why (#826): the preview's routes and sentences are what the person confirmed. If the way to a device or the broker
// URL changed before the run, nothing is submitted and the person starts again.
func TestBulkRunSendsNothingWhenTheRoutesChanged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, then func(rs *routeStub)
	}{
		{"HTTP failed since the preview", func(rs *routeStub) { rs.set(false) }, func(rs *routeStub) { rs.set(false, 1) }},
		{"the broker URL is plain now", func(rs *routeStub) { rs.set(true, 1) }, func(rs *routeStub) { rs.set(false, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			tc.first(rs)
			fb := &fakeBatch{}
			e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = fb; d.TypedRoutes = rs.routes })
			_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", "PowerDelta 10"))
			if !strings.Contains(body, "Over ") {
				t.Fatalf("preview: %s", body)
			}
			tc.then(rs)
			resp, body := e.do(t, "POST", "/devices/bulk/command/run", "admin", bulkForm("token", confirmToken(t, body)))
			if resp.StatusCode != http.StatusConflict || len(fb.reqs) != 0 || !strings.Contains(body, "Nothing was sent") {
				t.Fatalf("run: %d %v %s", resp.StatusCode, fb.reqs, body)
			}
		})
	}
}

// Why (#826): a command MQTT does not carry, typed while the device's HTTP fails, goes nowhere. The console names no
// transport for it (it would be a false one) and sends it announced as going nowhere, so that the router lets it
// through to no transport; the bulk preview says it is not sent to those devices and the run leaves them out.
func TestACommandThatGoesNowhereNamesNoTransport(t *testing.T) {
	const cmd = `Json {"Status":0}`
	rs := &routeStub{nowhere: map[model.DeviceID]bool{1: true}}
	e, f := noticeEnv(t, rs)
	_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(cmd))
	for _, s := range []string{sayHTTP, sayMQTTPlain, sayMQTTTLS} {
		if strings.Contains(body, page(s)) {
			t.Fatalf("names a transport: %s", body)
		}
	}
	if len(f.ran()) != 1 || f.routes[0] != transport.None {
		t.Fatalf("ran %v routes %v, want one send announced as going nowhere", f.ran(), f.routes)
	}
	fb := &fakeBatch{}
	e = setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = fb; d.TypedRoutes = rs.routes })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", cmd))
	if !strings.Contains(body, "<dt>Not sent</dt>") || !strings.Contains(body, "1 of 2 devices. MQTT does not carry this command") || !strings.Contains(body, "<dt>Over HTTP</dt>") {
		t.Fatalf("preview: %s", body)
	}
	if resp, _ := e.do(t, "POST", "/devices/bulk/command/run", "admin", bulkForm("token", confirmToken(t, body))); resp.StatusCode != http.StatusSeeOther || len(fb.reqs) != 1 ||
		len(fb.reqs[0]) != 1 || fb.reqs[0][0].DeviceID != 2 {
		t.Fatalf("run: %d %v, want device 2 only", resp.StatusCode, fb.reqs)
	}
}
