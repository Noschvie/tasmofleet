package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

func TestReadGoesOverMQTTFirstEvenWhenHTTPWorks(t *testing.T) {
	// Why (#456 A): reads of a device on our broker that is online answer over MQTT, so a device that
	// HTTP cannot reach answers them and HTTP is not loaded with a poll per read.
	g := newRig(t)
	ctx, tr := WithTrace(t.Context())
	raw, err := g.r.Command(ctx, g.target(), "Status 2")
	if err != nil || string(raw) != `{"MQTT":"yes"}` || g.mq.n() != 1 || g.http.n() != 0 || tr.Via() != "mqtt" {
		t.Fatalf("raw=%s err=%v mqtt=%d http=%d via=%s", raw, err, g.mq.n(), g.http.n(), tr.Via())
	}
}

func TestReadUsesHTTPWhenMQTTIsNotUsable(t *testing.T) {
	// Why: a device off our broker, offline on MQTT or without a topic is read the way it always was.
	for _, tc := range []struct {
		name string
		mod  func(g *rig)
	}{
		{"other broker", func(g *rig) { g.devs.d.MQTT = model.MQTTOtherBroker }},
		{"MQTT none", func(g *rig) { g.devs.d.MQTT = model.MQTTNone }},
		{"no topic", func(g *rig) { g.devs.d.Topic = "" }},
		{"LWT offline", func(g *rig) { g.f.lwtOffline = true }},
		{"bus down", func(g *rig) { g.mq.down = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			tc.mod(g)
			if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.http.n() != 1 || g.mq.n() != 0 {
				t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
			}
		})
	}
}

func TestReadFailsOverToHTTPOnceWhenMQTTGaveNoAnswer(t *testing.T) {
	// Why (#456 A, review of PR 626): reads change nothing on the device, so a read that MQTT
	// published and the device never answered (SetOption4 1 devices answer on a topic we do not decode,
	// a broker restart, a dropped QoS 0 publish) is asked once over HTTP instead of failing for good.
	// Only a timeout counts as "no answer"; an answer we cannot use is an answer and is not repeated.
	for _, tc := range []struct {
		name     string
		err      error
		failover bool
		marked   bool
	}{
		{"refused by the channel", fmt.Errorf("%w: Status 0 is never sent", mqttcmd.ErrRefused), true, false},
		{"could not publish", fmt.Errorf("%w: Status 2 not sent", tasmota.ErrUnreachable), true, false},
		{"published, no answer", fmt.Errorf("mqtt: no answer to Status 2: %w", tasmota.ErrTimeout), true, true},
		{"published, device went offline", fmt.Errorf("mqtt: offline before it answered (outcome unknown): %w", tasmota.ErrTimeout), true, true},
		{"published, reply unusable", fmt.Errorf("%w: reply", tasmota.ErrBadResponse), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.mq.err = tc.err
			ctx, tr := WithTrace(t.Context())
			raw, err := g.r.Command(ctx, g.target(), "Status 2")
			if tc.failover {
				if err != nil || string(raw) != `{"HTTP":"yes"}` || g.http.n() != 1 || g.mq.n() != 1 {
					t.Fatalf("raw=%s err=%v http=%d mqtt=%d", raw, err, g.http.n(), g.mq.n())
				}
			} else if !errors.Is(err, tasmota.ErrBadResponse) || g.http.n() != 0 || g.mq.n() != 1 {
				t.Fatalf("err=%v http=%d mqtt=%d: an answer we cannot use is not asked again", err, g.http.n(), g.mq.n())
			}
			if _, _, marked := g.r.unanswered.HTTPFailure(7); marked != tc.marked {
				t.Errorf("device marked as leaving MQTT reads unanswered = %v, want %v", marked, tc.marked)
			}
			if tc.marked && tr.Via() != "http+mqtt" {
				t.Errorf("via = %s: both transports carried the read", tr.Via())
			}
		})
	}
}

func TestATraceListsOnlyTheTransportsThatAnswered(t *testing.T) {
	// Why (#810): the identity check after a read vouches for the transports the read's values came from. A read
	// MQTT published and nobody answered brought no value over MQTT (the read went on over HTTP), so asking the
	// check there too would wait out a second timeout and refuse a read the stored device answered at its address.
	// Via keeps counting that call: for the audit a write left unanswered may have run.
	for _, tc := range []struct {
		name     string
		err      error
		via      string
		answered []Path
	}{
		{"answered over MQTT", nil, "mqtt", []Path{MQTT}},
		{"published, no answer", fmt.Errorf("mqtt: no answer to Status 2: %w", tasmota.ErrTimeout), "http+mqtt", []Path{HTTP}},
		{"could not publish", fmt.Errorf("%w: Status 2 not sent", tasmota.ErrUnreachable), "http", []Path{HTTP}},
		{"published, reply unusable", fmt.Errorf("%w: reply", tasmota.ErrBadResponse), "mqtt", []Path{MQTT}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.mq.err = tc.err
			ctx, tr := WithTrace(t.Context())
			_, _ = g.r.Command(ctx, g.target(), "Status 2") // the outcome is not the point: the trace is
			if tr.Via() != tc.via || fmt.Sprint(tr.Answered()) != fmt.Sprint(tc.answered) {
				t.Errorf("via=%s answered=%v, want via=%s answered=%v", tr.Via(), tr.Answered(), tc.via, tc.answered)
			}
		})
	}
	empty, none := &Trace{}, (*Trace)(nil)
	if a := empty.Answered(); len(a) != 0 {
		t.Errorf("a trace that recorded nothing answered over %v", a)
	}
	if a := none.Answered(); len(a) != 0 { // like Via, safe on no trace at all
		t.Errorf("no trace answered over %v", a)
	}
}

func TestNextReadAfterAnUnansweredOneIsNotPublished(t *testing.T) {
	// Why: a device that leaves MQTT reads unanswered would cost 8 s (16 s with the late-reply hold)
	// per read; the mark sends its reads over HTTP for 10 minutes while HTTP works.
	g := newRig(t)
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil {
		t.Fatal(err)
	}
	g.mq.err = nil
	if raw, err := g.r.Command(t.Context(), g.target(), "Status 3"); err != nil || string(raw) != `{"HTTP":"yes"}` || g.mq.n() != 1 || g.http.n() != 2 {
		t.Fatalf("raw=%s err=%v mqtt=%d http=%d", raw, err, g.mq.n(), g.http.n())
	}
	// the mark runs out: MQTT is tried again, and an answer clears it
	now := time.Now()
	g.r.unanswered = NewMemory(DefaultFailFor, func() time.Time { return now })
	g.f.unanswered = g.r.unanswered
	g.r.unanswered.Fail(7, HTTPFailUnreachable)
	now = now.Add(DefaultFailFor + time.Second)
	if raw, err := g.r.Command(t.Context(), g.target(), "Status 3"); err != nil || string(raw) != `{"MQTT":"yes"}` || g.mq.n() != 2 {
		t.Fatalf("after the mark ran out: raw=%s err=%v mqtt=%d", raw, err, g.mq.n())
	}
}

func TestAnAnsweredMQTTReadClearsTheMark(t *testing.T) {
	g := newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable)
	g.f.unanswered = nil // facts do not see the mark: MQTT is tried and answers
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil {
		t.Fatal(err)
	}
	if _, _, marked := g.r.unanswered.HTTPFailure(7); marked {
		t.Error("an answered read must clear the mark")
	}
}

func TestUnansweredReadIsNotSentOverHTTPWhenTheCallerGaveUp(t *testing.T) {
	// Why: a read the caller no longer waits for must not start another request on the device.
	g := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	g.mq.err = fmt.Errorf("mqtt: sent and not answered before the caller gave up: %w: %w", tasmota.ErrTimeout, context.Canceled)
	g.mq.onDo = cancel
	if _, err := g.r.Command(ctx, g.target(), "Status 2"); !errors.Is(err, tasmota.ErrTimeout) || g.http.n() != 0 {
		t.Fatalf("err=%v http=%d", err, g.http.n())
	}
}

func TestUnansweredReadIsNotSentOverHTTPWhenHTTPIsFailing(t *testing.T) {
	// Why: with HTTP known to fail there is nothing to fall back to; the caller sees the timeout.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrTimeout) || g.http.n() != 0 {
		t.Fatalf("err=%v http=%d", err, g.http.n())
	}
	if _, _, marked := g.r.unanswered.HTTPFailure(7); marked {
		t.Error("a device marked while HTTP fails would be read over a failing HTTP afterwards")
	}
}

func TestMarkedDeviceIsStillReadOverMQTTWhenHTTPFails(t *testing.T) {
	g := newRig(t)
	g.r.unanswered.Fail(7, HTTPFailUnreachable)
	g.mem.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
		t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
}

func TestRepliesMQTTCannotMatchAreReadOverHTTPFirst(t *testing.T) {
	// Why: State, a bare Gpio, Gpios and Modules answer with keys the channel does not match; one
	// console State would otherwise mark the device and move all its reads to HTTP for 10 minutes.
	for _, c := range []string{"State", "Gpio", "Gpios", "Modules"} {
		g := newRig(t)
		if _, err := g.r.Command(WithConsole(t.Context()), g.target(), c); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
			t.Errorf("%s: err=%v mqtt=%d http=%d", c, err, g.mq.n(), g.http.n())
		}
	}
}

func TestFailoverReadIsSentOnceOverHTTPAndNotBackToMQTT(t *testing.T) {
	// Why: the fall back to HTTP after a refusal or an unanswered read must not bounce to MQTT again when
	// HTTP did not send either (that would publish the read a second time).
	for _, mqttErr := range []error{
		fmt.Errorf("%w: refused", mqttcmd.ErrRefused),
		fmt.Errorf("mqtt: no answer to Status 2: %w", tasmota.ErrTimeout),
	} {
		g := newRig(t)
		g.mq.err = mqttErr
		g.http.err = tasmota.ErrForbiddenAddr
		if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 1 || g.http.n() != 1 {
			t.Fatalf("%v: err=%v mqtt=%d http=%d", mqttErr, err, g.mq.n(), g.http.n())
		}
	}
}

func TestReadFailsOverToHTTPWhenTheSettingsChangedMeanwhile(t *testing.T) {
	// Why: the snapshot check refuses before publishing ("nothing was sent"), which is a proof of
	// not-published like any other refusal: the read may take the other path.
	g := newRig(t)
	g.r.o.Facts = changingFacts{inner: g.f, devs: g.devs}
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
		t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
}

func TestPinnedHTTPReadStaysHTTP(t *testing.T) {
	// Why (D5): a job that pinned HTTP is never rerouted, reads included.
	g := newRig(t)
	if _, err := g.r.Command(WithPath(t.Context(), HTTP), g.target(), "Status 2"); err != nil || g.http.n() != 1 || g.mq.n() != 0 {
		t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
	}
}

func TestJobCommandsAreNeverMQTTFirst(t *testing.T) {
	// Why: Upgrade/WebGetConfig with an argument start a job with its own safety checks.
	g := newRig(t)
	if _, err := g.r.Command(t.Context(), g.target(), "Upgrade 1"); err != nil || g.mq.n() != 0 || g.http.n() != 1 {
		t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
}

func TestStatus0IsNeverPublishedByTheRouter(t *testing.T) {
	// Why (CLAUDE.md safety rule 2): Status 0 is not a read (cmdpolicy asks a person to confirm it). Since #605
	// writes go over MQTT first, so a Status 0 a person confirmed in the console reaches the real command
	// channel, which refuses it (T1) before anything is published; it then goes once over HTTP, as before.
	g := newSimRig(t)
	before := len(g.dev.Commands())
	ctx, tr := WithTrace(WithConsole(t.Context()))
	if _, err := g.r.Command(ctx, g.target(), "Status 0"); err != nil || tr.Via() != "http" {
		t.Fatalf("err=%v via=%s, want one send over HTTP and nothing published", err, tr.Via())
	}
	if got := g.dev.Commands()[before:]; len(got) != 1 {
		t.Fatalf("the device received %v, want the one Status 0 over HTTP", got)
	}
}

type brokenFacts struct{ inner *facts }

func (b brokenFacts) HTTPState(id model.DeviceID) HTTPState { return b.inner.HTTPState(id) }
func (b brokenFacts) FactsFor(context.Context, model.Device) (Facts, error) {
	return Facts{}, errors.New("inventory unavailable")
}

func TestReadWithoutFactsNeverPublishes(t *testing.T) {
	// Why: without facts the router cannot check that the topic is exclusive, the FullTopic matches and
	// the device is online on our broker. A read then takes the established HTTP path and publishes
	// nothing, also when HTTP proves it sent nothing (fail closed).
	for _, tc := range []struct {
		name    string
		httpErr error
	}{
		{"HTTP answers", nil},
		{"HTTP did not send", tasmota.ErrForbiddenAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.r.o.Facts = brokenFacts{g.f}
			g.http.err = tc.httpErr
			_, err := g.r.Command(t.Context(), g.target(), "Status 2")
			if !errors.Is(err, tc.httpErr) || (tc.httpErr == nil && err != nil) || g.mq.n() != 0 || g.http.n() != 1 {
				t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
			}
		})
	}
}

func TestReadOfAHTTPOnlyDeviceDoesNotAskForFacts(t *testing.T) {
	// Why: facts list the whole inventory; a device that is not on our broker must not pay for that on
	// every read (the poller reads many devices).
	g := newRig(t)
	g.devs.d.MQTT = model.MQTTNone
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.f.snapshotted != 0 {
		t.Fatalf("err=%v facts asked %d times", err, g.f.snapshotted)
	}
}

func TestReadExplainsWhyMQTTWasNotUsedWhenBothFail(t *testing.T) {
	// Why: the console shows this error; it says once why MQTT was not used, also when HTTP sent
	// nothing (a second explanation would name a second, different HTTP failure).
	for _, httpErr := range []error{tasmota.ErrUnreachable, tasmota.ErrForbiddenAddr} {
		g := newRig(t)
		g.mem.Fail(7, HTTPFailUnreachable)
		g.http.err = httpErr
		g.f.lwtOffline = true
		_, err := g.r.Command(t.Context(), g.target(), "Status 2")
		if !errors.Is(err, httpErr) || strings.Count(err.Error(), "not online on MQTT") != 1 || g.http.n() != 1 || g.mq.n() != 0 {
			t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
		}
	}
}
