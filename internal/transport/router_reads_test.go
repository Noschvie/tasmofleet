package transport

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
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

func TestWritesKeepTheHTTPFirstOrder(t *testing.T) {
	// Why: stage 1 changes reads only; a write has no synchronous MQTT reply to rely on yet (stage 2).
	g := newRig(t)
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.http.n() != 1 || g.mq.n() != 0 {
		t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
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

func TestReadFailsOverToHTTPOnlyWhenNothingWasPublished(t *testing.T) {
	// Why (#456 A, D4 turned round): an MQTT error proves "never left" only when the channel refused
	// the command or could not publish it. A published read that got no answer has an unknown
	// outcome for that read: it is reported as it is and never sent a second time over HTTP.
	for _, tc := range []struct {
		name      string
		err       error
		failover  bool
		wantIsErr error
	}{
		{"refused by the channel", fmt.Errorf("%w: Status 0 is never sent", mqttcmd.ErrRefused), true, nil},
		{"could not publish", fmt.Errorf("%w: Status 2 not sent", tasmota.ErrUnreachable), true, nil},
		{"published, no answer", fmt.Errorf("mqtt: no answer to Status 2: %w", tasmota.ErrTimeout), false, tasmota.ErrTimeout},
		{"published, device went offline", fmt.Errorf("mqtt: offline before it answered (outcome unknown): %w", tasmota.ErrTimeout), false, tasmota.ErrTimeout},
		{"published, reply unusable", fmt.Errorf("%w: reply", tasmota.ErrBadResponse), false, tasmota.ErrBadResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.mq.err = tc.err
			raw, err := g.r.Command(t.Context(), g.target(), "Status 2")
			if tc.failover {
				if err != nil || string(raw) != `{"HTTP":"yes"}` || g.http.n() != 1 || g.mq.n() != 1 {
					t.Fatalf("raw=%s err=%v http=%d mqtt=%d", raw, err, g.http.n(), g.mq.n())
				}
				return
			}
			if !errors.Is(err, tc.wantIsErr) || g.http.n() != 0 || g.mq.n() != 1 {
				t.Fatalf("err=%v http=%d mqtt=%d: a published read must not be sent again", err, g.http.n(), g.mq.n())
			}
		})
	}
}

func TestFailoverReadIsSentOnceOverHTTPAndNotBackToMQTT(t *testing.T) {
	// Why: the fall back to HTTP after a refusal must not bounce to MQTT again when HTTP did not send
	// either (that would publish the read a second time).
	g := newRig(t)
	g.mq.err = fmt.Errorf("%w: refused", mqttcmd.ErrRefused)
	g.http.err = tasmota.ErrForbiddenAddr
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 1 || g.http.n() != 1 {
		t.Fatalf("err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
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
	// Why (CLAUDE.md safety rule 2): the real channel refuses Status 0; the router must treat that as
	// "nothing published" and not turn it into an error for a console read.
	g := newRig(t)
	g.mq.err = fmt.Errorf("%w: Status 0 is never sent", mqttcmd.ErrRefused)
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "Status 0"); err != nil || g.http.n() != 1 {
		t.Fatalf("err=%v http=%d", err, g.http.n())
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
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.http.err = tasmota.ErrUnreachable
	g.f.lwtOffline = true
	_, err := g.r.Command(t.Context(), g.target(), "Status 2")
	if err == nil || !strings.Contains(err.Error(), "not online on MQTT") {
		t.Fatalf("err=%v", err)
	}
}
