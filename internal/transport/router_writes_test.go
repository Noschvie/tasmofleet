package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Why (#605): a device usable on our broker under the strict rule takes changes over MQTT first, while HTTP
// works too: console and settings writes, Restart, a Backlog. Exactly one send, and the trace (the audit's
// "via") names MQTT.
func TestStrictlyUsableWritesGoOverMQTTFirst(t *testing.T) {
	for _, c := range []string{"Power1 ON", "Restart 1", "TelePeriod 60", "Backlog SetOption19 1; SaveData 1"} {
		g := newRig(t)
		ctx, tr := WithTrace(t.Context())
		raw, err := g.r.Command(ctx, g.target(), c)
		if err != nil || string(raw) != `{"MQTT":"yes"}` || g.mq.n() != 1 || g.http.n() != 0 || tr.Via() != "mqtt" {
			t.Errorf("%q: raw=%s err=%v mqtt=%d http=%d via=%s", c, raw, err, g.mq.n(), g.http.n(), tr.Via())
		}
	}
}

// Why (#605, D4): a write MQTT provably did not publish (the channel refused it: an executor, a file command,
// Backlog2; the bus went down; the device's last will said Offline) changed nothing, so it is sent once over
// HTTP. Never back to MQTT within the call, also when HTTP did not send either.
func TestAWriteMQTTDidNotPublishIsSentOnceOverHTTP(t *testing.T) {
	for _, mqttErr := range []error{
		fmt.Errorf("%w: Rule1 can run other commands", mqttcmd.ErrRefused),
		fmt.Errorf("%w: not connected", tasmota.ErrUnreachable),
		fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable),
	} {
		g := newRig(t)
		g.mq.err = mqttErr
		ctx, tr := WithTrace(t.Context())
		raw, err := g.r.Command(ctx, g.target(), "Rule1 1")
		if err != nil || string(raw) != `{"HTTP":"yes"}` || g.mq.n() != 1 || g.http.n() != 1 || tr.Via() != "http" {
			t.Errorf("%v: raw=%s err=%v mqtt=%d http=%d via=%s", mqttErr, raw, err, g.mq.n(), g.http.n(), tr.Via())
		}
		g.http.err = tasmota.ErrForbiddenAddr // HTTP did not send either: no second MQTT attempt
		if _, err := g.r.Command(t.Context(), g.target(), "Rule1 1"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 2 || g.http.n() != 2 {
			t.Errorf("%v, HTTP not sent: err=%v mqtt=%d http=%d", mqttErr, err, g.mq.n(), g.http.n())
		}
	}
}

// Why (#605, D4): a write that MQTT published and the device did not confirm may have run. A second send over
// HTTP could switch a relay twice or restart the device twice, so the caller gets the unknown outcome
// (tasmota.ErrTimeout), also while HTTP works, and also when the caller gave up waiting.
func TestATimedOutMQTTWriteIsNeverResent(t *testing.T) {
	for _, mqttErr := range []error{
		fmt.Errorf("mqtt: no answer to Power1: %w", tasmota.ErrTimeout),
		fmt.Errorf("mqtt: the device went offline before it answered Restart (outcome unknown): %w", tasmota.ErrTimeout),
		fmt.Errorf("mqtt: Power1 was sent and not answered before the caller gave up (outcome unknown): %w: %w", tasmota.ErrTimeout, context.Canceled),
	} {
		g := newRig(t)
		g.mq.err = mqttErr
		ctx, tr := WithTrace(t.Context())
		_, err := g.r.Command(ctx, g.target(), "Power1 TOGGLE")
		if !errors.Is(err, tasmota.ErrTimeout) || g.mq.n() != 1 || g.http.n() != 0 || tr.Via() != "mqtt" {
			t.Errorf("%v: err=%v mqtt=%d http=%d via=%s", mqttErr, err, g.mq.n(), g.http.n(), tr.Via())
		}
	}
}

// Why (#605, the product owner's transport rule of 2026-11-14): HTTP is used only when an operation cannot be done
// over MQTT or MQTT is not usable for the device. Secrets are no reason: HTTP to a Tasmota is plain text too, the
// device password included. A change of the MQTT connection is no reason either: while HTTP works (it answers
// the router's one Status 11 right before, the only HTTP call), a device MQTT loses after the change is still
// reached over HTTP for the next command, so nothing asks first. Each of these real strings, from TasmoFleet's own
// code and typed in the console or a bulk command alike, is published once over MQTT while HTTP works, the trace
// (the audit's "via") says mqtt, and the reply of a command cmdpolicy keeps secret is still masked.
func TestWritesGoOverMQTTFirstWhateverTheyCarry(t *testing.T) {
	for _, tc := range []struct {
		cmd    string
		masked bool // cmdpolicy redacts it: the reply is scrubbed over MQTT as it was before
		cut    bool // it changes the MQTT connection: HTTP is asked one Status 11 first
	}{
		{"WebPassword x", true, false},
		{"Password1 x", true, false},
		{"MqttHost broker2", false, true},
		{"MqttPassword x", true, true},
		{"Topic newtopic", false, true},
		{"OtaUrl http://u:p@h/f", false, false},
		{"Dimmer 50", false, false},
		{"Backlog Power 1; WebPassword x", true, false},
		{"MqttUser x", false, true},
		{"FullTopic %prefix%/x/", false, true},
		{"SetOption3 0", false, true},
		{"Backlog Power ON; MqttHost 10.0.0.9", false, true},
		{"SSId1 home", true, false},
		{"RgxPassword x", true, false},
		{"TmToken abc", true, false},
		{"MI32Key 00112233445566778899AABBCCDDEEFFA4C138AABBCC", true, false},
		{"Sendmail [smtp.x:465:u:pw:a@b:c@d:s] body", false, false},
		{"BLEPair A4C138AABBCC 123456", false, false},
		{`Template {"NAME":"Plug","GPIO":[0,0,0,0,0,0,0,0,0,0,0,0,0,0],"FLAG":0,"BASE":18}`, false, false},
		{`Backlog Template {"NAME":"Plug","GPIO":[0],"FLAG":0,"BASE":18}; Module 0`, false, false},
	} {
		for _, caller := range []string{"TasmoFleet's own", "console"} {
			g := newRig(t)
			g.f.httpOK = true
			g.mq.reply = `{"WebPassword":"x","Password1":"x","MqttPassword":"x","SSId1":"x","RgxPassword":"x","TmToken":"x","MIKeys":"x"}`
			ctx := t.Context()
			if caller == "console" {
				ctx = WithConsole(ctx)
			}
			ctx, tr := WithTrace(ctx)
			raw, err := g.r.Command(ctx, g.target(), tc.cmd)
			probes := 0
			if tc.cut {
				probes = 1
			}
			if err != nil || g.mq.n() != 1 || g.http.n() != probes || (probes == 1 && g.http.calls[0] != "Status 11") || tr.Via() != "mqtt" {
				t.Errorf("%q (%s), HTTP works: err=%v mqtt=%d http=%v via=%s, want one publish over MQTT", tc.cmd, caller, err, g.mq.n(), g.http.calls, tr.Via())
				continue
			}
			if tc.masked && strings.Contains(string(raw), `"x"`) {
				t.Errorf("%q (%s): the reply shows the secret: %s", tc.cmd, caller, raw)
			}
		}
	}
	// The lockout question belongs to a device that HTTP does not reach: there a change of the MQTT connection is
	// published only with the console's confirmation (T10), and nothing else asks.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "Topic newtopic"); !errors.Is(err, ErrCutOff) || g.mq.n() != 0 {
		t.Fatalf("HTTP failing, no confirmation: err=%v mqtt=%d, want a refusal that published nothing", err, g.mq.n())
	}
	if _, err := g.r.Command(WithCutOffConsent(WithConsole(t.Context())), g.target(), "Topic newtopic"); err != nil || g.mq.n() != 1 {
		t.Fatalf("HTTP failing, confirmed: err=%v mqtt=%d", err, g.mq.n())
	}
}

// Why (#605 review, the user's condition "sofern parallel noch HTTP funktioniert"): a change of the device's MQTT
// connection goes out without a question only while HTTP is known to work. "Not known to fail" is not enough: the
// normal state of a device that is live on MQTT is unknown (nothing polls its HTTP, a timeout is never recorded as a
// failure), and a firewalled device or one with WebServer 0 stays unknown for ever. Published there, MqttHost broker2
// would take the device off the broker with no HTTP to reach it again. So while HTTP is unknown the router asks HTTP
// one harmless question (Status 11) first: an answer proves HTTP usable and the change is published; no answer means
// the change needs the person's confirmation (ErrCutOff, nothing published, nothing sent over HTTP). A confirmed
// change and every other write never wait for the question.
func TestACutOffWriteGoesWithoutAQuestionOnlyWhileHTTPIsProvenUsable(t *testing.T) {
	cutOff := []string{"MqttHost broker2", "MqttPassword typo", "Topic newtopic", "SetOption3 0", "Backlog Power ON; MqttHost 10.0.0.9"}
	others := []string{"WebPassword x", "Password1 x", "OtaUrl http://u:p@h/f", "Dimmer 50", "Backlog Power 1; WebPassword x"}
	for _, probeErr := range []error{tasmota.ErrUnreachable, tasmota.ErrTimeout} {
		for _, cmd := range cutOff {
			g := newRig(t)
			g.http.err = probeErr
			ctx, tr := WithTrace(WithConsole(t.Context()))
			if _, err := g.r.Command(ctx, g.target(), cmd); !errors.Is(err, ErrCutOff) || g.mq.n() != 0 || g.http.n() != 1 || tr.Via() != "none" {
				t.Errorf("%q, HTTP unknown and silent (%v): err=%v mqtt=%d http=%d via=%s, want ErrCutOff after one probe, nothing published",
					cmd, probeErr, err, g.mq.n(), g.http.n(), tr.Via())
			}
			if n := g.http.n(); n == 1 && g.http.calls[0] != "Status 11" {
				t.Errorf("%q: HTTP got %q, want only the probe Status 11 (the change itself never goes over HTTP here)", cmd, g.http.calls[0])
			}
			g = newRig(t)
			g.http.err = probeErr
			if _, err := g.r.Command(WithCutOffConsent(WithConsole(t.Context())), g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
				t.Errorf("%q, HTTP unknown, confirmed: err=%v mqtt=%d http=%d, want one publish and no probe", cmd, err, g.mq.n(), g.http.n())
			}
		}
		for _, cmd := range others {
			g := newRig(t)
			g.http.err = probeErr
			if _, err := g.r.Command(WithConsole(t.Context()), g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
				t.Errorf("%q, HTTP unknown and silent: err=%v mqtt=%d http=%d, want one publish and no probe", cmd, err, g.mq.n(), g.http.n())
			}
		}
	}
	for _, cmd := range cutOff {
		g := newRig(t) // HTTP unknown, but it answers the probe
		ctx, tr := WithTrace(WithConsole(t.Context()))
		if _, err := g.r.Command(ctx, g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.n() != 1 || g.http.calls[0] != "Status 11" || tr.Via() != "mqtt" {
			t.Errorf("%q, HTTP unknown, probe answered: err=%v mqtt=%d http=%v via=%s, want the probe, then one publish (via mqtt)",
				cmd, err, g.mq.n(), g.http.calls, tr.Via())
		}
		// A fresh answer on record is no proof: HTTP may have stopped since (the person's own WebServer 0 a minute
		// ago). The router asks HTTP right now, and a silent HTTP means the question, as for an unknown one.
		g = newRig(t)
		g.f.httpOK = true
		g.http.err = tasmota.ErrUnreachable
		ctx, tr = WithTrace(WithConsole(t.Context()))
		if _, err := g.r.Command(ctx, g.target(), cmd); !errors.Is(err, ErrCutOff) || g.mq.n() != 0 || g.http.n() != 1 || g.http.calls[0] != "Status 11" || tr.Via() != "none" {
			t.Errorf("%q, HTTP ok on record but silent now: err=%v mqtt=%d http=%v via=%s, want ErrCutOff after one probe, nothing published",
				cmd, err, g.mq.n(), g.http.calls, tr.Via())
		}
	}
}

// Why (#605 review, round 3): the person's own earlier change can close HTTP. The console's HTTP probe on opening
// puts a fresh answer on record, WebServer 0 then goes over MQTT (no change of the MQTT connection, no probe), and
// HTTP stops answering. A following MqttHost broker2 must not trust the answer on record: published, it would take
// the device off the broker with HTTP already closed, and TasmoFleet would have lost it. The router asks HTTP
// right then; the silence refuses the change (ErrCutOff, nothing published) and is remembered as a failure, so the
// console asks before the next try.
func TestACutOffWriteAfterHTTPClosedIsRefusedDespiteAnAnswerOnRecord(t *testing.T) {
	g := newRig(t)
	g.f.httpOK = true // the console's probe answered a minute ago
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "WebServer 0"); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
		t.Fatalf("WebServer 0: err=%v mqtt=%d http=%d, want one publish", err, g.mq.n(), g.http.n())
	}
	g.http.err = tasmota.ErrUnreachable // the web server is off now
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "MqttHost broker2"); !errors.Is(err, ErrCutOff) || g.mq.n() != 1 {
		t.Fatalf("MqttHost after WebServer 0: err=%v mqtt=%d, want ErrCutOff and nothing more published", err, g.mq.n())
	}
	if g.http.n() != 1 || g.http.calls[0] != "Status 11" {
		t.Fatalf("HTTP got %v, want only the probe Status 11", g.http.calls)
	}
	if _, _, failing := g.mem.HTTPFailure(7); !failing {
		t.Fatal("the probe's silence is not remembered: the console would not ask before the next try")
	}
}

// Why (#605 review, round 3): a bulk command or a settings save carries no consent, so the router decides per
// device whether a change of the MQTT connection may go: one publish where HTTP answers right now, a refusal that
// sent nothing where it does not. The bulk preview no longer refuses up front for a device whose HTTP is merely not
// known to work; this is what keeps such a device safe instead.
func TestACutOffWriteWithoutConsentDependsOnHTTPAnsweringNow(t *testing.T) {
	for _, cmd := range []string{"MqttHost broker2", "MqttPort 8883", "GroupTopic fleet", "SetOption103 1"} {
		g := newRig(t) // HTTP unknown, answers
		if _, err := g.r.Command(t.Context(), g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.n() != 1 {
			t.Errorf("%q, HTTP answers: err=%v mqtt=%d http=%d, want the probe and one publish", cmd, err, g.mq.n(), g.http.n())
		}
		g = newRig(t)
		g.http.err = tasmota.ErrUnreachable
		if _, err := g.r.Command(t.Context(), g.target(), cmd); !errors.Is(err, ErrCutOff) || g.mq.n() != 0 || g.http.n() != 1 {
			t.Errorf("%q, HTTP silent: err=%v mqtt=%d http=%d, want ErrCutOff after the probe, nothing published", cmd, err, g.mq.n(), g.http.n())
		}
	}
}

// Why (#605 review, round 4; the product owner's rule "HTTP stays the fallback for the next commands"): a change of
// the MQTT connection goes without a question only because HTTP carries the commands that follow. Tasmota restarts
// after MqttPassword, MqttUser, Topic, SetOption3 and the like through a clean DISCONNECT, so the broker drops the
// will and the retained last will stays Online; a device that cannot rejoin TasmoFleet's broker (a typo in the
// password) still looks usable on MQTT. Without a mark every following command would be probed or published into
// the void and end as an unknown outcome, never resent over HTTP, the person's corrective MqttPassword included:
// the lockout the rule forbids while HTTP works. So after such a change was published, writes and reads go over
// HTTP while HTTP is not failing.
func TestAfterAChangeOfTheMQTTConnectionHTTPCarriesTheNextCommands(t *testing.T) {
	g := newRig(t) // HTTP unknown, answers the router's probe
	ctx, tr := WithTrace(WithConsole(t.Context()))
	if _, err := g.r.Command(ctx, g.target(), "MqttPassword typo"); err != nil || g.mq.n() != 1 || g.http.n() != 1 || g.http.calls[0] != "Status 11" || tr.Via() != "mqtt" {
		t.Fatalf("MqttPassword typo: err=%v mqtt=%d http=%v via=%s, want the probe, then one publish", err, g.mq.n(), g.http.calls, tr.Via())
	}
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout) // the device restarted and cannot rejoin the broker
	for _, cmd := range []string{"MqttPassword right", "SetOption3 1", "Power1 ON", "Status 5"} {
		ctx, tr := WithTrace(WithConsole(t.Context()))
		raw, err := g.r.Command(ctx, g.target(), cmd)
		last := g.http.calls[len(g.http.calls)-1]
		if err != nil || string(raw) != `{"HTTP":"yes"}` || last != cmd || g.mq.n() != 1 || tr.Via() != "http" {
			t.Errorf("%q after the change: raw=%s err=%v last HTTP call %q mqtt=%d via=%s, want it over HTTP and nothing more published",
				cmd, raw, err, last, g.mq.n(), tr.Via())
		}
	}
}

// Why (#605 review, round 4): the mark exists because HTTP carries the next commands. A change the person confirmed
// on a device HTTP was not proven to reach (the console's confirmation, no probe) sets no mark: its next commands
// stay on MQTT, the only way that may still work. An answer over MQTT proves the device is back on the broker and
// clears the mark, so MQTT is first again at once instead of after ten minutes.
func TestTheMarkAfterAChangeOfTheMQTTConnectionNeedsHTTPAndEndsWithAnMQTTAnswer(t *testing.T) {
	g := newRig(t)
	if _, err := g.r.Command(WithCutOffConsent(WithConsole(t.Context())), g.target(), "MqttHost broker2"); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
		t.Fatalf("confirmed MqttHost: err=%v mqtt=%d http=%d, want one publish and no probe", err, g.mq.n(), g.http.n())
	}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 2 || g.http.n() != 0 {
		t.Fatalf("Power1 ON after a confirmed change: err=%v mqtt=%d http=%d, want it over MQTT", err, g.mq.n(), g.http.n())
	}

	g = newRig(t)
	if _, err := g.r.Command(t.Context(), g.target(), "Topic newtopic"); err != nil || g.mq.n() != 1 || g.http.n() != 1 {
		t.Fatalf("Topic newtopic: err=%v mqtt=%d http=%d, want the probe and one publish", err, g.mq.n(), g.http.n())
	}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 1 || g.http.n() != 2 {
		t.Fatalf("Power1 ON right after the change: err=%v mqtt=%d http=%d, want it over HTTP", err, g.mq.n(), g.http.n())
	}
	if _, err := g.r.Command(WithPath(t.Context(), MQTT), g.target(), "Status 5"); err != nil || g.mq.n() != 2 {
		t.Fatalf("pinned read: err=%v mqtt=%d, want one publish with an answer", err, g.mq.n())
	}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 3 || g.http.n() != 2 {
		t.Fatalf("Power1 ON after an MQTT answer: err=%v mqtt=%d http=%d, want it over MQTT again", err, g.mq.n(), g.http.n())
	}
}

// Why (#605 security review): the reply over MQTT of a command whose name is not secret but whose text carries a
// credential is returned as the device sent it, as before. The OTA engine sets OtaUrl and checks
// that the device stored exactly the URL it sent (ota.deviceAPI.SetOtaURL); a masked echo of a URL with a login
// would fail every update and every put-back of such a URL that goes over MQTT (a pinned job, HTTP failing). The
// console masks what it shows on its own (devicecmd).
func TestACredentialInTheTextLeavesTheReplyAsSent(t *testing.T) {
	const url = "http://u:p@h/fw.bin"
	g := newRig(t)
	g.mq.reply = `{"OtaUrl":"` + url + `"}`
	raw, err := g.r.Command(WithPath(t.Context(), MQTT), g.target(), "OtaUrl "+url)
	if err != nil || g.mq.n() != 1 || !strings.Contains(string(raw), url) {
		t.Fatalf("pinned to MQTT: raw=%s err=%v mqtt=%d", raw, err, g.mq.n())
	}
	g.mem.Fail(7, HTTPFailUnreachable)
	raw, err = g.r.Command(t.Context(), g.target(), "OtaUrl "+url)
	if err != nil || g.mq.n() != 2 || !strings.Contains(string(raw), url) {
		t.Fatalf("HTTP failing: raw=%s err=%v mqtt=%d", raw, err, g.mq.n())
	}
}

// Why (#605, review #279): a write to a topic that might be shared would change several devices, and a device
// whose MQTT reads go unanswered would leave every write an unknown outcome. Both keep HTTP while it works.
func TestWritesThatMQTTMustNotCarryFirstGoOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(g *rig)
	}{
		{"own GroupTopic unread", func(g *rig) { g.f.groupKnown = false }},
		{"another device's GroupTopic unread", func(g *rig) { g.f.othersUnrd = []string{"garage"} }},
		{"MQTT reads unanswered", func(g *rig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }},
		{"last will Offline", func(g *rig) { g.f.lwtOffline = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			tc.mod(g)
			ctx, tr := WithTrace(t.Context())
			if _, err := g.r.Command(ctx, g.target(), "Power1 ON"); err != nil || g.http.n() != 1 || g.mq.n() != 0 || tr.Via() != "http" {
				t.Fatalf("err=%v http=%d mqtt=%d via=%s", err, g.http.n(), g.mq.n(), tr.Via())
			}
		})
	}
}

// Why: the device page shows Choose's writes path as what a change will do; the router must do exactly that for
// the same facts, or the card promises MQTT while the change goes over HTTP (or the other way round).
func TestRouterWritesFollowChoose(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(g *rig)
	}{
		{"strict usable", func(*rig) {}},
		{"own GroupTopic unread", func(g *rig) { g.f.groupKnown = false }},
		{"others unread", func(g *rig) { g.f.othersUnrd = []string{"garage", "shed"} }},
		{"reads unanswered", func(g *rig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }},
		{"offline on MQTT", func(g *rig) { g.f.lwtOffline = true }},
		{"HTTP failing", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable) }},
		{"HTTP failing, reads unanswered", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable); g.r.unanswered.Fail(7, HTTPFailUnreachable) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			tc.mod(g)
			f, err := g.f.FactsFor(t.Context(), g.devs.d)
			if err != nil {
				t.Fatal(err)
			}
			want := Choose(f).Writes
			if _, err := g.r.Command(t.Context(), g.target(), "TelePeriod 60"); err != nil {
				t.Fatal(err)
			}
			got := HTTP
			if g.mq.n() > 0 {
				got = MQTT
			}
			if got != want || g.mq.n()+g.http.n() != 1 {
				t.Fatalf("router used %s (mqtt=%d http=%d), Choose says %s", got, g.mq.n(), g.http.n(), want)
			}
		})
	}
}

// Why: the MQTT-first write goes through the same send as every other MQTT call, so the topic is checked
// again right before the publish (T6): a topic changed since the snapshot publishes nothing, and the write
// is then sent over HTTP (nothing was published).
func TestMQTTFirstWriteRechecksTheTopicBeforeThePublish(t *testing.T) {
	g := newRig(t)
	g.r.o.Facts = changingFacts{inner: g.f, devs: g.devs}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
		t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
}

// Why: without facts the router cannot check that the topic is the device's own; a write then takes the
// established HTTP path and publishes nothing (fail closed). A device that is not on our broker is changed over
// HTTP without the inventory being listed for its facts (every switch of the device list would pay for it).
func TestWritesWithoutFactsOrOffOurBrokerGoOverHTTP(t *testing.T) {
	g := newRig(t)
	g.r.o.Facts = brokenFacts{g.f}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
		t.Fatalf("without facts: err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
	for _, mod := range []func(*rig){
		func(g *rig) { g.devs.d.MQTT = model.MQTTOtherBroker },
		func(g *rig) { g.devs.d.MQTT = model.MQTTNone },
		func(g *rig) { g.devs.d.Topic = "" },
	} {
		g := newRig(t)
		mod(g)
		if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 0 || g.http.n() != 1 || g.f.snapshotted != 0 {
			t.Fatalf("%+v: err=%v mqtt=%d http=%d facts asked %d times", g.devs.d, err, g.mq.n(), g.http.n(), g.f.snapshotted)
		}
	}
}

// Why (#605): the power toggle calls the command channel itself and asks this whether it may switch over HTTP
// after MQTT. Only a proof that nothing was published counts; a timeout (an unknown outcome) never does.
func TestNotPublishedCountsOnlyProofsThatNothingWasSent(t *testing.T) {
	for err, want := range map[error]bool{
		nil: false,
		fmt.Errorf("%w: refused", mqttcmd.ErrRefused):                                               true,
		fmt.Errorf("%w: not connected", tasmota.ErrUnreachable):                                     true,
		fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout):                                       false,
		fmt.Errorf("%w: Power1 may have been sent: %w", tasmota.ErrTimeout, tasmota.ErrUnreachable): false,
		errors.New("something else"):                                                                false,
	} {
		if got := NotPublished(err); got != want {
			t.Errorf("NotPublished(%v) = %v, want %v", err, got, want)
		}
	}
}

// Why (#605 review, the coordinator's decision of 2026-11-14 17:15): the device runs MqttTLS as SetOption103 and
// MqttTLSFingerprint as SetOption132, in any case and with any index, alone or in a Backlog: each changes how the
// device connects to the broker. The router must treat each as the change of the MQTT connection it is: with HTTP
// failing, no publish without the person's confirmation (ErrCutOff); with HTTP answering, one Status 11 first, then
// one publish, and the mark that sends the next commands over HTTP. Read by its own name, MqttTLS 1 went out with
// no question and no mark, and a device that then could not rejoin the broker was lost to every following command.
func TestACutOffWriteUnderASetOptionSynonymIsJudgedLikeItsSetOption(t *testing.T) {
	for _, cmd := range []string{"MqttTLS 1", "mqtttls1 1", "MqttTLSFingerprint 0", "Backlog Power ON; MqttTLS 1"} {
		g := newRig(t)
		g.mem.Fail(7, HTTPFailUnreachable)
		g.http.err = tasmota.ErrUnreachable
		if _, err := g.r.Command(t.Context(), g.target(), cmd); !errors.Is(err, ErrCutOff) || g.mq.n() != 0 {
			t.Errorf("%q, HTTP failing: err=%v mqtt=%d, want ErrCutOff and nothing published", cmd, err, g.mq.n())
		}

		g = newRig(t)
		if _, err := g.r.Command(t.Context(), g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.n() != 1 || g.http.calls[0] != "Status 11" {
			t.Errorf("%q, HTTP answers: err=%v mqtt=%d http=%v, want exactly one Status 11, then one publish", cmd, err, g.mq.n(), g.http.calls)
		}
		if _, _, marked := g.r.MQTTConnChanged().HTTPFailure(7); !marked {
			t.Errorf("%q, HTTP answers: no mark, so the next commands would go MQTT first into the void", cmd)
		}
	}
}

// Why (#605 review, round 5): the mark exists because the device may not rejoin the broker after a change of its MQTT
// connection, and that holds whichever transport carried the change. Over HTTP the change reaches the device just the
// same: the MQTT channel refused the Backlog (it runs other commands) and HTTP carried it once; the console sent a
// corrective change over HTTP because the device's MQTT reads went unanswered; Make compliant sent it over HTTP. Each
// time the device is marked, so its next commands go over HTTP and nothing is published into the void.
func TestAChangeOfTheMQTTConnectionThatHTTPCarriedMarksTheDevice(t *testing.T) {
	g := newRig(t)
	g.mq.err = fmt.Errorf("%w: Json can run other commands", mqttcmd.ErrRefused)
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), `Backlog MqttPassword typo; Json {"Power":1}`); err != nil || g.mq.n() != 1 ||
		g.http.n() != 2 || g.http.calls[1] != `Backlog MqttPassword typo; Json {"Power":1}` {
		t.Fatalf("refused Backlog: err=%v mqtt=%d http=%v, want the probe, the refusal and the Backlog over HTTP", err, g.mq.n(), g.http.calls)
	}
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout) // the device cannot rejoin the broker
	for _, cmd := range []string{"MqttPassword right", "Power1 ON"} {
		ctx, tr := WithTrace(WithConsole(t.Context()))
		if _, err := g.r.Command(ctx, g.target(), cmd); err != nil || g.mq.n() != 1 || g.http.calls[len(g.http.calls)-1] != cmd || tr.Via() != "http" {
			t.Errorf("%q after the refused Backlog: err=%v mqtt=%d http=%v via=%s, want it over HTTP and nothing more published",
				cmd, err, g.mq.n(), g.http.calls, tr.Via())
		}
	}

	// A corrective change sent over HTTP because Choose said HTTP (here: the device's MQTT reads went unanswered).
	g = newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "MqttPassword right"); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
		t.Fatalf("MqttPassword over HTTP first: err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
	if _, _, marked := g.r.MQTTConnChanged().HTTPFailure(7); !marked {
		t.Error("a change of the MQTT connection HTTP carried did not mark the device")
	}

	// A write that starts on HTTP (the device's MQTT reads went unanswered), HTTP timing out: the change may have run.
	g = newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable)
	g.http.err = fmt.Errorf("http: %w", tasmota.ErrTimeout)
	if _, err := g.r.Command(t.Context(), g.target(), "Topic newtopic"); !errors.Is(err, tasmota.ErrTimeout) || g.mq.n() != 0 {
		t.Fatalf("Topic over HTTP first: err=%v mqtt=%d", err, g.mq.n())
	}
	if _, _, marked := g.r.MQTTConnChanged().HTTPFailure(7); !marked {
		t.Error("a change of the MQTT connection that timed out over HTTP did not mark the device")
	}

	// Nothing reached the device over HTTP, or the change is no change of the MQTT connection: no mark.
	for _, tc := range []struct {
		cmd string
		err error
	}{{"Topic newtopic", tasmota.ErrForbiddenAddr}, {"TelePeriod 60", nil}} {
		g = newRig(t)
		g.r.unanswered.Fail(7, HTTPFailUnreachable) // the write starts on HTTP
		g.http.err = tc.err
		_, _ = g.r.Command(t.Context(), g.target(), tc.cmd) // only the mark is asked here
		if _, _, marked := g.r.MQTTConnChanged().HTTPFailure(7); marked {
			t.Errorf("%q, HTTP %v: marked, want no mark", tc.cmd, tc.err)
		}
	}
}

// Why (#605 review, round 5): a write MQTT published and the device never confirmed is an unknown outcome and is never
// resent; but it also says the device's replies are not where TasmoFleet listens, as an unanswered read does. Unmarked,
// every following relay toggle and console write of such a device (its last will still Online after the mark of a
// change expired) would again end as an unknown outcome while HTTP works. So the device is marked like after an
// unanswered read, and its next commands go over HTTP. A caller that gave up waiting, or HTTP failing, marks nothing.
func TestAWriteMQTTLeftUnansweredSendsTheNextCommandsOverHTTP(t *testing.T) {
	g := newRig(t)
	g.mq.err = fmt.Errorf("mqtt: no answer to Power1: %w", tasmota.ErrTimeout)
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); !errors.Is(err, tasmota.ErrTimeout) || g.mq.n() != 1 || g.http.n() != 0 {
		t.Fatalf("Power1 ON: err=%v mqtt=%d http=%d, want the unknown outcome and no resend", err, g.mq.n(), g.http.n())
	}
	ctx, tr := WithTrace(t.Context())
	if _, err := g.r.Command(ctx, g.target(), "Power1 OFF"); err != nil || g.mq.n() != 1 || g.http.n() != 1 || tr.Via() != "http" {
		t.Fatalf("Power1 OFF after the unanswered write: err=%v mqtt=%d http=%d via=%s, want it over HTTP", err, g.mq.n(), g.http.n(), tr.Via())
	}

	gaveUp, cancel := context.WithCancel(t.Context())
	g = newRig(t)
	g.mq.onDo = cancel
	g.mq.err = fmt.Errorf("mqtt: Power1 was not answered before the caller gave up: %w: %w", tasmota.ErrTimeout, context.Canceled)
	_, _ = g.r.Command(gaveUp, g.target(), "Power1 ON") // only the mark is asked here
	if _, _, marked := g.r.unanswered.HTTPFailure(7); marked {
		t.Error("a caller that gave up waiting marked the device")
	}
}
