package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #876: a discovery config that names a stored device's topic with another MAC is a claim anyone who can publish on the
// discovery topics can make. It flags the device for the broker reader's next pass, whose check (the router's own, it
// hears every reply on the topic) alone marks the device. These tests drive the reader as the app wires it, with the
// real router and a command channel on which the device and, when set, a second board answer.

const claimedTopic = "plug"

// claimEnv is a reader wired as the app wires the topic checks, over the router's real marks. It only does the topic
// work of a pass (no identity or data read is wired, and HTTP is down for the device, so no broker read is due).
type claimEnv struct {
	t      *testing.T
	r      *brokerHostReader
	p      *Prober
	st     *store.SQLite
	id     model.DeviceID
	onTopi *brokerAnswers // the device on the topic; its Status 5 count is the number of checks
	ch     *twoOnTopic
	now    time.Time
	log    bytes.Buffer
	busy   bool // enter refuses: a job holds the device
}

func newClaimEnv(t *testing.T, second, lwt string) *claimEnv {
	t.Helper()
	onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, lwt)
	router := p.Reads.(*transport.Router)
	facts := routerFacts(st, router, id, lwt)
	router.SetFacts(facts)
	ch := &twoOnTopic{dev: onTopic, second: second}
	router.SetMQTT(ch)
	e := &claimEnv{t: t, p: p, st: st, id: id, onTopi: onTopic, ch: ch, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	e.r = newBrokerHostReader(st.ListDevices, func(model.DeviceID) bool { return false }, p.ReadBroker,
		func(context.Context, model.DeviceID, string) (func(), error) {
			if e.busy {
				return nil, errBrokerBusy
			}
			return func() {}, nil
		}, slog.New(slog.NewTextHandler(&e.log, nil)))
	e.r.now = func() time.Time { return e.now }
	e.r.topicMarked, e.r.recheckTopic, e.r.checkClaim = p.topicMarked, p.recheckMarked, p.checkClaimed
	return e
}

// claim is what the discovery worker does, and the reader's loop does with it: the claim is queued without waiting and
// taken up by run.
func (e *claimEnv) claim(topic string) {
	e.t.Helper()
	e.r.topicClaimed(e.id, topic)
	select {
	case c := <-e.r.claims:
		e.r.noteClaim(c)
	default:
		e.t.Fatal("the claim did not reach the reader's queue")
	}
}

func (e *claimEnv) checks() int {
	n := 0
	for _, c := range e.onTopi.asked() {
		if c == "Status 5" {
			n++
		}
	}
	return n
}

func (e *claimEnv) marked() bool { return !e.p.Topics.Since(e.id, claimedTopic).IsZero() }

func (e *claimEnv) warnings() int {
	return strings.Count(e.log.String(), "another device answers on the device's MQTT topic")
}

// Why (#876): the claim is unverified. If it marked the device, anyone who can publish on the discovery topics (under a
// strict per-device ACL, any one device's login) could push every other device off MQTT for good. The router's check
// hears who answers on the topic: with only the device answering, a forged claim costs one Status 5 window and marks
// nothing. When MQTT is not usable for the device nothing is asked at all (a mark would change nothing there).
func TestAForgedTopicClaimAloneMarksNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lwt        string
		wantChecks int
	}{
		{"only the device answers on the topic", "Online", 1},
		{"MQTT is not usable for the device", "Offline", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t, "", tc.lwt)
			e.claim(claimedTopic)
			if e.checks() != 0 || e.marked() {
				t.Fatalf("before a pass: %d checks, marked %v: the claim itself did something", e.checks(), e.marked())
			}
			e.r.pass(t.Context())
			if e.marked() {
				t.Error("a claim only the device answers on marked the device")
			}
			if got := e.checks(); got != tc.wantChecks {
				t.Errorf("checks = %d, want %d", got, tc.wantChecks)
			}
			if e.warnings() != 0 {
				t.Errorf("warned of another device when none answered: %s", e.log.String())
			}
		})
	}
}

// Why (#876): the claim is what makes the router look at the topic of a device that is not marked yet, which nothing
// else does between the identity probes (every 24 hours): a real second board marks the device through the check, with
// one warning that names the topic, and the device's writes then stay off the topic (T19).
func TestASecondBoardOnAClaimedTopicIsMarkedByTheRoutersCheck(t *testing.T) {
	e := newClaimEnv(t, otherMAC, "Online")
	e.claim(claimedTopic)
	e.r.pass(t.Context())
	if !e.marked() {
		t.Fatal("a second board answers on the claimed topic and the device is not marked")
	}
	if got := e.checks(); got != 1 {
		t.Errorf("checks = %d, want 1", got)
	}
	if got := e.warnings(); got != 1 || !strings.Contains(e.log.String(), "topic=plug") {
		t.Errorf("warnings = %d in %q, want one that names the topic", got, e.log.String())
	}
}

// Why (#876): every published copy of the config is a claim, and each check holds a 1.5 s reply window and the device's
// lock. A forger, or a board that republishes, must not make the router ask a device's topic more than once per
// brokerHostRetry. Twenty claims within a minute, with the reader's passes between them, are one check; the mark such a
// check sets is asked again on its own schedule only after brokerHostRetry, not at the next pass (30 s), which would be
// a second check inside the window. After the window a claim asks again.
func TestTopicClaimsAskTheTopicAtMostOncePerRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second string
	}{
		{"a forged claim", ""},
		{"a second board that is really there", otherMAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newClaimEnv(t, tc.second, "Online")
			for i := range 20 {
				e.now = e.now.Add(3 * time.Second)
				e.claim(claimedTopic)
				if i%10 == 9 { // the reader's passes, every 30 s
					e.r.pass(t.Context())
				}
			}
			if got := e.checks(); got != 1 {
				t.Fatalf("checks after 20 claims in a minute = %d, want 1", got)
			}
			if got := e.marked(); got != (tc.second != "") {
				t.Errorf("marked = %v with second board %q", got, tc.second)
			}
			if tc.second != "" {
				return // a marked device is asked on the mark's own schedule from here, which is not about claims
			}
			e.now = e.now.Add(brokerHostRetry)
			e.claim(claimedTopic)
			e.r.pass(t.Context())
			if got := e.checks(); got != 2 {
				t.Errorf("checks after a claim past the window = %d, want 2: a claim after brokerHostRetry is asked", got)
			}
		})
	}
}

// Why (#876): a claim is about the topic it named. A device whose topic was edited since, or that was deleted, is not
// asked: a check of another topic proves nothing about the claimed one, and the claim and its window must not outlive
// the device.
func TestAClaimForAnotherTopicOrAGoneDeviceAsksNothing(t *testing.T) {
	t.Run("the topic was edited before the pass", func(t *testing.T) {
		e := newClaimEnv(t, otherMAC, "Online")
		e.claim(claimedTopic)
		d, err := e.st.GetDevice(t.Context(), e.id)
		if err != nil {
			t.Fatal(err)
		}
		d.Topic = "plug-new"
		if err := e.st.UpdateDevice(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		e.r.pass(t.Context())
		if e.checks() != 0 || len(e.r.claimed) != 0 {
			t.Errorf("checks = %d, claims left = %d, want none of either", e.checks(), len(e.r.claimed))
		}
	})
	t.Run("the topic was edited between the list and the check", func(t *testing.T) {
		// The reader's list is a snapshot; the prober reads the stored topic again before it asks (withStoredTopic).
		e := newClaimEnv(t, otherMAC, "Online")
		d, err := e.st.GetDevice(t.Context(), e.id)
		if err != nil {
			t.Fatal(err)
		}
		d.Topic = "plug-new"
		if err := e.st.UpdateDevice(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		d.Topic = claimedTopic // the snapshot the reader listed
		if err := e.p.checkClaimed(t.Context(), d, claimedTopic); err != nil || e.checks() != 0 {
			t.Errorf("checkClaimed = %v after %d checks, want nil and none: the stored topic is not the claimed one", err, e.checks())
		}
	})
	t.Run("the device was deleted", func(t *testing.T) {
		e := newClaimEnv(t, otherMAC, "Online")
		e.claim(claimedTopic)
		e.r.pass(t.Context()) // one check: opens the window
		e.claim(claimedTopic)
		if err := e.st.DeleteDevice(t.Context(), e.id); err != nil {
			t.Fatal(err)
		}
		e.r.pass(t.Context())
		if len(e.r.claimed) != 0 || len(e.r.claimNext) != 0 {
			t.Errorf("claims = %v, windows = %v left for a deleted device", e.r.claimed, e.r.claimNext)
		}
	})
}

// Why (#876): a claim is a flag with a defined end. A device a job holds is not asked (nothing is sent to it), but the
// claim waits for the first pass where the lock is free, and ends by itself after brokerHostPendingFor: a job that never
// lets go must not keep a forged claim alive for good. A device that is marked already needs no claim: its own
// schedule asks the topic.
func TestAClaimOnABusyOrMarkedDeviceWaitsOrEnds(t *testing.T) {
	t.Run("busy, then free", func(t *testing.T) {
		e := newClaimEnv(t, otherMAC, "Online")
		e.busy = true
		e.claim(claimedTopic)
		e.r.pass(t.Context())
		if e.checks() != 0 || len(e.r.claimed) != 1 {
			t.Fatalf("checks = %d, claims = %d: a busy device is not asked and the claim waits", e.checks(), len(e.r.claimed))
		}
		e.busy = false
		e.now = e.now.Add(brokerHostTick)
		e.r.pass(t.Context())
		if e.checks() != 1 || !e.marked() {
			t.Errorf("checks = %d, marked = %v after the device was free, want 1 and marked", e.checks(), e.marked())
		}
	})
	t.Run("busy until the claim's end", func(t *testing.T) {
		e := newClaimEnv(t, otherMAC, "Online")
		e.busy = true
		e.claim(claimedTopic)
		e.now = e.now.Add(brokerHostPendingFor + time.Second)
		e.r.pass(t.Context())
		e.busy = false
		e.now = e.now.Add(brokerHostTick)
		e.r.pass(t.Context())
		if e.checks() != 0 || len(e.r.claimed) != 0 {
			t.Errorf("checks = %d, claims = %d: a claim a job held off for brokerHostPendingFor ends unasked", e.checks(), len(e.r.claimed))
		}
	})
	t.Run("already marked", func(t *testing.T) {
		e := newClaimEnv(t, otherMAC, "Online")
		e.p.Topics.Mark(e.id, claimedTopic)
		e.claim(claimedTopic)
		e.r.pass(t.Context()) // the mark's own schedule asks once; the claim adds nothing
		e.claim(claimedTopic)
		e.now = e.now.Add(brokerHostTick)
		e.r.pass(t.Context())
		if got := e.checks(); got != 1 || len(e.r.claimed) != 0 {
			t.Errorf("checks = %d, claims = %d, want 1 (the mark's own) and no claim left", got, len(e.r.claimed))
		}
	})
}

// Why (#876): the claim must reach the reader and nowhere else. The options the app hands the discovery manager carry
// the hook, the hook marks nothing in the router (a forged claim alone marks nothing), and it never waits: it runs on
// the discovery worker, which hears every message of the broker, so a reader with a full queue must drop the claim
// instead of stalling discovery.
func TestTheDiscoveryHookOnlyFlagsTheDeviceForTheReader(t *testing.T) {
	e := newClaimEnv(t, otherMAC, "Online")
	router := e.p.Reads.(*transport.Router)
	a := &runtime{svc: &Services{Router: router}, brokerHost: e.r, log: slog.New(slog.NewTextHandler(&e.log, nil))}

	opts := a.discoveryOptions(nil, nil)
	if opts.OnTopicClaim == nil {
		t.Fatal("setupDiscovery does not wire OnTopicClaim: discovery would hear another board on a topic and tell nobody")
	}
	opts.OnTopicClaim(e.id, claimedTopic)
	if e.marked() || e.checks() != 0 {
		t.Fatal("the hook marked the device or asked the topic itself: only the reader's check may")
	}
	select {
	case c := <-e.r.claims:
		if c.id != e.id || c.topic != claimedTopic {
			t.Fatalf("the reader got %+v", c)
		}
	default:
		t.Fatal("the claim did not reach the reader")
	}

	done := make(chan struct{})
	go func() {
		for range brokerHostQueue + 5 {
			opts.OnTopicClaim(e.id, claimedTopic)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook waits on a full queue: it would stall the discovery worker")
	}
	if got := len(e.r.claims); got != brokerHostQueue {
		t.Errorf("queued %d claims, want the queue's %d (the rest dropped)", got, brokerHostQueue)
	}
}

// Why (#876): a reader that is not there (MQTT off, a test runtime) must not make the hook panic on the discovery worker.
func TestADiscoveryClaimWithoutAReaderIsDropped(t *testing.T) {
	a := &runtime{svc: &Services{Router: transport.NewRouter(transport.Options{})}}
	a.discoveryOptions(nil, nil).OnTopicClaim(1, claimedTopic)
}

// Why (#876): a check that is cut short or fails leaves no claim behind that would be asked again at every pass: the
// claim ends with its one try, and the next claim after the window asks again.
func TestAClaimEndsWithItsCheckWhateverItLearnt(t *testing.T) {
	e := newClaimEnv(t, otherMAC, "Online")
	e.r.checkClaim = func(context.Context, model.Device, string) error { return errors.New("nothing heard") }
	e.claim(claimedTopic)
	e.r.pass(t.Context())
	if len(e.r.claimed) != 0 {
		t.Errorf("a claim whose check failed is still waiting: %v", e.r.claimed)
	}
	if e.marked() {
		t.Error("a failed check marked the device")
	}
}

// Why (#876): a claim is untrusted input and costs the reader's one goroutine: each check holds it for the 1.5 s window
// (up to the 15 s budget for a device that stays silent) while kicks, restart reads and the periodic reads wait. Under a
// strict ACL one login can flag every stored device at once, so a pass runs at most brokerHostClaimsPerPass checks, like
// the reader's one probe request per pass. The claims over the cap are not lost: they are asked at the passes that follow
// (every 30 s), each device exactly once.
func TestAPassRunsOnlyAFewClaimChecksAndTheRestWaitForLaterPasses(t *testing.T) {
	const devices = 10
	devs := make([]model.Device, devices)
	for i := range devs {
		devs[i] = model.Device{ID: model.DeviceID(i + 1), Topic: "plug" + string(rune('a'+i))}
	}
	asked := map[model.DeviceID]int{}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := newBrokerHostReader(func(context.Context) ([]model.Device, error) { return devs, nil },
		func(model.DeviceID) bool { return false }, func(context.Context, model.DeviceID) error { return nil },
		func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	r.now = func() time.Time { return now }
	r.topicMarked = func(model.Device) bool { return false }
	r.recheckTopic = func(context.Context, model.Device) error { return nil }
	r.checkClaim = func(_ context.Context, d model.Device, _ string) error { asked[d.ID]++; return nil }
	for _, d := range devs {
		r.topicClaimed(d.ID, d.Topic)
		r.noteClaim(<-r.claims)
	}

	r.pass(t.Context())
	total := 0
	for _, n := range asked {
		total += n
	}
	if total > brokerHostClaimsPerPass {
		t.Fatalf("one pass ran %d claim checks, want at most %d: one login flagging the whole fleet would hold the reader for minutes", total, brokerHostClaimsPerPass)
	}
	for i := 0; i < devices && len(r.claimed) > 0; i++ {
		now = now.Add(brokerHostTick)
		r.pass(t.Context())
	}
	for _, d := range devs {
		if asked[d.ID] != 1 {
			t.Errorf("device %d was asked %d times, want exactly once: the claims over the cap wait, and none is asked twice", d.ID, asked[d.ID])
		}
	}
}
