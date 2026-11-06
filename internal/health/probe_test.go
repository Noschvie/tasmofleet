package health

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func refuseAll(h *harness) {
	failAll(h, tasmota.ErrHTTPAPIRefused)
}

// Why (#447): a device whose HTTP API is switched off still serves its web page. The poll
// that learns it must keep the fact (the router's memory forgets after 10 minutes), and the
// next answer must clear it; otherwise the UI says "no answer" for a device that is there.
func TestRefusedHTTPAPIIsRemembered(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	refuseAll(h)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; got.IsZero() {
		t.Fatal("a refused poll left no HTTPRefusedAt")
	}
	// A plain failure (no route) is not a refusal: the page does not answer either.
	h.advance(31 * time.Minute) // a refused device is polled like a failing one: every 30 minutes at most
	failAll(h, tasmota.ErrUnreachable)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; !got.IsZero() {
		t.Fatalf("an unreachable device kept the refusal of %v", got)
	}
	refuseAll(h)
	h.advance(31 * time.Minute)
	h.step()
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(31 * time.Minute)
	h.step()
	if got := h.svc.Get(1).HTTPRefusedAt; !got.IsZero() {
		t.Fatalf("HTTP answered again but the refusal of %v stayed", got)
	}
}

// Why (#456): a device that is fine on MQTT is not polled over HTTP, so the HTTP fact decays
// to "not checked". A person who opens the device page gets one deliberate probe instead.
func TestProbeHTTPChecksADeviceThatIsFineOnMQTT(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("a device fine on MQTT was polled: %v", h.cl.commands())
	}
	h.svc.ProbeHTTP(1)
	h.step()
	if got := h.cl.commands(); len(got) != 1 || got[0] != "Status 11" {
		t.Fatalf("probe sent %v, want one Status 11", got)
	}
	if h.svc.Get(1).SeenHTTP.IsZero() {
		t.Fatal("a successful probe left SeenHTTP unset")
	}
	h.want(1, model.HealthOnline)
}

// Why: with the background poll (#619) the person-triggered probe is no longer the only HTTP a device with
// an Offline will gets, but it is still how a page view finds out now: it records what HTTP showed (a
// refused API too) and never changes the state.
func TestProbeHTTPReachesALWTOfflineDeviceOnRequest(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	failAll(h, errDown)
	h.step() // the background poll reaches nothing and backs off
	h.cl.reset()
	refuseAll(h) // then the web page answers, its HTTP API off
	h.svc.ProbeHTTP(1)
	h.step()
	g := h.svc.Get(1)
	if len(h.cl.commands()) != 1 || g.HTTPRefusedAt.IsZero() {
		t.Fatalf("probe: commands %v, refused at %v", h.cl.commands(), g.HTTPRefusedAt)
	}
	if g.State != model.HealthOffline {
		t.Fatalf("a probe changed the state to %s: the LWT still says offline", g.State)
	}
}

// Why: reloading a page must not hammer a device. One probe per gap, longer after a failure.
func TestProbeHTTPIsRateLimitedPerDevice(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	failAll(h, tasmota.ErrUnreachable)
	for i := 0; i < 5; i++ {
		h.svc.ProbeHTTP(1)
		h.svc.ProbeHTTP(1)
		h.step()
		h.advance(30 * time.Second)
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	}
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("%d probes within a few minutes, want 1: %v", n, h.cl.commands())
	}
	h.advance(probeFailGap)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.svc.ProbeHTTP(1)
	h.step()
	if n := len(h.cl.commands()); n != 2 {
		t.Fatalf("no probe after the gap: %v", h.cl.commands())
	}
}

// Why: a probe that fails says something about HTTP, not about the device. It must not count
// toward "offline" while MQTT is delivering, nor move the poll schedule.
func TestFailedProbeDoesNotMakeAMQTTDeviceOffline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	failAll(h, tasmota.ErrUnreachable)
	for i := 0; i < 6; i++ {
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false) // MQTT keeps delivering
		h.svc.ProbeHTTP(1)
		h.step()
		h.advance(probeFailGap)
	}
	// MQTT goes quiet: the first silent poll must still be allowed to fail three times before
	// the device counts as offline, as without any probe.
	h.svc.mu.Lock()
	fails := h.svc.devs[1].fails
	h.svc.mu.Unlock()
	if fails != 0 {
		t.Fatalf("probes counted %d failures toward offline", fails)
	}
}

// Why: a probe of an unknown or just-polled device is a no-op (the page may be opened for a
// device that was deleted a second ago, and a fresh HTTP fact needs no second request).
func TestProbeHTTPSkipsUnknownDevicesAndFreshFacts(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	h.svc.ProbeHTTP(9)
	h.step() // the regular first poll
	before := len(h.cl.commands())
	h.svc.ProbeHTTP(1)
	h.step()
	if n := len(h.cl.commands()); n != before {
		t.Fatalf("probe right after a successful poll sent %d extra commands", n-before)
	}
}

// Why (#447): a refused HTTP API is an answer. The device's web page is up; only TasmoFleet's commands are
// refused. Counted as failures, the refusals turned a device that is there "Offline" after three polls (the
// header, its banner and the device list) while the Reachability tile said "The web page answers". A refusal
// is contact: it never counts toward offline and it moves "last seen", but never SeenHTTP, which says the API
// answered (routing, the #598 re-read and the #580 hint read it).
func TestRefusedHTTPAPIIsAnAnswerNotAFailure(t *testing.T) {
	t.Run("HTTP-only device that answered before", func(t *testing.T) {
		h := newHarness(t, false, Options{}, httpDev(1))
		h.svc.reload(h.ctx, h.clk.Now())
		h.step()
		answered := h.svc.Get(1).SeenHTTP
		refuseAll(h)
		for i := 0; i < 2*DefaultOfflineAfter; i++ {
			h.advance(31 * time.Minute)
			h.step()
			h.want(1, model.HealthOnline)
		}
		g := h.svc.Get(1)
		if !g.SeenHTTP.Equal(answered) {
			t.Fatalf("a refusal moved SeenHTTP from %v to %v", answered, g.SeenHTTP)
		}
		if !g.LastSeen.Equal(h.clk.Now()) {
			t.Fatalf("last seen %v, want the last refusal at %v", g.LastSeen, h.clk.Now())
		}
	})
	t.Run("HTTP-only device added with its API off", func(t *testing.T) {
		h := newHarness(t, false, Options{}, httpDev(1))
		h.svc.reload(h.ctx, h.clk.Now())
		refuseAll(h)
		for i := 0; i < 2*DefaultOfflineAfter; i++ {
			h.step()
			h.want(1, model.HealthOnline)
			h.advance(31 * time.Minute)
		}
		if g := h.svc.Get(1); !g.SeenHTTP.IsZero() {
			t.Fatalf("a refusal set SeenHTTP %v", g.SeenHTTP)
		}
	})
	t.Run("MQTT device whose telemetry is quiet", func(t *testing.T) {
		h := newHarness(t, true, Options{}, mqttDev(1, "a"))
		h.listen()
		h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
		h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
		h.step()
		refuseAll(h)
		for i := 0; i < 12; i++ {
			h.advance(31 * time.Minute)
			h.step()
			if st := h.state(1); st == model.HealthOffline {
				t.Fatalf("after %d refused polls the device is %s", len(h.cl.commands()), st)
			}
		}
		if n := len(h.cl.commands()); n <= DefaultOfflineAfter {
			t.Fatalf("only %d polls: the device never reached the offline threshold", n)
		}
		if g := h.svc.Get(1); !g.SeenHTTP.IsZero() {
			t.Fatalf("a refusal set SeenHTTP %v", g.SeenHTTP)
		}
	})
}

// Why (decision 2026-11-05): a refused API does not change by itself. A refused poll waits like a failing one
// (up to 30 minutes), not the 5-minute interval of a healthy device; the page's probe gives news on demand.
func TestRefusedPollsBackOffLikeFailingOnes(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	refuseAll(h)
	h.step()
	for range 23 { // 30 minutes less the jitter's 20 %: nothing before
		h.advance(time.Minute)
		h.step()
	}
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("%d polls within 23 minutes of a refusal: %v", n, h.cl.commands())
	}
	h.advance(8 * time.Minute)
	h.step()
	if n := len(h.cl.commands()); n != 2 {
		t.Fatalf("no poll 31 minutes after a refusal: %v", h.cl.commands())
	}
	// The snapshot read of a device on MQTT that never sent its STATE is such a poll too: refused, it waits as long.
	m := newHarness(t, true, Options{}, mqttDev(1, "a"))
	m.listen()
	m.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	refuseAll(m)
	m.step()
	for range 23 {
		m.advance(time.Minute)
		m.step()
	}
	if n := len(m.cl.commands()); n != 1 {
		t.Fatalf("%d snapshot reads within 23 minutes of a refusal: %v", n, m.cl.commands())
	}
}

// Why (decision 2026-11-05): the banner says "Offline since ..." from the state change or the last contact. A probe
// that reaches a device its last will reports offline (the page asked for it) must move neither, or every page
// view would make the device look seen again.
func TestProbeContactDoesNotMoveOfflineSince(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false)
	h.want(1, model.HealthOffline)
	before := h.svc.Get(1)
	h.advance(time.Hour)
	holdPolls(h.svc, 1)                          // only the page's probe asks: a background poll would be #619's business, not this rule's
	for _, answer := range []bool{true, false} { // the device answers HTTP, then refuses its API
		if answer {
			h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
		} else {
			refuseAll(h)
			h.advance(probeFailGap)
		}
		h.svc.ProbeHTTP(1)
		h.step()
		g := h.svc.Get(1)
		if !g.LastSeen.Equal(before.LastSeen) || !g.Since.Equal(before.Since) || g.State != model.HealthOffline {
			t.Fatalf("a probe moved the offline time: last seen %v -> %v, since %v -> %v, state %s",
				before.LastSeen, g.LastSeen, before.Since, g.Since, g.State)
		}
		if answer && g.SeenHTTP.IsZero() {
			t.Fatal("the probe's answer was not recorded as SeenHTTP")
		}
	}
}

// holdPolls pushes the next background poll of a device far away, so a test can look at the probe alone.
func holdPolls(s *Service, id model.DeviceID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devs[id].nextPoll = s.d.Clock.Now().Add(1000 * time.Hour)
}

// Why (#619): a device on our broker that left unclean (power cut while switching, a restored config for
// another broker) keeps a retained Offline will but answers HTTP. It used to show Offline forever, and the
// #580 hint and #598 re-read, which need a fresh HTTP answer from an online device, never reached it.
func TestRetainedOfflineWillDeviceThatAnswersHTTPComesBack(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.want(1, model.HealthOffline)
	h.step()
	g := h.svc.Get(1)
	if g.State != model.HealthOnline || g.SeenHTTP.IsZero() || g.Source != model.SourceHTTP {
		t.Fatalf("after an answering poll: state %s, SeenHTTP %v, source %s", g.State, g.SeenHTTP, g.Source)
	}
	if g.LWT != "Offline" {
		t.Fatalf("the will is a fact about the broker and stays visible: %q", g.LWT)
	}
	// The answer is contact: "last seen" moves, here and in the store (a scheduled backup skips a device that
	// was never seen as "never answered").
	h.st.mu.Lock()
	stored := h.st.seenAt[1]
	h.st.mu.Unlock()
	if now := h.clk.Now(); !g.LastSeen.Equal(now) || !stored.Equal(now) {
		t.Fatalf("last seen %v, stored %v, want the poll's answer at %v", g.LastSeen, stored, now)
	}
	// It keeps being polled at the normal interval (the re-read needs a fresh answer), not per tick.
	h.cl.reset()
	h.step()
	h.advance(6 * time.Minute)
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("%d polls over 6 minutes, want exactly the one that came due: %v", n, h.cl.commands())
	}
	h.want(1, model.HealthOnline)
	// When it stops answering it goes back to offline after the usual number of failures, not before (an
	// HTTP-only device does the same).
	failAll(h, errDown)
	for i := 1; i <= DefaultOfflineAfter; i++ {
		h.advance(6 * time.Minute) // past each due time: the interval until it counts as offline
		h.step()
		want := model.HealthOnline
		if i == DefaultOfflineAfter {
			want = model.HealthOffline
		}
		h.want(1, want)
	}
	// And the next background poll it answers brings it back.
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(maxBackoff)
	h.step()
	h.want(1, model.HealthOnline)
}

// Why (#619, #447): a refused HTTP API is an answer, the web page is up (0.30.0). With MQTT off as well, the
// retained Offline will kept such a device offline: the caveat #447 left to #619. Like any device that refuses
// its API it is online, and APIOff says that no path is left, so backups skip it and actions say why. Going
// offline at the refusal would also hide that finding (APIOff needs a refusal after the state change): the
// tile would say "HTTP no answer" for a device whose web page answers.
func TestRetainedOfflineWillDeviceWithHTTPAPIOffIsOnlineWithoutAPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(h *harness)
	}{
		{"API off from the start (#447's device: MQTT off too)", func(*harness) {}},
		{"answered, then the API was switched off", func(h *harness) {
			h.step()
			h.want(1, model.HealthOnline)
			h.advance(6 * time.Minute)
		}},
		{"gone, then back with the API off", func(h *harness) {
			failAll(h, errDown)
			for range DefaultOfflineAfter {
				h.step()
				h.advance(maxBackoff)
			}
			h.want(1, model.HealthOffline)
		}},
	} {
		h := newHarness(t, true, Options{}, mqttDev(1, "a"))
		h.listen()
		h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
		h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
		tc.before(h)
		refuseAll(h)
		h.step()
		g := h.svc.Get(1)
		if g.State != model.HealthOnline || !g.APIOff() || !g.LastSeen.Equal(h.clk.Now()) {
			t.Fatalf("%s: state %s, APIOff %v, last seen %v at %v", tc.name, g.State, g.APIOff(), g.LastSeen, h.clk.Now())
		}
	}
}

// Why (#456, decision 2026-11-05; #619): for a device its last will reports offline the background poll alone
// decides the state. A probe a page view asked for records what HTTP showed, but it must neither bring back a
// device the polls found gone nor take one offline whose API it finds off: opening a page would change the
// state, and "Offline since" with it.
func TestProbeLeavesTheStateOfADeviceItsWillReportsOffline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.step()                       // the background poll reaches it: online
	failAll(h, errDown)
	for range DefaultOfflineAfter {
		h.advance(6 * time.Minute)
		h.step()
	}
	h.want(1, model.HealthOffline)
	before := h.svc.Get(1)
	holdPolls(h.svc, 1)
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.svc.ProbeHTTP(1)
	h.step()
	g := h.svc.Get(1)
	if g.State != model.HealthOffline || !g.Since.Equal(before.Since) || !g.LastSeen.Equal(before.LastSeen) || !g.SeenHTTP.After(before.SeenHTTP) {
		t.Fatalf("a probe's answer: state %s, since %v -> %v, last seen %v -> %v, SeenHTTP %v",
			g.State, before.Since, g.Since, before.LastSeen, g.LastSeen, g.SeenHTTP)
	}

	h = newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.step()
	h.want(1, model.HealthOnline)
	holdPolls(h.svc, 1)
	refuseAll(h)
	h.advance(probeFreshFor)
	h.svc.ProbeHTTP(1)
	h.step()
	if g := h.svc.Get(1); g.State != model.HealthOnline || g.HTTPRefusedAt.IsZero() {
		t.Fatalf("a probe that found the API off: state %s, refused at %v", g.State, g.HTTPRefusedAt)
	}
}

// Why (#619): a device that refuses the stored password answers too (401). Those refusals are no run of silence
// that takes back what the poll reached: once the person fixes the password, the next answer (here the probe of
// the page they open) shows the device online, not offline.
func TestCredentialRefusalsAreNoSilenceForADeviceItsWillReportsOffline(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.step()
	failAll(h, tasmota.ErrAuth)
	for range DefaultOfflineAfter {
		h.advance(maxBackoff)
		h.step()
	}
	h.want(1, model.HealthAuthFailed)
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(probeFreshFor)
	h.svc.ProbeHTTP(1)
	h.step()
	h.want(1, model.HealthOnline)
}

// Why (#619): the broker replays the retained will after every reconnect of TasmoFleet's MQTT connection. That
// replay is no news: a device the background poll reaches stays online and is not polled at once. Only a new
// will, one that follows an Online, starts over.
func TestReplayedOfflineWillDoesNotUndoAPollAnswer(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.step()
	h.want(1, model.HealthOnline)
	h.cl.reset()
	h.advance(time.Minute)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true) // the reconnect's replay
	h.want(1, model.HealthOnline)
	h.step()
	if n := len(h.cl.commands()); n != 0 {
		t.Fatalf("a replayed will was polled at once: %v", h.cl.commands())
	}
}

// Why (#619): an HTTP answer from before the device's new, real last will says nothing about now. A fresh
// Offline will makes the device offline again and is checked, one poll interval later (#652: right after a
// live will the device is usually still rebooting).
func TestNewOfflineWillOverridesAnEarlierPollAnswer(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", true)
	h.advance(DefaultPollInterval) // a replayed will is polled somewhere within one interval, not at once (#652)
	h.step()
	h.want(1, model.HealthOnline)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.want(1, model.HealthOnline)
	h.advance(time.Minute)
	failAll(h, errDown)
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Offline", false)
	h.want(1, model.HealthOffline)
	h.cl.reset()
	h.advance(DefaultPollInterval * 13 / 10) // past the jitter
	h.step()
	if n := len(h.cl.commands()); n != 1 {
		t.Fatalf("a fresh will was not checked after one interval: %v", h.cl.commands())
	}
}

// reset forgets the commands recorded so far.
func (c *fakeClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cmds = nil
}

// Why (#645): the card's "Since" is when the API was first found off. HTTPRefusedAt moves with every refusal and
// drives the rules; HTTPRefusedSince stays until HTTP answers or the device is gone altogether.
func TestRefusalSinceIsTheFirstRefusalOfTheRun(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	refuseAll(h)
	h.step()
	first := h.clk.Now()
	for range 3 {
		h.advance(31 * time.Minute)
		h.step()
	}
	g := h.svc.Get(1)
	if !g.HTTPRefusedSince.Equal(first) || !g.HTTPRefusedAt.After(first) {
		t.Fatalf("since %v, latest %v, want since the first refusal %v", g.HTTPRefusedSince, g.HTTPRefusedAt, first)
	}
	// nothing answers at all: the finding is gone, and a later refusal starts a new run
	failAll(h, tasmota.ErrUnreachable)
	h.advance(31 * time.Minute)
	h.step()
	if g := h.svc.Get(1); !g.HTTPRefusedSince.IsZero() {
		t.Fatalf("an unreachable device kept the refusal since %v", g.HTTPRefusedSince)
	}
	refuseAll(h)
	h.advance(31 * time.Minute)
	h.step()
	second := h.clk.Now()
	if g := h.svc.Get(1); !g.HTTPRefusedSince.Equal(second) {
		t.Fatalf("new run since %v, want %v", g.HTTPRefusedSince, second)
	}
	h.cl.set(func(string, string) (json.RawMessage, error) { return json.RawMessage(sts11), nil })
	h.advance(31 * time.Minute)
	h.step()
	if g := h.svc.Get(1); !g.HTTPRefusedSince.IsZero() {
		t.Fatalf("HTTP answered, the refusal since %v stayed", g.HTTPRefusedSince)
	}
}

// Why: a refusal is an answer, so the failures before it no longer add up with those after it. Without the reset,
// one timeout after a refusal would take a device offline that talked to us a moment ago.
func TestRefusalResetsTheRunOfFailures(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1))
	h.svc.reload(h.ctx, h.clk.Now())
	failAll(h, errDown)
	h.step()
	h.advance(31 * time.Minute)
	h.step()
	h.svc.mu.Lock()
	before := h.svc.devs[1].fails
	h.svc.mu.Unlock()
	if before == 0 {
		t.Fatal("fixture: no failures counted")
	}
	refuseAll(h)
	h.advance(31 * time.Minute)
	h.step()
	h.svc.mu.Lock()
	after := h.svc.devs[1].fails
	h.svc.mu.Unlock()
	if after != 0 {
		t.Fatalf("failures after a refusal: %d, want 0 (was %d)", after, before)
	}
}

// Why: a probe is the page's question, not a poll. A refused probe records the refusal and leaves the poll
// schedule alone; a probe that times out says nothing about the API and keeps the finding, while one that finds
// nothing at all (no route) drops it.
func TestRefusedProbeKeepsThePollScheduleAndTheFindingSurvivesATimeout(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "a"))
	h.listen()
	h.bus.emit(mqttbus.KindLWT, "a", "LWT", "Online", false)
	h.bus.emit(mqttbus.KindState, "a", "STATE", `{"UptimeSec":10}`, false)
	h.step()
	h.svc.mu.Lock()
	next := h.svc.devs[1].nextPoll
	h.svc.mu.Unlock()
	refuseAll(h)
	h.svc.ProbeHTTP(1)
	h.step()
	h.svc.mu.Lock()
	moved := !h.svc.devs[1].nextPoll.Equal(next)
	h.svc.mu.Unlock()
	if h.svc.Get(1).HTTPRefusedAt.IsZero() || moved {
		t.Fatalf("refused probe: recorded %v, poll schedule moved %v", !h.svc.Get(1).HTTPRefusedAt.IsZero(), moved)
	}
	failAll(h, tasmota.ErrTimeout)
	h.advance(probeFailGap)
	h.svc.ProbeHTTP(1)
	h.step()
	if h.svc.Get(1).HTTPRefusedAt.IsZero() {
		t.Fatal("a probe that timed out dropped the refusal")
	}
	failAll(h, tasmota.ErrUnreachable)
	h.advance(probeFailGap)
	h.svc.ProbeHTTP(1)
	h.step()
	if !h.svc.Get(1).HTTPRefusedAt.IsZero() {
		t.Fatal("a probe that reached nothing kept the refusal")
	}
}

// Why (#456, #619): "Offline since" ignores a probe's HTTP contact only for a device that lives on our broker,
// where the will is the truth. A device that is not in MQTT mode (the broker switched off, or never ours) has
// only HTTP, so its HTTP contact is its last sighting whatever stale will it once sent.
func TestLastObservationIgnoresHTTPProbesOnlyInMQTTMode(t *testing.T) {
	h := newHarness(t, false, Options{}, mqttDev(1, "a"))
	sb := &switchedBus{on: true}
	h.svc.d.Bus = sb
	t0 := h.clk.Now()
	ds := &devState{topic: "a", cap: model.MQTTOurs, lwt: "Offline", lastMQTT: t0, lastPolled: t0, lastHTTP: t0.Add(time.Hour)}
	h.svc.mu.Lock()
	defer h.svc.mu.Unlock()
	if got := h.svc.lastObs(ds); !got.Equal(t0) {
		t.Fatalf("MQTT mode: last observation %v, want the will's time %v (a probe must not move it)", got, t0)
	}
	sb.on = false
	if got := h.svc.lastObs(ds); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("not in MQTT mode: last observation %v, want the HTTP contact", got)
	}
}
