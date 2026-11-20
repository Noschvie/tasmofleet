package transport

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// A command a person types whose arguments the audit hides (#826): a secret can be in it, and it crosses the network
// in plain text over either transport. The product owner's rule (2026-11-14): its confirmation names the transport it
// will take, honestly, and the send never takes another one.
const hiddenCmd = "BLEPair A4C138AABBCC 123456"

// Why (#826): the confirmation is only honest when it names the route the send then takes. TypedRoute must be the
// router's own decision in every state the router distinguishes; for each state an unannounced typed send shows the
// route the router takes on its own, and TypedRoute must name exactly that.
func TestTypedRouteIsTheRouteTheSendTakes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cmd   string
		setup func(g *rig)
		want  Path
	}{
		{"HTTP unknown, usable on the broker", hiddenCmd, func(*rig) {}, MQTT},
		{"HTTP answers", hiddenCmd, func(g *rig) { g.f.httpOK = true }, MQTT},
		{"HTTP failing", hiddenCmd, func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable) }, MQTT},
		{"writes over MQTT not possible, HTTP works", hiddenCmd, func(g *rig) { g.f.groupKnown = false }, HTTP},
		{"no path at all", hiddenCmd, func(g *rig) { g.f.groupKnown = false; g.mem.Fail(7, HTTPFailUnreachable) }, HTTP},
		{"not on our broker", hiddenCmd, func(g *rig) { g.devs.d.MQTT = model.MQTTUnknown }, HTTP},
		{"MQTT just went unanswered", hiddenCmd, func(g *rig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }, HTTP},
		{"the health poll newer than the failure", hiddenCmd, func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable); g.f.healthNewer = true }, HTTP},
		{"MQTT not connected", hiddenCmd, func(g *rig) { g.mq.down = true }, HTTP},
		{"the channel never publishes it, HTTP works", "Rule1 ON Power1#State DO WebPassword x ENDON", func(g *rig) {
			g.mq.err = fmt.Errorf("%w: Rule1 can run other commands", mqttcmd.ErrRefused)
		}, HTTP},
		{"it starts a job", "WebGetConfig http://u:pw@h/c.dmp", func(*rig) {}, HTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !ConfirmsRoute(tc.cmd) {
				t.Fatalf("ConfirmsRoute(%q) = false", tc.cmd)
			}
			g := newRig(t)
			tc.setup(g)
			if got := g.r.TypedRoute(t.Context(), 7, tc.cmd); got != tc.want {
				t.Fatalf("TypedRoute = %s, want %s", got, tc.want)
			}
			ctx, tr := WithTrace(WithConsole(t.Context()))
			_, _ = g.r.Command(ctx, g.target(), tc.cmd) //nolint:errcheck // only the route matters here
			if tr.Via() != map[Path]string{HTTP: "http", MQTT: "mqtt"}[tc.want] {
				t.Fatalf("the send went %s, TypedRoute said %s", tr.Via(), tc.want)
			}
			// announced, it goes the same way (a fresh rig: the send above taught the memory what HTTP did)
			g = newRig(t)
			tc.setup(g)
			ctx, tr = WithTrace(WithRoute(WithConsole(t.Context()), tc.want))
			_, err := g.r.Command(ctx, g.target(), tc.cmd)
			if errors.Is(err, ErrRouteChanged) || tr.Via() != map[Path]string{HTTP: "http", MQTT: "mqtt"}[tc.want] {
				t.Fatalf("announced %s: err=%v via=%s", tc.want, err, tr.Via())
			}
		})
	}
}

// Why (#826): a confirmation that said HTTP is no consent to the broker. When HTTP then proves it sent nothing, the
// router fails over to MQTT for every other write; for this one it publishes nothing and the person is asked again
// (by then the memory knows HTTP fails, so the next confirmation names the broker).
func TestAnAnnouncedHTTPSendNeverFailsOverToMQTT(t *testing.T) {
	g := newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable) // writes start on HTTP
	if got := g.r.TypedRoute(t.Context(), 7, hiddenCmd); got != HTTP {
		t.Fatalf("TypedRoute = %s, want http", got)
	}
	g.http.err = tasmota.ErrForbiddenAddr // provably not sent: any other write would go over MQTT now
	ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), HTTP))
	if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 1 || tr.Via() != "none" {
		t.Fatalf("err=%v mqtt=%d http=%d via=%s, want the MQTT failover refused", err, g.mq.n(), g.http.n(), tr.Via())
	}
	if got := g.r.TypedRoute(t.Context(), 7, hiddenCmd); got != MQTT {
		t.Fatalf("after the refused HTTP call TypedRoute = %s, want mqtt for the next confirmation", got)
	}
	// unannounced (TasmoFleet's own write) the failover is unchanged
	g.mem.Clear(7)
	if _, err := g.r.Command(t.Context(), g.target(), hiddenCmd); err != nil || g.mq.n() != 1 {
		t.Fatalf("unannounced: err=%v mqtt=%d, want the failover", err, g.mq.n())
	}
}

// Why (#826): a confirmation that said MQTT is no consent to HTTP either. When MQTT publishes nothing (the bus went
// down, the device's last will says Offline) the router goes on over HTTP for every other write; this one is not
// sent, and the person is asked again with the route that holds now.
func TestAnAnnouncedMQTTSendNeverGoesOnOverHTTP(t *testing.T) {
	g := newRig(t)
	g.mq.err = fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), MQTT))
	if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.http.n() != 0 || tr.Via() != "none" {
		t.Fatalf("err=%v mqtt=%d http=%d via=%s, want nothing over HTTP", err, g.mq.n(), g.http.n(), tr.Via())
	}
}

// Why (#826): the facts may change between the confirmation and the send (HTTP failed meanwhile, the device's MQTT
// went unanswered). The send then takes neither route: nothing is sent, ErrRouteChanged says why, and the console asks
// again. This holds for every way to the broker, the pinned one included.
func TestAConfirmedRouteThatNoLongerHoldsSendsNothing(t *testing.T) {
	g := newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable) // the route is HTTP now
	ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), MQTT))
	if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
		t.Fatalf("said MQTT, route HTTP: err=%v mqtt=%d http=%d via=%s, want nothing sent", err, g.mq.n(), g.http.n(), tr.Via())
	}
	g = newRig(t) // the route is MQTT now
	ctx, tr = WithTrace(WithRoute(WithConsole(t.Context()), HTTP))
	if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
		t.Fatalf("said HTTP, route MQTT: err=%v mqtt=%d http=%d via=%s, want nothing sent", err, g.mq.n(), g.http.n(), tr.Via())
	}
	ctx, tr = WithTrace(WithRoute(WithPath(WithConsole(t.Context()), MQTT), HTTP))
	if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || tr.Via() != "none" {
		t.Fatalf("said HTTP, pinned MQTT: err=%v mqtt=%d via=%s, want nothing published", err, g.mq.n(), tr.Via())
	}
	// HTTP failing: the route is MQTT; a confirmation that said HTTP sends nothing over the HTTP known to fail either
	g.mem.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(WithRoute(WithConsole(t.Context()), HTTP), g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 0 {
		t.Fatalf("said HTTP, HTTP failing: err=%v mqtt=%d http=%d, want nothing sent", err, g.mq.n(), g.http.n())
	}
}

// Why (#826): a command the MQTT channel never publishes is named HTTP (it never crosses the broker). Announced so,
// it goes over HTTP as such commands always do: the router refuses it for the broker the way the channel would, so
// the write goes on over HTTP; nothing is published.
func TestACommandMQTTDoesNotCarryGoesOverHTTPAsAnnounced(t *testing.T) {
	const rule = "Rule1 ON Power1#State DO WebPassword x ENDON"
	if !mqttcmd.Refuses(rule) {
		t.Fatalf("the channel would publish %q", rule)
	}
	g := newRig(t)
	ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), HTTP))
	if _, err := g.r.Command(ctx, g.target(), rule); err != nil || g.mq.n() != 0 || g.http.n() != 1 || tr.Via() != "http" {
		t.Fatalf("err=%v mqtt=%d http=%d via=%s, want one send over HTTP", err, g.mq.n(), g.http.n(), tr.Via())
	}
}

// Why (#826): only a change whose arguments the audit hides names its route; what the audit shows in full and reads
// keep their way without a question.
func TestOnlyHiddenTypedWritesNameTheirRoute(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{hiddenCmd, true},
		{"WebPassword hunter2", true},
		{"MqttPassword hunter2", true},
		{"Backlog Power 1; WebPassword hunter2", true},
		{"Rule1 ON Power1#State DO WebPassword x ENDON", true},
		{"TelePeriod 60", false}, // shown in full
		{"Power1 ON", false},     // shown in full
		{"Status 2", false},      // a read
		{"WebPassword", false},   // a read, nothing to hide
		// #1046: a group topic is a topic name, shown in full like Topic, so it names no route; a login pasted into
		// it is masked and does
		{"GroupTopic2 kitchen", false},
		{"GroupTopic2 a:b@c", true},
		// never sent (cmdpolicy.ClassForbidden): no transport to name, it is refused at once
		{"FileUpload x", false},
		{"Backlog Power 1; FileDownload y", false},
	} {
		if got := ConfirmsRoute(tc.cmd); got != tc.want {
			t.Errorf("ConfirmsRoute(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// Why (#826): an announcement that names neither transport (a caller's bug: an empty route) must not turn into "any
// route": nothing is sent.
func TestARouteThatIsNeitherTransportSendsNothing(t *testing.T) {
	for _, p := range []Path{None, ""} {
		g := newRig(t)
		ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), p))
		if _, err := g.r.Command(ctx, g.target(), hiddenCmd); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
			t.Errorf("route %q: err=%v mqtt=%d http=%d via=%s, want nothing sent", p, err, g.mq.n(), g.http.n(), tr.Via())
		}
	}
}

// Why (#826): a command the MQTT channel never publishes, typed while the device's HTTP is known to fail, goes nowhere:
// the router tries MQTT and the channel refuses it. The confirmation must not name a transport for it (None), and
// announced so, nothing is sent: not over the broker, and not over HTTP should HTTP come back before the send.
func TestACommandMQTTDoesNotCarryGoesNowhereWhileHTTPFails(t *testing.T) {
	const rule = "Rule1 ON Power1#State DO WebPassword x ENDON"
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	if got := g.r.TypedRoute(t.Context(), 7, rule); got != None {
		t.Fatalf("TypedRoute = %s, want none", got)
	}
	ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), None))
	if _, err := g.r.Command(ctx, g.target(), rule); !errors.Is(err, mqttcmd.ErrRefused) || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
		t.Fatalf("err=%v mqtt=%d http=%d via=%s, want the channel's refusal, nothing sent and the audit saying so", err, g.mq.n(), g.http.n(), tr.Via())
	}
	g.mem.Clear(7) // HTTP works again before the send: the route said nothing goes, so nothing goes
	ctx, tr = WithTrace(WithRoute(WithConsole(t.Context()), None))
	if _, err := g.r.Command(ctx, g.target(), rule); !errors.Is(err, ErrRouteChanged) || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
		t.Fatalf("HTTP back: err=%v mqtt=%d http=%d via=%s, want nothing sent", err, g.mq.n(), g.http.n(), tr.Via())
	}
}

// Why (#1001, the maintainer's decision of 2026-11-19): the MQTT channel never carries a Status 0 (safety rule 2), so
// on a device whose command route is MQTT the router would send a typed Status 0 over HTTP, and on a device reached
// over MQTT only that ends in a timeout instead of a reason. "HTTP not failing" is no proof that HTTP works: a device
// live on MQTT normally has its HTTP state unknown (docs/pitfalls.md). So TypedRoute says None for a Status 0 there
// unless a fresh HTTP answer is on record, and the send announced so reaches neither transport. Where HTTP is known
// to work, or the device's writes go over HTTP anyway, Status 0 still goes over HTTP: refusing it there would take
// the admin's own query away for nothing.
func TestTypedRouteSendsStatus0NowhereWhileHTTPIsNotKnownToWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(g *rig)
		want  Path
	}{
		{"HTTP unknown, MQTT is the route", func(*rig) {}, None},
		{"HTTP failing, MQTT is the route", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable) }, None},
		{"a fresh HTTP answer on record", func(g *rig) { g.f.httpOK = true }, HTTP},
		{"writes over MQTT not possible", func(g *rig) { g.f.groupKnown = false }, HTTP},
		{"not on our broker", func(g *rig) { g.devs.d.MQTT = model.MQTTUnknown }, HTTP},
		{"MQTT just went unanswered", func(g *rig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }, HTTP},
	} {
		for _, cmd := range []string{"Status 0", "status 00", "Backlog Power1 ON; Status 0"} {
			t.Run(tc.name+"/"+cmd, func(t *testing.T) {
				g := newRig(t)
				tc.setup(g)
				if got := g.r.TypedRoute(t.Context(), 7, cmd); got != tc.want {
					t.Fatalf("TypedRoute = %s, want %s", got, tc.want)
				}
				ctx, tr := WithTrace(WithRoute(WithConsole(t.Context()), tc.want))
				_, err := g.r.Command(ctx, g.target(), cmd)
				if tc.want == None {
					if err == nil || g.mq.n() != 0 || g.http.n() != 0 || tr.Via() != "none" {
						t.Fatalf("announced as going nowhere: err=%v mqtt=%d http=%d via=%s, want nothing sent", err, g.mq.n(), g.http.n(), tr.Via())
					}
					return
				}
				if err != nil || g.mq.n() != 0 || g.http.n() != 1 || tr.Via() != "http" {
					t.Fatalf("announced over HTTP: err=%v mqtt=%d http=%d via=%s, want one HTTP send", err, g.mq.n(), g.http.n(), tr.Via())
				}
			})
		}
	}
}
