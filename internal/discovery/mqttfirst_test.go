package discovery

import (
	"bufio"
	"context"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// MQTT-first discovery (spec 11.3a, issue #47): devices that publish no
// tasmota/discovery config are found from their own topics.

// fakeBus records the commands discovery publishes.
type fakeBus struct {
	mu   sync.Mutex
	cmds []string // "<topic> <command> <payload>"
	subs map[int]func(mqttbus.Message)
	next int
	err  error
}

func (b *fakeBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]func(mqttbus.Message){}
	}
	id := b.next
	b.next++
	b.subs[id] = h
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *fakeBus) Command(_ context.Context, topic, command, payload string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.cmds = append(b.cmds, topic+" "+command+" "+payload)
	return nil
}

func (b *fakeBus) Connected() bool { return true }

func (b *fakeBus) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cmds...)
}

// manualClock is a clock the test moves.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type mqttEnv struct {
	*env
	bus   *fakeBus
	clk   *manualClock
	unsub func()
}

func newMQTTEnv(t *testing.T, mut func(*Options)) *mqttEnv {
	t.Helper()
	clk := &manualClock{t: time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)}
	e := newEnv(t, "172.31.0.0/16", func(o *Options) {
		o.Clock = clk
		if mut != nil {
			mut(o)
		}
	})
	b := &fakeBus{}
	me := &mqttEnv{env: e, bus: b, clk: clk}
	me.unsub = e.mgr.Attach(b)
	return me
}

// fixture reads the synthetic MQTT-only capture into decoded messages.
func fixtureMessages(t *testing.T) []mqttbus.Message {
	t.Helper()
	f, err := os.Open("../../testdata/fixtures/mqtt/synthetic-no-discovery.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only fixture
	codec, _ := mqttbus.NewTopicCodec("", [3]string{})
	var out []mqttbus.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		kind, topic, suffix, ok := codec.Decode(parts[2])
		if !ok {
			t.Fatalf("fixture topic %q does not decode", parts[2])
		}
		out = append(out, mqttbus.Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: []byte(parts[3]), Retained: parts[1] == "1"})
	}
	return out
}

func pick(msgs []mqttbus.Message, topic string, kind mqttbus.Kind) []mqttbus.Message {
	var out []mqttbus.Message
	for _, m := range msgs {
		if m.Topic == topic && (kind == "" || m.Kind == kind) {
			out = append(out, m)
		}
	}
	return out
}

func (e *mqttEnv) feed(msgs ...mqttbus.Message) {
	for _, m := range msgs {
		e.mgr.handleMessage(bg, m)
	}
}

func onlyNew(t *testing.T, e *env) Proposal {
	t.Helper()
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindNew {
		t.Fatalf("proposals = %+v, want one new", p)
	}
	return p[0]
}

// Why: the reporter's missing devices (#47) publish LWT, STATE and INFO but no
// tasmota/discovery config. Discovery must ask such a topic for its MAC with
// one individual Status 5 (never Status 0, safety rule 2) and propose the
// device from the answer, with the topic MQTT saw.
func TestTopicWithoutDiscoveryConfigIsAskedForItsMACAndProposed(t *testing.T) {
	e := newMQTTEnv(t, nil)
	msgs := fixtureMessages(t)
	all := pick(msgs, "garage-door", "")
	e.feed(all[:4]...) // LWT, INFO1..3
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("LWT and INFO carry no MAC, yet proposals = %+v", p)
	}
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 1 || got[0] != "garage-door STATUS 5" {
		t.Fatalf("commands = %q, want one Status 5 to garage-door", got)
	}
	e.feed(all[4]) // the STATUS5 answer
	p := onlyNew(t, e.env)
	if p.Via != ViaMQTT || p.Device.MAC != "02:00:5e:a1:b2:c3" || p.Device.Topic != "garage-door" || p.Device.Address != "172.31.13.41" || p.Device.Name != "garage-door" {
		t.Fatalf("proposal = %+v", p)
	}
	if !e.audit.has("discovery.identify") {
		t.Error("the automated Status 5 must be audited (which topic, why)")
	}
	for _, c := range e.bus.sent() {
		if strings.HasSuffix(c, " 0") || strings.HasSuffix(c, "STATUS ") {
			t.Errorf("discovery sent %q: Status 0 is never sent by automated code", c)
		}
	}
}

// Why: an ESP32 on Ethernet reports no Wi-Fi address; the proposal must carry
// the Ethernet address, and the device must still be recognised by either MAC.
func TestEthernetDeviceIsProposedAtItsEthernetAddress(t *testing.T) {
	e := newMQTTEnv(t, nil)
	msgs := pick(fixtureMessages(t), "workshop-eth", "")
	e.feed(msgs[:2]...)
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.feed(msgs[2])
	p := onlyNew(t, e.env)
	if p.Device.Address != "172.31.23.40" || p.Device.MAC != "02:00:5e:e7:40:00" {
		t.Fatalf("proposal = %+v, want the Ethernet IP and the Wi-Fi MAC", p)
	}

	// Stored with its Ethernet MAC only: the same device, no proposal.
	e2 := newMQTTEnv(t, nil)
	if _, err := e2.inv.Create(bg, "t", model.Device{Name: "eth", Address: "172.31.23.40", MAC: "02:00:5e:e7:40:01"}); err != nil {
		t.Fatal(err)
	}
	e2.feed(msgs[:2]...)
	e2.clk.add(askGrace)
	e2.mgr.askDue(bg)
	e2.feed(msgs[2])
	if p := mustProposals(t, e2.env); len(p) != 0 {
		t.Fatalf("a device known by its Ethernet MAC came back as %+v", p)
	}
}

// Why: an offline device cannot answer; asking it is noise. It is listed as
// seen but not identified, so "on the broker but not in the list" is visible.
func TestOfflineTopicIsNotAskedButListed(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(pick(fixtureMessages(t), "attic-fan", "")...)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked an offline topic: %q", got)
	}
	u := e.mgr.Unidentified()
	if len(u) != 1 || u[0].Topic != "attic-fan" || u[0].Online || !strings.Contains(u[0].Reason, "offline") {
		t.Fatalf("unidentified = %+v", u)
	}
}

// Why: Tasmota publishes its discovery config a few seconds after the LWT. A
// device that is about to announce itself must not be asked first.
func TestFreshlyOnlineTopicWaitsForItsDiscoveryConfig(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "couch", Payload: []byte("Online")})
	e.mgr.askDue(bg)
	e.clk.add(askGrace / 2)
	e.feed(discoveryMsg("02005EEA7CA3", "172.31.13.29", "couch"))
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked a device that announced itself: %q", got)
	}
}

// Why: a flood of fake LWTs must not become a command flood: one command per
// second for the whole bus, one per topic per 10 minutes while unresolved.
func TestStatusQueriesAreRateLimited(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for _, topic := range []string{"a", "b"} {
		e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: topic, Payload: []byte("Online"), Retained: true})
	}
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 1 {
		t.Fatalf("%d commands within a second, want 1", n)
	}
	e.clk.add(askGap)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("%d commands after the gap, want 2", n)
	}
	e.clk.add(askGap)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 2 {
		t.Fatalf("a topic was asked again within %v: %q", askEvery, e.bus.sent())
	}
	e.clk.add(askEvery)
	e.mgr.askDue(bg)
	if n := len(e.bus.sent()); n != 3 {
		t.Fatalf("an unresolved topic must be asked again after %v: %q", askEvery, e.bus.sent())
	}
}

// Why: Tasmota mirrors every HTTP command reply on MQTT, so tasmofleet's own
// probes produce STATUS5 messages for inventory devices. Only replies for
// topics discovery asked may become candidates.
func TestUnaskedStatus5IsIgnored(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(pick(fixtureMessages(t), "garage-door", mqttbus.KindStatus)...)
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("an unasked STATUS5 became %+v", p)
	}
}

// Why: topics an inventory device or a discovery config already accounts for
// need no question.
func TestKnownTopicsAreNotAsked(t *testing.T) {
	e := newMQTTEnv(t, nil)
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online"), Retained: true})
	e.feed(discoveryMsg("02005EEA7CA3", "172.31.13.29", "couch"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "couch", Payload: []byte("Online"), Retained: true})
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("asked known topics: %q", got)
	}
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Fatalf("known topics listed as unidentified: %+v", u)
	}
}

// Why: both sources resolve to one proposal by MAC and must not race: a scan
// sighting of a device MQTT saw keeps its topic (and source), and contributes
// the HTTP port; a later MQTT sighting keeps that port.
func TestScanAndMQTTSightingsMergeByMAC(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	scan := candidate{via: ViaScan, mac: "02:00:5e:d2:2c:14", ip: mustAddr("172.31.13.31"), port: 8080, name: "Lamp lamp"}
	if _, err := e.mgr.consider(bg, scan); err != nil {
		t.Fatal(err)
	}
	p := onlyNew(t, e.env)
	if p.Device.Topic != "lamp" || p.Device.FullTopic == "" || p.Via != ViaMQTT || p.Device.Port != 8080 {
		t.Fatalf("after the scan: %+v", p)
	}
	e.clk.add(minMessageGap)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	if p := onlyNew(t, e.env); p.Device.Port != 8080 || p.Device.Topic != "lamp" {
		t.Fatalf("after the next MQTT sighting: %+v", p)
	}
}

// Why: #45, adding an offline device seen on MQTT must not depend on an HTTP
// probe. The device is stored with the MAC and topic MQTT saw, so MQTT
// matches it when it comes back.
func TestOfflineMQTTDeviceIsAcceptedWithoutProbe(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	p := onlyNew(t, e.env)
	if p.LWT != "Offline" {
		t.Fatalf("proposal LWT = %q, want Offline", p.LWT)
	}
	id, err := e.mgr.Accept(bg, "admin", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.ver.calls.Load(); n != 0 {
		t.Errorf("an offline device was probed %d times at accept", n)
	}
	d, _ := e.inv.Get(bg, id)
	if d.MAC != "02:00:5e:d2:2c:14" || d.Topic != "lamp" || d.Address != "172.31.13.31" {
		t.Fatalf("device = %+v", d)
	}
}

// Why: the fleet default password goes only to an address that proved the
// MAC (security review 2026-10-22); an offline device cannot, so it fails closed.
func TestOfflineAcceptWithDefaultPasswordStillNeedsProof(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	_, err := e.mgr.Accept(WithDevicePassword(bg, "fake-default"), "admin", onlyNew(t, e.env).ID)
	if !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("err = %v, want ErrNotAcceptable", err)
	}
}

// Why: a device added while offline has no identity; it must be probed when
// it comes online, not only after a manual "Refresh identity", and not more
// than once per 10 minutes.
func TestDeviceWithoutIdentityIsProbedWhenItComesOnline(t *testing.T) {
	var mu sync.Mutex
	var probed []model.DeviceID
	e := newMQTTEnv(t, func(o *Options) {
		o.OnAccepted = func(id model.DeviceID) { mu.Lock(); probed = append(probed, id); mu.Unlock() }
	})
	id, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	if err != nil {
		t.Fatal(err)
	}
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Offline"), Retained: true})
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp", Payload: []byte("Online")})
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	mu.Lock()
	got := append([]model.DeviceID(nil), probed...)
	mu.Unlock()
	if len(got) != 1 || got[0] != id {
		t.Fatalf("probes = %v, want one of %d", got, id)
	}
	e.clk.add(reprobeEvery)
	e.feed(mqttbus.Message{Kind: mqttbus.KindState, Topic: "lamp", Payload: []byte(`{}`)})
	mu.Lock()
	n := len(probed)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("probes after %v = %d, want 2", reprobeEvery, n)
	}
}

// Why: the MQTT discovery setting switches the whole MQTT side off, the
// automated Status 5 included.
func TestDetachStopsQueries(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "a", Payload: []byte("Online"), Retained: true})
	e.unsub()
	e.mgr.askDue(bg)
	if got := e.bus.sent(); len(got) != 0 {
		t.Fatalf("commands after detach: %q", got)
	}
	if u := e.mgr.Unidentified(); len(u) != 0 {
		t.Fatalf("unidentified after detach: %+v", u)
	}
}

// Why: the unresolved-topic table is bounded; fake topics cannot grow it.
func TestUnidentifiedTopicsAreBounded(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for i := 0; i < maxTopics+10; i++ {
		e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "t" + itoa(i), Payload: []byte("Offline"), Retained: true})
	}
	if n := len(e.mgr.Unidentified()); n != maxTopics {
		t.Fatalf("%d topics kept, want %d", n, maxTopics)
	}
}

// Why: an address outside DEVICE_NETWORKS could never be added; it must say
// so instead of disappearing silently.
func TestAnswerOutsideNetworksIsListedWithReason(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "far", Payload: []byte("Online"), Retained: true})
	e.clk.add(askGrace)
	e.mgr.askDue(bg)
	e.feed(mqttbus.Message{Kind: mqttbus.KindStatus, Topic: "far", Suffix: "STATUS5",
		Payload: []byte(`{"StatusNET":{"Hostname":"far","IPAddress":"10.9.9.9","Mac":"02:00:5E:99:99:99"}}`)})
	if p := mustProposals(t, e.env); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
	u := e.mgr.Unidentified()
	if len(u) != 1 || !strings.Contains(u[0].Reason, "outside the device networks") {
		t.Fatalf("unidentified = %+v", u)
	}
}

var _ store.DiscoveryStore = (*store.SQLite)(nil)

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func itoa(i int) string { return strconv.Itoa(i) }
