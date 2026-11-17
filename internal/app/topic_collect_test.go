package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/templates"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #804, #878: the checks that vouch for reads over MQTT (the data refresh and the broker read before their reads, the
// Settings tab and the policy poller after theirs, ReadNet's Status 5) heard one reply: whichever device on the topic
// answered first. A second device on an unmarked topic was never heard, and its values could be stored on this device.
// These tests drive the real router as the app wires it.

// sharedTopic is the command channel of a topic on which the stored device answers first and, while second is set, a
// second device answers every Status 5 after it (a board with a copied config). Do hears the first reply only, as a
// single read does; DoAll (transport.MQTTMultiReader, the production channel's collect-all) hears every reply in the
// window. Every read gets an answer that fills the caches (Status 6 with a broker, Module and Status as storedAnswer).
type sharedTopic struct {
	mu     sync.Mutex
	second string
	got    []string
}

func (m *sharedTopic) Connected() bool { return true }
func (m *sharedTopic) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	m.mu.Lock()
	m.got = append(m.got, cmd)
	m.mu.Unlock()
	switch cmd {
	case "Status 5":
		return mac5(storedMAC), nil
	case "Status 6":
		return json.RawMessage(`{"StatusMQT":{"MqttHost":"mqtt.home","MqttPort":1883}}`), nil
	case "SetOption3":
		return json.RawMessage(`{"SetOption3":"ON"}`), nil
	}
	return answers("", nil)(cmd)
}
func (m *sharedTopic) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}
func (m *sharedTopic) DoAll(ctx context.Context, d model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	first, err := m.Do(ctx, d, cmd)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.second == "" || cmd != "Status 5" {
		return []json.RawMessage{first}, nil
	}
	return []json.RawMessage{first, mac5(m.second)}, nil
}
func (m *sharedTopic) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.got...)
}

// sharedRig is a device on our broker (topic "plug", stored MAC storedMAC) whose reads go over MQTT, with the three data
// caches wired as the app wires them (Settings, policy with one rule, templates), all reading through the router.
type sharedRig struct {
	p     *Prober
	d     model.Device
	topic *sharedTopic
	set   *settings.Manager
	rep   *policyReporter
	tpl   *templates.Manager
	a     *runtime
}

func newSharedRig(t *testing.T, second string) *sharedRig {
	t.Helper()
	p, st, id := brokerReadOverRouter(t, &brokerAnswers{mac: storedMAC}, &brokerAnswers{mac: storedMAC, host: "http.example"}, "Online")
	g := &sharedRig{p: p, topic: &sharedTopic{second: second}}
	router := p.Reads.(*transport.Router)
	router.SetMQTT(g.topic)
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	g.d = d
	g.set = settings.New(settings.Options{Devices: st, Client: router, ReadTimeout: 5 * time.Second, Verify: p.VerifyRead})
	g.rep = &policyReporter{devices: fakeLister{d}, reader: g.set, verify: p.VerifyRead, now: time.Now}
	g.rep.setRules([]policy.Rule{{ID: "r1", Command: "PowerOnState", Want: "3"}})
	g.tpl = templates.New(templates.Options{Devices: st, Client: router, Backups: noBackups{}})
	g.a = &runtime{svc: &Services{Prober: p}, settings: g.set, policyLive: g.rep, templates: g.tpl}
	if !p.ReadsOverMQTT(t.Context(), d) {
		t.Fatal("the router does not read the device over MQTT: the rig does not show the case it is about")
	}
	return g
}

func (g *sharedRig) marked() bool { return !g.p.Topics.Since(g.d.ID, "plug").IsZero() }

func (g *sharedRig) count5() int {
	n := 0
	for _, c := range g.topic.asked() {
		if c == "Status 5" {
			n++
		}
	}
	return n
}

// wantNothingCached fails when any of the caches a page or the policy report shows holds a value of the device.
func (g *sharedRig) wantNothingCached(t *testing.T) {
	t.Helper()
	if snap, ok := g.set.Last(g.d.ID); ok {
		t.Errorf("the Settings cache holds %d values heard on a topic two devices answer on", len(snap.Values))
	}
	if v := g.rep.vals.get(g.d.ID); len(v) != 0 {
		t.Errorf("the policy values %v were stored from a topic two devices answer on", v)
	}
	if liveTemplate(t, g.tpl) {
		t.Error("a template reading was stored from a topic two devices answer on")
	}
}

// Why (#804): the data refresh is the only background visit of a device whose HTTP API is off, and what it reads fills
// the Settings tab, the policy report and the template page. Its check heard one reply on the topic, so with a second
// board on the topic (a copied config) the stored device's own reply, heard first, vouched for every read after it,
// and those reads were answered by both boards: the caches showed whichever answered first, and nothing marked the
// topic, so the device's writes kept going to both. The check before the reads must hear every reply within the
// identity window: two replies refuse the refresh, nothing is cached, the topic is marked and the device page gets the
// reason (FailOtherDevice). The broker read follows the same check and stores no broker then.
func TestTwoDevicesOnAnUnmarkedTopicRefuseTheDataRefresh(t *testing.T) {
	t.Run("the data refresh", func(t *testing.T) {
		g := newSharedRig(t, otherMAC)
		r := newBrokerHostReader(nil, nil, g.p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
			slog.New(slog.DiscardHandler))
		var got error
		r.readData = func(ctx context.Context, d model.Device) error { got = g.a.refreshData(ctx, d); return got }
		if !r.readDataOf(t.Context(), g.d) {
			t.Fatal("the data was not read")
		}
		if !errors.Is(got, errAnotherDeviceOnTopic) {
			t.Errorf("refreshData = %v, want errAnotherDeviceOnTopic", got)
		}
		g.wantNothingCached(t)
		if !g.marked() {
			t.Error("the topic two devices answer on is not marked: the device's writes still go to both")
		}
		if f, ok := r.outcomes.Failure(g.d.ID); !ok || f.Reason != settings.FailOtherDevice {
			t.Errorf("the device page's reason = %q (recorded %v), want %q", f.Reason, ok, settings.FailOtherDevice)
		}
		if asked := g.topic.asked(); !slices.Equal(asked, []string{"Status 5"}) {
			t.Errorf("asked %v on the topic, want only the check", asked)
		}
	})
	t.Run("the broker read", func(t *testing.T) {
		g := newSharedRig(t, otherMAC)
		if err := g.p.ReadBroker(t.Context(), g.d.ID); !errors.Is(err, errAnotherDeviceOnTopic) {
			t.Errorf("ReadBroker = %v, want errAnotherDeviceOnTopic", err)
		}
		if d, _ := g.p.Devices.GetDevice(t.Context(), g.d.ID); d.MQTTHost != "old.example" {
			t.Errorf("stored broker %q: a broker heard on a topic two devices answer on was stored", d.MQTTHost)
		}
		if !g.marked() {
			t.Error("the topic two devices answer on is not marked")
		}
	})
	t.Run("the device alone on its topic: one check, then the reads", func(t *testing.T) {
		g := newSharedRig(t, "")
		if err := g.a.refreshData(t.Context(), g.d); err != nil {
			t.Fatal(err)
		}
		if _, ok := g.set.Last(g.d.ID); !ok {
			t.Error("the Settings cache was not filled although only the device answers on its topic")
		}
		if v := g.rep.vals.get(g.d.ID)["PowerOnState"]; v.value == "" {
			t.Errorf("the policy values were not refreshed: %v", g.rep.vals.get(g.d.ID))
		}
		// the checks after the Settings and policy reads rely on the first one (pinnedToMQTTCheck): that is right only
		// because the first one heard every reply
		if n := g.count5(); n != 1 {
			t.Errorf("%d x Status 5 on the topic in one refresh, want 1", n)
		}
		if g.marked() {
			t.Error("the device alone on its topic was marked")
		}
	})
}

// Why (#878): the Settings tab and the policy poller read through the router and check after their reads (VerifyRead).
// Over MQTT that check heard one reply, so while two devices answer on the topic the tab and the policy report showed
// whichever answered first, possibly the other device's values. The check must hear every reply on the topic: two
// replies refuse the values, nothing is stored, and the topic is marked.
func TestTheSettingsTabAndThePolicyPollerRefuseTwoDevicesOnTheTopic(t *testing.T) {
	for _, c := range []struct {
		name string
		read func(t *testing.T, g *sharedRig) bool // true: the read returned values
	}{
		{"the Settings tab", func(t *testing.T, g *sharedRig) bool {
			vals, err := g.set.Read(t.Context(), g.d.ID)
			return err == nil || len(vals) != 0
		}},
		{"the policy poller", func(t *testing.T, g *sharedRig) bool { return g.rep.readDevice(t.Context(), g.d) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newSharedRig(t, otherMAC)
			if c.read(t, g) {
				t.Error("the read returned values heard on a topic two devices answer on")
			}
			g.wantNothingCached(t)
			if !g.marked() {
				t.Error("the topic two devices answer on is not marked")
			}
		})
	}
}

// Why (#878): a check after reads over MQTT that learnt nothing from the check that hears every reply (nobody answered
// in the window, a channel that hears one reply only) has nothing that vouches for the topic: a single reply there is
// whichever device answered first. It must refuse the values instead of passing on one reply. A single reply without a
// MAC is not learning nothing: Tasmota always reports its MAC in Status 5 (minimal firmware answers Unknown instead), so
// the replier is another device on the topic, as it is over HTTP (VerifyAddress); the topic is marked, or the device's
// writes over MQTT keep reaching it.
func TestACheckAfterAnMQTTReadThatLearntNothingRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		ch    func(*brokerAnswers) transport.MQTTDoer
		want  error
		marks bool // the check marks the topic
	}{
		{"nobody answers in the window", func(b *brokerAnswers) transport.MQTTDoer { return noCollectTopic{brokerMQTT{dev: b}} }, tasmota.ErrUnreachable, false},
		{"the channel hears one reply only", func(b *brokerAnswers) transport.MQTTDoer { return oneReplyTopic{dev: b} }, tasmota.ErrUnreachable, false},
		{"a reply without a MAC", func(b *brokerAnswers) transport.MQTTDoer { return noMACTopic{brokerMQTT{dev: b}} }, errAnotherDeviceOnTopic, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			onTopic := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
			p, st, id := brokerReadOverRouter(t, onTopic, &brokerAnswers{mac: storedMAC, host: "http.example"}, "Online")
			p.Reads.(*transport.Router).SetMQTT(c.ch(onTopic))
			d, err := st.GetDevice(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			tctx, trace := transport.WithTrace(t.Context())
			if _, err := p.Reads.Command(tctx, tasmota.TargetFor(d), "Status 6"); err != nil {
				t.Fatal(err)
			}
			if via := trace.Via(); via != "mqtt" {
				t.Fatalf("the read went over %s, want mqtt: the test does not show the case it is about", via)
			}
			if err := p.VerifyRead(t.Context(), d, trace); !errors.Is(err, c.want) {
				t.Errorf("VerifyRead = %v, want %v: nothing vouched for the read on the topic", err, c.want)
			}
			if marked := !p.Topics.Since(d.ID, "plug").IsZero(); marked != c.marks {
				t.Errorf("topic marked %v, want %v", marked, c.marks)
			}
		})
	}
}

// noMACTopic answers Status 5 on the topic without a MAC, in a single read and in the window alike.
type noMACTopic struct{ brokerMQTT }

func (m noMACTopic) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	if cmd == "Status 5" {
		return json.RawMessage(`{"StatusNET":{"Hostname":"plug"}}`), nil
	}
	return m.brokerMQTT.Do(ctx, d, cmd)
}
func (m noMACTopic) DoAll(ctx context.Context, d model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	raw, err := m.Do(ctx, d, cmd)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}

// Why (#804 review): a reply on the topic without a MAC comes from something that is not the stored device (Tasmota
// always reports its MAC in Status 5; minimal firmware answers Unknown). Read as "nothing learnt", the check went on to
// the address, where the stored device vouched for reads the router then sent to HTTP, and nothing marked the topic:
// the device's writes over MQTT kept reaching the other replier. The refresh must refuse with the other device's
// reason (the page says FailOtherDevice), mark the topic, ask nothing at the address and cache nothing.
func TestAReplyWithoutAMACOnTheTopicIsAnotherDevice(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "http.example"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	router := p.Reads.(*transport.Router)
	router.SetMQTT(noMACTopic{brokerMQTT{dev: onTopic}})
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ReadsOverMQTT(t.Context(), d) {
		t.Fatal("the router does not read the device over MQTT: the rig does not show the case it is about")
	}
	set := settings.New(settings.Options{Devices: st, Client: router, ReadTimeout: 5 * time.Second, Verify: p.VerifyRead})
	a := &runtime{svc: &Services{Prober: p}, settings: set}
	if err := a.refreshData(t.Context(), d); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Errorf("refreshData = %v, want errAnotherDeviceOnTopic", err)
	}
	if p.Topics.Since(id, "plug").IsZero() {
		t.Error("the topic something else answers on is not marked: the device's writes still reach it")
	}
	if got := atAddr.asked(); len(got) != 0 {
		t.Errorf("asked %v at the address after the topic showed another device", got)
	}
	if snap, ok := set.Last(id); ok {
		t.Errorf("the Settings cache holds %d values", len(snap.Values))
	}
}

// Why (#804): the check before the data refresh and the broker read heard every reply only for a topic already marked;
// for every other device read over MQTT, a check that learnt nothing in the window (nobody answered in it, the window was
// cut short) fell through to the routed check, which went to the topic again and heard one reply, whichever device
// answered first, and pinned the reads to the topic. Nothing vouches for the topic then: the check goes to the address
// (pinned to HTTP, so it is answered there or not at all) and the reads follow it there; with HTTP failing too, nothing
// is read.
func TestAnUnmarkedTopicTheFullCheckLearntNothingOnIsNotRead(t *testing.T) {
	setup := func(t *testing.T, atAddr *brokerAnswers) (*Prober, model.Device, *brokerAnswers) {
		t.Helper()
		onTopic := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
		p.Reads.(*transport.Router).SetMQTT(noCollectTopic{brokerMQTT{dev: onTopic}})
		d, err := st.GetDevice(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !p.ReadsOverMQTT(t.Context(), d) {
			t.Fatal("the router does not read the device over MQTT: the rig does not show the case it is about")
		}
		return p, d, onTopic
	}
	t.Run("HTTP fails: refused", func(t *testing.T) {
		p, d, onTopic := setup(t, &brokerAnswers{mac: storedMAC, host: "http.example", down: true})
		if _, err := p.verifyAndPin(t.Context(), d); err == nil {
			t.Fatal("verifyAndPin = nil: the reads were vouched for by one reply on the topic")
		}
		if err := p.ReadBroker(t.Context(), d.ID); err == nil {
			t.Error("ReadBroker = nil, want the refusal")
		}
		if got := onTopic.asked(); slices.Contains(got, "Status 6") {
			t.Errorf("asked %v on the topic after a check that vouched for nothing", got)
		}
		if d, _ := p.Devices.GetDevice(t.Context(), d.ID); d.MQTTHost != "old.example" {
			t.Errorf("stored broker %q, want the stored one kept", d.MQTTHost)
		}
	})
	t.Run("HTTP works: the reads go to the address", func(t *testing.T) {
		atAddr := &brokerAnswers{mac: storedMAC, host: "http.example"}
		p, d, onTopic := setup(t, atAddr)
		pctx, err := p.verifyAndPin(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		if pin, ok := transport.PathFrom(pctx); !ok || pin != transport.HTTP {
			t.Errorf("the reads are pinned to %v (pinned %v), want HTTP, where the check was answered", pin, ok)
		}
		if got := onTopic.asked(); len(got) != 0 {
			t.Errorf("asked %v on the topic with a single read after the check that hears every reply learnt nothing", got)
		}
		if got := atAddr.asked(); !slices.Equal(got, []string{"Status 5"}) {
			t.Errorf("asked %v at the address, want the check", got)
		}
	})
}

// Why (#804, design check): ReadNet's Status 5 is its own check, and the network facts it stores come from that reply.
// Over MQTT it heard one reply, so with a second board on the topic it stored whichever answered first and marked
// nothing. It must hear every reply: two replies store nothing and mark the topic.
func TestReadNetStoresNothingWhenTwoDevicesAnswerOnTheTopic(t *testing.T) {
	g := newSharedRig(t, otherMAC)
	if err := g.p.ReadNet(t.Context(), g.d.ID); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Errorf("ReadNet = %v, want errAnotherDeviceOnTopic", err)
	}
	if d, _ := g.p.Devices.GetDevice(t.Context(), g.d.ID); !d.Net.ReadAt.IsZero() {
		t.Errorf("network facts %+v stored from a topic two devices answer on", d.Net)
	}
	if !g.marked() {
		t.Error("the topic two devices answer on is not marked")
	}
	if asked := g.topic.asked(); slices.Contains(asked, "Status 11") {
		t.Errorf("asked %v on the topic after the check refused", asked)
	}
}

// Why (#804): the routed check is what a device gets whose reads the router did not send over MQTT when the prober
// asked (ReadsOverMQTT); the router decides again for the check itself, and its facts can change in between (the last
// will comes Online, an HTTP failure is recorded). If that check then goes to the topic it hears one reply, whichever
// device answered first. Such an answer must vouch for nothing there: the data refresh, the broker read and ReadNet
// refuse instead of pinning their reads to the topic or storing the facts of that reply.
func TestARoutedCheckAnsweredOnTheTopicAloneVouchesForNothing(t *testing.T) {
	setup := func(t *testing.T) (*Prober, model.Device, *sharedTopic) {
		t.Helper()
		g := newSharedRig(t, otherMAC)
		g.p.ReadsOverMQTT = func(context.Context, model.Device) bool { return false } // decided before the facts changed
		return g.p, g.d, g.topic
	}
	t.Run("the check before the reads", func(t *testing.T) {
		p, d, topic := setup(t)
		if _, err := p.verifyAndPin(t.Context(), d); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("verifyAndPin = %v, want the refusal: one reply on the topic vouched for the reads", err)
		}
		if asked := topic.asked(); !slices.Equal(asked, []string{"Status 5"}) {
			t.Errorf("asked %v on the topic, want only the check", asked)
		}
	})
	t.Run("ReadNet", func(t *testing.T) {
		p, d, topic := setup(t)
		if err := p.ReadNet(t.Context(), d.ID); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("ReadNet = %v, want the refusal", err)
		}
		if got, _ := p.Devices.GetDevice(t.Context(), d.ID); !got.Net.ReadAt.IsZero() {
			t.Errorf("network facts %+v stored on one reply on the topic", got.Net)
		}
		if asked := topic.asked(); slices.Contains(asked, "Status 11") {
			t.Errorf("asked %v on the topic after the check vouched for nothing", asked)
		}
	})
}
