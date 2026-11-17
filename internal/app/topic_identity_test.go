package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #852: another device on a device's MQTT topic marks the device in the router, and #682: the identity probe reads
// the device over MQTT first once the check that hears every reply on its topic heard it alone with a stored MAC. Both
// drive the real router as the app wires it.

// setMAC changes the MAC the device on the topic or at the address reports.
func (b *brokerAnswers) setMAC(mac string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mac = mac
}

// readOver sends one read through the router and says over which transport it was answered.
func readOver(t *testing.T, p *Prober, id model.DeviceID, onTopic, atAddr *brokerAnswers) string {
	t.Helper()
	topicBefore, addrBefore := len(onTopic.asked()), len(atAddr.asked())
	if _, err := p.Reads.Command(t.Context(), tasmota.TargetFor(model.Device{ID: id, Address: "192.0.2.1"}), "Status 6"); err != nil {
		t.Fatal(err)
	}
	switch {
	case len(onTopic.asked()) > topicBefore && len(atAddr.asked()) == addrBefore:
		return "mqtt"
	case len(atAddr.asked()) > addrBefore && len(onTopic.asked()) == topicBefore:
		return "http"
	}
	t.Fatalf("the read went over both transports: topic %v, address %v", onTopic.asked(), atAddr.asked())
	return ""
}

// Why (#852): when another device answers on a device's topic, every command the router sends there reaches that
// device: the reads it answers, the writes it runs. Before #852 the broker read saw it and only logged it, and the next
// command went to the topic again. The router must hold the device off the topic until the topic is the device's own
// again: its own MAC answering there (the other device left), or a changed stored topic. A topic still taken keeps the
// mark, and the check that keeps it asks nothing at the address.
func TestAForeignMACOnTheTopicSendsTheDevicesNextCommandsOverHTTP(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	if got := readOver(t, p, id, onTopic, atAddr); got != "mqtt" {
		t.Fatalf("before any check the read went over %s, want MQTT (the device is usable there)", got)
	}
	if err := p.ReadBroker(t.Context(), id); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Fatalf("ReadBroker = %v, want errAnotherDeviceOnTopic", err)
	}
	if got := readOver(t, p, id, onTopic, atAddr); got != "http" {
		t.Fatalf("after another device answered on the topic the next read went over %s, want HTTP", got)
	}

	t.Run("still taken: the mark stays", func(t *testing.T) {
		addr := len(atAddr.asked())
		if err := p.ReadBroker(t.Context(), id); !errors.Is(err, errAnotherDeviceOnTopic) {
			t.Fatalf("ReadBroker = %v, want errAnotherDeviceOnTopic: the check must ask the topic itself", err)
		}
		if got := atAddr.asked()[addr:]; len(got) != 0 {
			t.Errorf("asked at the address %v while the topic answered", got)
		}
		if got := readOver(t, p, id, onTopic, atAddr); got != "http" {
			t.Errorf("read over %s, want HTTP while the topic is taken", got)
		}
	})
	t.Run("the other device left: the mark ends", func(t *testing.T) {
		onTopic.setMAC(storedMAC)
		if err := p.ReadBroker(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if got := readOver(t, p, id, onTopic, atAddr); got != "mqtt" {
			t.Errorf("read over %s after the device answered on its topic again, want MQTT", got)
		}
	})
	t.Run("the stored topic changed: the mark of the old one does not hold", func(t *testing.T) {
		onTopic.setMAC(otherMAC)
		if err := p.ReadBroker(t.Context(), id); !errors.Is(err, errAnotherDeviceOnTopic) {
			t.Fatalf("ReadBroker = %v, want errAnotherDeviceOnTopic", err)
		}
		d, err := st.GetDevice(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		d.Topic = "plug-new"
		if err := st.UpdateDevice(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if got := readOver(t, p, id, onTopic, atAddr); got != "mqtt" {
			t.Errorf("read over %s on the device's new topic, want MQTT", got)
		}
	})
}

// Why (#852): the Settings tab and the policy poller check after their reads (VerifyRead). A foreign MAC answered there
// over MQTT is the same finding as the broker read's and must hold the device off the topic too.
func TestAForeignMACAfterAReadOverMQTTMarksTheTopic(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	tctx, trace := transport.WithTrace(t.Context())
	if _, err := p.Reads.Command(tctx, tasmota.TargetFor(d), "Status 6"); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyRead(t.Context(), d, trace); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Fatalf("VerifyRead = %v, want errAnotherDeviceOnTopic", err)
	}
	if got := readOver(t, p, id, onTopic, atAddr); got != "http" {
		t.Errorf("read over %s after the check found another device on the topic, want HTTP", got)
	}
}

// Why (#852): the data refresh is the only background visit a device whose HTTP API is off gets, so its log line is
// where a person learns that another device answers on the device's topic. It said "another device answers at the
// address" at Debug, which names the wrong place and is not seen. A foreign MAC at the address stays a Debug line (a
// DHCP lease handed on).
func TestTheDataRefreshWarnsOfAnotherDeviceOnTheTopic(t *testing.T) {
	visit := func(t *testing.T, onTopic, atAddr *brokerAnswers, lwt string) string {
		t.Helper()
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, lwt)
		d, err := st.GetDevice(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		r := newBrokerHostReader(nil, nil, p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
			slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		r.readData = func(ctx context.Context, d model.Device) error { _, err := p.verifyAndPin(ctx, d); return err }
		if !r.readDataOf(t.Context(), d) {
			t.Fatal("the data was not read")
		}
		return buf.String()
	}
	t.Run("on the topic", func(t *testing.T) {
		log := visit(t, &brokerAnswers{mac: otherMAC}, &brokerAnswers{mac: storedMAC}, "Online")
		if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "another device answers on the device's MQTT topic") || !strings.Contains(log, "topic=plug") {
			t.Errorf("log %q, want a warning that names the topic", log)
		}
		if strings.Contains(log, "at the address") {
			t.Errorf("log %q names the address for a finding on the topic", log)
		}
	})
	t.Run("at the address", func(t *testing.T) {
		log := visit(t, &brokerAnswers{mac: storedMAC}, &brokerAnswers{mac: otherMAC}, "Offline")
		if strings.Contains(log, "level=WARN") || !strings.Contains(log, "another device answers at the address") {
			t.Errorf("log %q, want a debug line that names the address", log)
		}
	})
}

// Why (#852): the mark ends only when a check on the topic finds the device's own MAC again, so the broker read of a
// device whose topic is taken must come back soon (brokerHostRetry), not after the six hours of the periodic read:
// otherwise the device stays on HTTP for hours after the topic was fixed.
func TestATakenTopicIsAskedAgainSoon(t *testing.T) {
	p, _, id := brokerReadOverRouter(t, &brokerAnswers{mac: otherMAC}, &brokerAnswers{mac: storedMAC}, "Online")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r := newBrokerHostReader(nil, nil, p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
		slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	if refused, err := r.readLocked(t.Context(), id, visitWhat{broker: true}); err != nil || !refused {
		t.Fatalf("readLocked = %v, %v; want refused", refused, err)
	}
	if got := r.next[id]; !got.Equal(now.Add(brokerHostRetry)) {
		t.Errorf("next broker read at %v, want %v (in %v)", got, now.Add(brokerHostRetry), brokerHostRetry)
	}
}

// Why (#852, review): the data refresh is the only background visit of a device whose HTTP API is off (the broker read
// needs HTTP), so for that device it is the check that ends the mark once the topic is fixed. While the mark holds and
// HTTP fails, its writes, power toggle, backups and restores have no path at all: a data refresh that found the topic
// taken must come back as soon as the broker read does (brokerHostRetry), not after the six hours of the broker's
// interval.
func TestADataRefreshThatFindsATakenTopicComesBackSoon(t *testing.T) {
	p, st, id := brokerReadOverRouter(t, &brokerAnswers{mac: otherMAC}, &brokerAnswers{mac: storedMAC}, "Online")
	p.Reads.(*transport.Router).Memory().Fail(id, transport.HTTPFailAPIRefused)
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r := newBrokerHostReader(nil, nil, p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
		slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }
	r.readData = func(ctx context.Context, d model.Device) error { _, err := p.verifyAndPin(ctx, d); return err }
	if !r.readDataOf(t.Context(), d) {
		t.Fatal("the data was not read")
	}
	if p.Topics.Since(id, d.Topic).IsZero() {
		t.Fatal("the topic is not marked: the test does not show the case it is about")
	}
	if got := r.dataNext[id]; !got.Equal(now.Add(brokerHostRetry)) {
		t.Errorf("next data refresh at %v, want %v (in %v)", got, now.Add(brokerHostRetry), brokerHostRetry)
	}
}

// probeTopic is the device on its MQTT topic as the identity probe sees it: it reports mac, an identity (16.1.0, a
// 4 MB flash, the German build), a broker, GroupTopic, OTA URL and DeviceName that differ from what the device at the
// address reports, so the stored values show which transport carried them. silent names commands nobody answers;
// second, when set, is the MAC of a second device that answers every Status 5 on the topic too; noReply: the check that
// hears every reply hears nothing; onWindow runs inside that check's window, after the replies came. onAsk runs after
// each answered command.
type probeTopic struct {
	mu       sync.Mutex
	mac      string
	second   string
	noReply  bool
	silent   map[string]bool
	status   string // the reply to the plain Status; "" is the device's own, with its topic "plug"
	status2  string // the reply to Status 2; "" is topicStatus2
	onAsk    func(cmd string)
	onWindow func()
	got      []string
}

const (
	topicStatus2         = `{"StatusFWR":{"Version":"16.1.0(release-tasmota)","Hardware":"ESP8266EX"}}`
	topicStatus2Minimal  = `{"StatusFWR":{"Version":"16.0.0(release-minimal)","Hardware":"ESP8266EX"}}`
	topicStatus2Safeboot = `{"StatusFWR":{"Version":"16.0.0(release-safeboot)","Hardware":"ESP32-D0WD-V3"}}`
	topicStatus4         = `{"StatusMEM":{"ProgramFlashSize":4096,"FlashSize":4096,"Free":1500,"Features":["0407","8F9AC787"]}}`
)

func (m *probeTopic) Connected() bool { return true }
func (m *probeTopic) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	raw, err := m.answer(cmd)
	if err == nil && m.onAsk != nil {
		m.onAsk(cmd)
	}
	return raw, err
}

func (m *probeTopic) answer(cmd string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, cmd)
	if m.silent[cmd] {
		return nil, tasmota.ErrTimeout
	}
	switch cmd {
	case "Status 5":
		return mac5(m.mac), nil
	case "Status 2":
		if m.status2 != "" {
			return json.RawMessage(m.status2), nil
		}
		return json.RawMessage(topicStatus2), nil
	case "Status 4":
		return json.RawMessage(topicStatus4), nil
	case "Status 6":
		return json.RawMessage(`{"StatusMQT":{"MqttHost":"mqtt.home","MqttPort":1883}}`), nil
	case "SetOption3":
		return json.RawMessage(`{"SetOption3":"ON"}`), nil
	case "Status 1":
		return json.RawMessage(`{"StatusPRM":{"OtaUrl":"http://ota.example/topic.bin","GroupTopic":"topicgroup"}}`), nil
	case "Status":
		if m.status != "" {
			return json.RawMessage(m.status), nil
		}
		return json.RawMessage(`{"Status":{"DeviceName":"Topic name","Topic":"plug","FriendlyName":["Topic relay"]}}`), nil
	}
	return nil, tasmota.ErrUnknownCommand
}
func (m *probeTopic) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}

// DoAll hears every reply within the window, as the production channel does (transport.MQTTMultiReader).
func (m *probeTopic) DoAll(ctx context.Context, d model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	if m.noReply {
		m.mu.Lock()
		m.got = append(m.got, cmd)
		m.mu.Unlock()
		return nil, tasmota.ErrTimeout
	}
	raw, err := m.Do(ctx, d, cmd)
	if err != nil {
		return nil, err
	}
	replies := []json.RawMessage{raw}
	if m.second != "" && cmd == "Status 5" {
		replies = append(replies, mac5(m.second))
	}
	if m.onWindow != nil {
		m.onWindow() // as the production channel: an ended context returns the replies heard so far, without an error
	}
	return replies, nil
}
func (m *probeTopic) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.got...)
}

// probeAddress records what the identity probe asks the device at the address, the main page included.
type probeAddress struct {
	tasmota.Client
	mu      sync.Mutex
	got     []string
	onIdent func() // runs after Status 4, the last read of the identity of full firmware (#686: no page)
}

func (h *probeAddress) note(c string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, c)
}
func (h *probeAddress) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	h.note(c)
	raw, err := h.Client.Command(ctx, t, c)
	if c == "Status 4" && h.onIdent != nil {
		h.onIdent()
	}
	return raw, err
}
func (h *probeAddress) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	h.note("main page")
	return h.Client.RootPage(ctx, t)
}
func (h *probeAddress) asked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.got...)
}

// probeOverRouter stores the simulated device (MAC of status5Net) on our broker under topic "plug" and returns a prober
// wired as the app wires it: the router's raw HTTP client, the router for the reads, its topic marks, and the router's
// read rule (runtime.readsOverMQTT over the same facts).
func probeOverRouter(t *testing.T, onTopic *probeTopic, lwt string) (*Prober, *probeAddress, model.DeviceID, func(model.DeviceID) model.Device) {
	t.Helper()
	sim, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	st := openBrokerStore(t)
	d.Name, d.Topic, d.MQTT, d.MQTTHost, d.MQTTPort = "plug", "plug", model.MQTTOurs, "old.example", 1883
	d.MAC, d.MACs = "aa:bb:cc:00:00:01", []string{"aa:bb:cc:00:00:01"}
	id, err := st.CreateDevice(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	addr := &probeAddress{Client: override{Client: sim.Client, reply: map[string]string{"Status 5": status5Net}}}
	router := transport.NewRouter(transport.Options{HTTP: addr, Devices: st})
	facts := &TransportFacts{Devices: st, Health: fakeFactsHealth{id: {LWT: lwt}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		ConnChanged: router.MQTTConnChanged(), TopicTaken: router.TopicTaken(),
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now}
	router.SetFacts(facts)
	router.SetMQTT(onTopic)
	rt := &runtime{facts: facts}
	p := &Prober{Client: router.HTTPOnly(), Reads: router, Devices: st, Gap: time.Millisecond, Broker: fixedBroker{host: "mqtt.home"},
		Topics: router.TopicTaken(), ReadsOverMQTT: rt.readsOverMQTT}
	get := func(id model.DeviceID) model.Device {
		got, err := st.GetDevice(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	return p, addr, id, get
}

// Why (#682, the product owner's transport rule): MQTT carries every read it can for a device usable on TasmoFleet's
// broker, HTTP only what MQTT technically cannot. The identity (Status 2, Status 4) went to the address in every case,
// so a device whose HTTP API was switched off after its first probe kept the version, chip, flash and language of that
// probe forever. Now the probe of a stored device on our broker goes over MQTT first, identity and facts, after the
// check that hears every reply on its topic heard it alone with a stored MAC (the identity selects firmware, so a
// check that hears whichever device answers first is not enough, #852). Nothing reaches the address then. Without
// that (MQTT not usable, another device on the topic, MQTT silent on a later read, a Status 2 on the topic that claims
// a minimal or safeboot image, which answers neither Status 5 nor Status 2 and would change how updates and the broker
// reader treat the device) the whole probe runs at the address, whose own Status 5 vouches for it, and nothing of the
// MQTT half is kept: another device on the topic marks it.
func TestTheIdentityOfADeviceOnOurBrokerIsReadOverMQTTFirst(t *testing.T) {
	atAddress := []string{"Status 5", "Status 2", "Status 4", "Status 6", "SetOption3", "Status 1", "Status"}
	onTopic := []string{"Status 5", "Status 2", "Status 4", "Status 6", "SetOption3", "Status 1", "Status"}
	for _, c := range []struct {
		name        string
		topic       *probeTopic
		lwt         string
		wantAddress []string
		wantTopic   []string
		overMQTT    bool // the stored identity and facts are the topic's
		marked      bool
	}{
		{"the stored device on its topic", &probeTopic{mac: "AA:BB:CC:00:00:01"}, "Online", nil, onTopic, true, false},
		{"MQTT not usable", &probeTopic{mac: "AA:BB:CC:00:00:01"}, "Offline", atAddress, nil, false, false},
		{"another device on the topic", &probeTopic{mac: otherMAC}, "Online", atAddress, []string{"Status 5"}, false, true},
		{"two devices answer on the topic", &probeTopic{mac: "AA:BB:CC:00:00:01", second: otherMAC}, "Online",
			atAddress, []string{"Status 5"}, false, true},
		{"the topic stops answering after the check", &probeTopic{mac: "AA:BB:CC:00:00:01", silent: map[string]bool{"Status 4": true}}, "Online",
			atAddress, []string{"Status 5", "Status 2", "Status 4"}, false, false},
		{"the topic stops answering in the facts", &probeTopic{mac: "AA:BB:CC:00:00:01", silent: map[string]bool{"Status 6": true}}, "Online",
			atAddress, []string{"Status 5", "Status 2", "Status 4", "Status 6"}, false, false},
		{"the topic claims a minimal image", &probeTopic{mac: "AA:BB:CC:00:00:01", status2: topicStatus2Minimal}, "Online",
			atAddress, []string{"Status 5", "Status 2", "Status 4"}, false, false},
		{"the topic claims a safeboot image", &probeTopic{mac: "AA:BB:CC:00:00:01", status2: topicStatus2Safeboot}, "Online",
			atAddress, []string{"Status 5", "Status 2", "Status 4"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, addr, id, get := probeOverRouter(t, c.topic, c.lwt)
			au := &idAudit{}
			p.Audit = au
			if err := p.Refresh(t.Context(), id, "admin"); err != nil {
				t.Fatal(err)
			}
			if got := addr.asked(); !slices.Equal(got, c.wantAddress) {
				t.Errorf("asked at the address: %v, want %v", got, c.wantAddress)
			}
			if got := c.topic.asked(); !slices.Equal(got, c.wantTopic) {
				t.Errorf("asked on the topic: %v, want %v", got, c.wantTopic)
			}
			d := get(id)
			if d.Identity.DetectedAt.IsZero() {
				t.Errorf("identity %+v: a probe stores an identity with its time, over either transport", d.Identity)
			}
			if v := d.Identity.Variant; v == "minimal" || v == "safeboot" {
				t.Errorf("stored variant %q: a minimal or safeboot image answers no Status 5 or Status 2 on its topic, so such an answer is not the device's", v)
			}
			fromTopic := d.Identity.Version == "16.1.0" || d.Identity.FlashSizeKB == 4096 || d.Identity.Language == "DE" ||
				d.MQTTHost == "mqtt.home" || d.GroupTopic == "topicgroup"
			if c.overMQTT {
				if d.Identity.Version != "16.1.0" || d.Identity.FlashSizeKB != 4096 || d.Identity.Language != "DE" || !d.Identity.LanguageKnown {
					t.Errorf("identity %+v: want the version, flash and language (Status 4 locale ID) the device reported on its topic", d.Identity)
				}
				if d.MQTTHost != "mqtt.home" || d.GroupTopic != "topicgroup" || d.OTAURL != "http://ota.example/topic.bin" {
					t.Errorf("stored broker %q, GroupTopic %q, OTA URL %q: want the facts the device reported on its topic", d.MQTTHost, d.GroupTopic, d.OTAURL)
				}
				if ev, ok := au.find("identity.mqtt_ask"); !ok || ev.Actor != "admin" || !strings.Contains(ev.Detail, `"via":"mqtt"`) {
					t.Errorf("audit %v: a person's refresh over MQTT names the person and the transport", au.actions())
				}
			} else if fromTopic {
				t.Errorf("stored identity %+v, broker %q, GroupTopic %q: a value of the MQTT half was kept after the probe went to the address",
					d.Identity, d.MQTTHost, d.GroupTopic)
			}
			if got := !p.Topics.Since(id, "plug").IsZero(); got != c.marked {
				t.Errorf("topic marked = %v, want %v", got, c.marked)
			}
		})
	}
}

// Why (#682): the audit keeps what a person (or discovery) asked a device over MQTT, as each question of the refresh
// over MQTT was audited before; the daily background refresh of every device would fill it with the same row.
func TestOnlyARefreshSomebodyStartedAuditsItsReadsOverMQTT(t *testing.T) {
	p, _, id, _ := probeOverRouter(t, &probeTopic{mac: "AA:BB:CC:00:00:01"}, "Online")
	au := &idAudit{}
	p.Audit = au
	if err := p.RefreshFacts(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := au.find("identity.mqtt_ask"); ok {
		t.Errorf("audit %v after the background refresh", au.actions())
	}
	if err := p.Refresh(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	if ev, ok := au.find("identity.mqtt_ask"); !ok || ev.Actor != "system" {
		t.Errorf("audit %v after discovery's refresh over MQTT", au.actions())
	}
}

// Why (#682, issue point 2): the identity over MQTT is stored only when the check that hears every reply on the
// topic heard exactly one reply, with a stored MAC. Here HTTP cannot reach the device, so MQTT is the only way: a
// second reply (a copied config next to the device), another MAC, no reply at all, or a window cut short (the page
// request ended while the check listened) must store nothing and ask nothing more on the topic, and the first two hold
// the device off its topic. One reply with the stored MAC is the device alone: it is stored, and a mark from earlier
// ends. A Status 2 after a passing check that claims a minimal or safeboot image (which answers neither Status 5 nor
// Status 2, so the answer is not the device's) stores nothing either: stored as minimal, the device would get another
// update path, the broker reader's limited-firmware rule and no firmware recheck.
func TestTheIdentityOverMQTTNeedsExactlyOneReplyWithAStoredMAC(t *testing.T) {
	for _, c := range []struct {
		name     string
		topic    *probeTopic
		cutShort bool // the caller's context ends inside the check's window
		markedBy bool // the device was held off its topic before
		stored   bool
		marked   bool
		checked  bool // the check found the device alone; a later answer is refused
	}{
		{"one reply with the stored MAC", &probeTopic{mac: "AA:BB:CC:00:00:01"}, false, false, true, false, true},
		{"one reply with the stored MAC ends a mark", &probeTopic{mac: "AA:BB:CC:00:00:01"}, false, true, true, false, true},
		{"two replies with the stored MAC", &probeTopic{mac: "AA:BB:CC:00:00:01", second: "AA:BB:CC:00:00:01"}, false, false, false, true, false},
		{"another MAC", &probeTopic{mac: otherMAC}, false, false, false, true, false},
		{"no reply", &probeTopic{mac: "AA:BB:CC:00:00:01", noReply: true}, false, false, false, false, false},
		{"no reply keeps a mark", &probeTopic{mac: "AA:BB:CC:00:00:01", noReply: true}, false, true, false, true, false},
		{"the window was cut short", &probeTopic{mac: "AA:BB:CC:00:00:01"}, true, false, false, false, false},
		{"Status 2 claims a minimal image", &probeTopic{mac: "AA:BB:CC:00:00:01", status2: topicStatus2Minimal}, false, false, false, false, true},
		{"Status 2 claims a safeboot image", &probeTopic{mac: "AA:BB:CC:00:00:01", status2: topicStatus2Safeboot}, false, false, false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, addr, id, get := probeOverRouter(t, c.topic, "Online")
			p.Client = unreachable{}
			p.Reads.(*transport.Router).Memory().Fail(id, transport.HTTPFailUnreachable) // the router knows HTTP fails
			if c.markedBy {
				p.Topics.Mark(id, "plug")
			}
			before := get(id).Identity
			ctx := t.Context()
			if c.cutShort {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				c.topic.onWindow = cancel // the page request ends while the check listens
			}
			err := p.Refresh(ctx, id, "admin")
			got := get(id).Identity
			if c.stored {
				if err != nil {
					t.Fatal(err)
				}
				if got.Version != "16.1.0" || got.DetectedAt.IsZero() {
					t.Errorf("identity %+v: the device alone on its topic was not identified over MQTT", got)
				}
			} else {
				if err == nil {
					t.Error("Refresh = nil, want the failure: nothing could vouch for the topic and HTTP cannot reach the device")
				}
				if got.Version != before.Version || got.Variant != before.Variant || !got.DetectedAt.Equal(before.DetectedAt) {
					t.Errorf("identity %+v stored, want %+v kept", got, before)
				}
				if asked := c.topic.asked(); !c.checked && slices.Contains(asked, "Status 2") {
					t.Errorf("asked %v on the topic after the check did not find the device alone", asked)
				}
			}
			if asked := c.topic.asked(); len(asked) == 0 || asked[0] != "Status 5" {
				t.Errorf("asked %v on the topic: the row never reached the check", asked)
			} else if c.checked && !slices.Contains(asked, "Status 2") {
				t.Errorf("asked %v on the topic: the check did not find the device alone, so the row does not show its case", asked)
			}
			if got := !p.Topics.Since(id, "plug").IsZero(); got != c.marked {
				t.Errorf("topic marked = %v, want %v", got, c.marked)
			}
			if got := addr.asked(); len(got) != 0 {
				t.Errorf("the HTTP client of the test asked %v: it must be the unreachable one", got)
			}
		})
	}
}

// Why (#682): the probe over MQTT took its answers from whoever listens on the topic it asked. A person who edits the
// topic while the probe reads (or a device moved, rebound or deleted meanwhile) must not get the old topic's answers
// stored on the device: the store writes only while the device still has the topic the check asked.
func TestAnIdentityReadOverMQTTIsNotStoredWhenTheTopicChangedMeanwhile(t *testing.T) {
	topic := &probeTopic{mac: "AA:BB:CC:00:00:01"}
	p, _, id, get := probeOverRouter(t, topic, "Online")
	before := get(id).Identity
	topic.onAsk = func(cmd string) {
		if cmd != "Status 4" {
			return
		}
		d := get(id)
		d.Topic = "plug-new"
		if err := p.Devices.(interface {
			UpdateDevice(context.Context, model.Device) error
		}).UpdateDevice(t.Context(), d); err != nil {
			t.Error(err)
		}
	}
	if err := p.Refresh(t.Context(), id, "admin"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Refresh = %v, want the store's conflict", err)
	}
	if got := get(id).Identity; got.Version == "16.1.0" || !got.DetectedAt.Equal(before.DetectedAt) {
		t.Errorf("identity %+v stored from the topic the device no longer has", got)
	}
}

// twoOnTopic is the command channel of a topic that the device on it (dev) and, while second is set, one more device
// listen on. Do hears the first reply only, as a single read does; DoAll (transport.MQTTMultiReader, the channel's
// collect-all) hears every reply within the window: dev's first, then the second device's.
type twoOnTopic struct {
	dev *brokerAnswers

	mu     sync.Mutex
	second string // the MAC of the second device; "" when it left the topic
}

func (m *twoOnTopic) setSecond(mac string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.second = mac
}
func (m *twoOnTopic) Connected() bool { return true }
func (m *twoOnTopic) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	return m.dev.answer(cmd)
}
func (m *twoOnTopic) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}
func (m *twoOnTopic) DoAll(_ context.Context, _ model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	first, err := m.dev.answer(cmd)
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

// Why (#852, review): the usual way another device gets onto a device's topic is a second board with the device's
// config (copied, restored) that answers next to it. Both answer every check, and a check that hears one reply hears
// whichever came first: the device's own reply ended the mark although the other board still listens, and the
// device's writes, power toggle and backups went to both boards until a later check happened to hear the other one.
// The mark may end only on a check that hears every reply on the topic and finds exactly one, the device's; the
// restore over MQTT already asks the same (UploadConfig).
func TestATopicTwoDevicesAnswerOnStaysMarked(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	ch := &twoOnTopic{dev: onTopic, second: otherMAC}
	p.Reads.(*transport.Router).SetMQTT(ch)
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.Topics.Mark(id, d.Topic)
	if _, err := p.verifyAndPin(t.Context(), d); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Fatalf("verifyAndPin = %v, want errAnotherDeviceOnTopic: a second device still answers on the topic", err)
	}
	if p.Topics.Since(id, d.Topic).IsZero() {
		t.Fatal("the device's own reply, heard first, ended the mark while another device still answers on the topic")
	}
	if got := readOver(t, p, id, onTopic, atAddr); got != "http" {
		t.Errorf("read over %s while two devices answer on the topic, want HTTP", got)
	}
	t.Run("two replies with the device's MAC: still not one device", func(t *testing.T) {
		ch.setSecond(storedMAC)
		if _, err := p.verifyAndPin(t.Context(), d); !errors.Is(err, errAnotherDeviceOnTopic) {
			t.Fatalf("verifyAndPin = %v, want errAnotherDeviceOnTopic", err)
		}
		if p.Topics.Since(id, d.Topic).IsZero() {
			t.Error("two replies ended the mark")
		}
	})
	t.Run("the other device left: the device alone on its topic ends the mark", func(t *testing.T) {
		ch.setSecond("")
		if _, err := p.verifyAndPin(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if !p.Topics.Since(id, d.Topic).IsZero() {
			t.Error("the device answered alone on its topic and is still held off it")
		}
	})
}

// Why (#852, review): while HTTP fails, a marked device's reads still go to its topic (MQTT is the only way left), and
// the check after them (VerifyRead) is answered there by whichever device replies first. A single reply with the
// device's MAC does not show that the other device left, so it must not end the mark: the device's writes would go to
// both devices. Since #608's review the pinned check is not even sent to the taken topic: a reply there vouches for
// nothing, so the check refuses the read (fail closed) instead of accepting whichever device answered first.
func TestACheckAfterAReadOverMQTTNeverEndsTheMark(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.Reads.(*transport.Router).Memory().Fail(id, transport.HTTPFailUnreachable)
	p.Topics.Mark(id, d.Topic)
	tctx, trace := transport.WithTrace(t.Context())
	if _, err := p.Reads.Command(tctx, tasmota.TargetFor(d), "Status 6"); err != nil {
		t.Fatal(err)
	}
	if via := trace.Via(); via != "mqtt" {
		t.Fatalf("the read went over %s, want mqtt: the test does not show the case it is about", via)
	}
	if err := p.VerifyRead(t.Context(), d, trace); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Errorf("VerifyRead = %v; want the check refused on the taken topic, so the read is not vouched for", err)
	}
	if p.Topics.Since(id, d.Topic).IsZero() {
		t.Error("the check after a read over MQTT ended the mark")
	}
}

// Why (#852, review): every MAC check answered on the topic alone is the same finding, wherever it runs. ReadNet's
// Status 5 is its own check and runs after every identity refresh; a foreign MAC there must hold the device off the
// topic like the broker read's, or the device's next commands go to the device it was just found not to be.
func TestReadNetMarksAnotherDeviceOnTheTopic(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, _, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	if err := p.ReadNet(t.Context(), id); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Fatalf("ReadNet = %v, want errAnotherDeviceOnTopic", err)
	}
	if got := readOver(t, p, id, onTopic, atAddr); got != "http" {
		t.Errorf("after ReadNet saw another MAC on the topic the next read went over %s, want HTTP", got)
	}
}

// Why (#852, review): the router asks the device's stored topic, which may differ from the snapshot a caller holds (a
// broker reader's list, minutes old; an edit or a refresh since). The mark belongs to the topic the check asked: taken
// from the stale snapshot, the check erased the mark of the stored topic and marked the old one, and the stored topic,
// where the other device was just heard, ended up unmarked.
func TestACheckWithAStaleSnapshotMarksTheTopicItAsked(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	stale, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	cur := stale
	cur.Topic = "plug-new"
	if err := st.UpdateDevice(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	p.Topics.Mark(id, "plug-new")
	if _, err := p.verifyAndPin(t.Context(), stale); !errors.Is(err, errAnotherDeviceOnTopic) {
		t.Fatalf("verifyAndPin = %v, want errAnotherDeviceOnTopic", err)
	}
	if p.Topics.Since(id, "plug-new").IsZero() {
		t.Error("the stored topic, where another device answered, is not marked")
	}
}

// Why (#852, third review): the mark sends a device's reads over HTTP, so the visits that ask the topic on their way (the
// broker read, the data refresh) must not be what ends it: they wait for gates the mark itself closes. A device reached
// over MQTT whose HTTP times out (a firewall that drops packets) is the usual case: timeouts are never recorded, so its
// HTTP state stays unknown and the HTTP health poll never reaches it. The data refresh found the other device and marked
// it; from then on Choose sends its reads over HTTP (unknown is not failing), the broker read waits for HTTP (readable),
// and the data refresh waits for its reads to go over MQTT (dataOverMQTT), so nothing asked the topic again and the
// device stayed held off it until TasmoFleet restarted. A marked device's topic must be asked on its own schedule, every
// brokerHostRetry, warning while it is still taken, until only the device answers there.
func TestAMarkedTopicIsAskedAgainOnItsOwnSchedule(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	router := p.Reads.(*transport.Router)
	facts := routerFacts(st, router, id, "Online")
	router.SetFacts(facts)
	ch := &twoOnTopic{dev: onTopic} // the device on the topic; DoAll hears every reply, as the real channel does
	router.SetMQTT(ch)
	var buf bytes.Buffer
	r := newBrokerHostReader(st.ListDevices,
		func(model.DeviceID) bool { return false }, // HTTP times out: the health poll never reaches the device
		p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
		slog.New(slog.NewTextHandler(&buf, nil)))
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	a := &runtime{svc: &Services{Prober: p}, facts: facts}
	a.wireDeviceRefresh(r) // as the app wires it (startBrokerHost)
	marked := func() bool { return !p.Topics.Since(id, "plug").IsZero() }

	r.pass(t.Context())
	if !marked() {
		t.Fatal("the data refresh did not mark the device: the test does not show the case it is about")
	}
	if f, err := facts.Facts(t.Context(), id); err != nil || f.HTTP.State != transport.HTTPUnknown {
		t.Fatalf("HTTP state %v (%v), want unknown (timeouts are never recorded): the test does not show the case it is about", f.HTTP.State, err)
	}

	now = now.Add(brokerHostRetry)
	buf.Reset()
	r.pass(t.Context())
	if !marked() {
		t.Fatal("the mark ended while the other device still answers on the topic")
	}
	if log := buf.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "another device answers on the device's MQTT topic") || !strings.Contains(log, "topic=plug") {
		t.Errorf("log %q, want a warning that names the topic while it is still taken", log)
	}

	onTopic.setMAC(storedMAC) // the other device left; only the device answers on its topic
	for range 2 {
		now = now.Add(brokerHostRetry)
		r.pass(t.Context())
		if !marked() {
			return
		}
	}
	t.Error("the device answers alone on its topic and is still held off it two passes later: nothing asked the topic again")
}

// cutShortTopic is the channel when the caller's context ends inside the reply window of a check that hears every
// reply: like mqttcmd.Channel.DoAll it returns the replies heard so far (the device's own, first) and no error.
type cutShortTopic struct{ brokerMQTT }

func (m cutShortTopic) DoAll(ctx context.Context, d model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	raw, err := m.Do(ctx, d, cmd)
	if err != nil {
		return nil, err
	}
	<-ctx.Done()
	return []json.RawMessage{raw}, nil
}

// oneReplyTopic is a channel that hears one reply only (no DoAll).
type oneReplyTopic struct{ dev *brokerAnswers }

func (m oneReplyTopic) Connected() bool { return true }
func (m oneReplyTopic) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	return m.dev.answer(cmd)
}
func (m oneReplyTopic) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}

// Why (#852, fourth review): only a check that heard its whole window may end a mark. A window cut short (the person's
// Refresh identity runs on the page request, which a reload ends; the reader's check has a budget) returned the
// device's own reply, which came first, as if it were the only one, and the mark ended although the second device
// would have answered later; a channel that hears one reply would end it on every check. Either way the device's
// writes, power toggles, backups and restores went to the shared topic again.
func TestOnlyACheckThatHeardItsWholeWindowEndsTheMark(t *testing.T) {
	for _, c := range []struct {
		name string
		ch   func(*brokerAnswers) transport.MQTTDoer
		ctx  func(context.Context) (context.Context, context.CancelFunc)
	}{
		{"the window was cut short", func(b *brokerAnswers) transport.MQTTDoer { return cutShortTopic{brokerMQTT{dev: b}} },
			func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(ctx, 20*time.Millisecond)
			}},
		{"the channel hears one reply", func(b *brokerAnswers) transport.MQTTDoer { return oneReplyTopic{dev: b} },
			func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			onTopic := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
			p, st, id := brokerReadOverRouter(t, onTopic, &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, "Online")
			p.Reads.(*transport.Router).SetMQTT(c.ch(onTopic))
			d, err := st.GetDevice(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			p.Topics.Mark(id, d.Topic)
			ctx, cancel := c.ctx(t.Context())
			defer cancel()
			if _, ok, err := p.recheckTopic(ctx, d); ok || err != nil {
				t.Errorf("recheckTopic = ok %v, %v; want nothing learnt", ok, err)
			}
			if p.Topics.Since(id, "plug").IsZero() {
				t.Error("the mark ended on a check that did not hear every reply in its window")
			}
		})
	}
}

// noCollectTopic is the device on its topic when the check that hears every reply learns nothing (here: nobody answers
// it in time), while a single read is still answered with the device's own MAC.
type noCollectTopic struct{ brokerMQTT }

func (noCollectTopic) DoAll(context.Context, model.Device, string, time.Duration) ([]json.RawMessage, error) {
	return nil, tasmota.ErrTimeout
}

// Why (#852, fourth review): for a marked device whose check that hears every reply learnt nothing, the reads fell
// through to a check that hears one reply. With HTTP failing that went to the topic, where whichever device answers
// first is heard: the device's own reply let the broker read and the data refresh read from a topic known to be
// taken, and Refresh identity wrote a Status 2 identity read there (before #682 it asked one reply there). Such a check vouches for nothing on that topic:
// nothing is read there until the check that hears every reply finds the device alone.
func TestAMarkedTopicTheFullCheckLearntNothingOnIsNotRead(t *testing.T) {
	setup := func(t *testing.T) (*Prober, model.DeviceID, *brokerAnswers) {
		t.Helper()
		onTopic := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}
		p, st, id := brokerReadOverRouter(t, onTopic, &brokerAnswers{mac: storedMAC, host: "http.example"}, "Online")
		router := p.Reads.(*transport.Router)
		router.SetMQTT(noCollectTopic{brokerMQTT{dev: onTopic}})
		router.Memory().Fail(id, transport.HTTPFailUnreachable)
		d, err := st.GetDevice(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		p.Topics.Mark(id, d.Topic)
		return p, id, onTopic
	}
	t.Run("the broker read", func(t *testing.T) {
		p, id, onTopic := setup(t)
		if err := p.ReadBroker(t.Context(), id); err == nil {
			t.Fatal("ReadBroker = nil: it read a topic held as taken after a check that heard one reply")
		}
		if got := onTopic.asked(); slices.Contains(got, "Status 6") {
			t.Errorf("asked %v on the taken topic", got)
		}
		if d, _ := p.Devices.GetDevice(t.Context(), id); d.MQTTHost != "old.example" {
			t.Errorf("stored broker %q, want the stored one kept", d.MQTTHost)
		}
		if p.Topics.Since(id, "plug").IsZero() {
			t.Error("the mark ended")
		}
	})
	t.Run("Refresh identity", func(t *testing.T) {
		p, id, onTopic := setup(t)
		p.Client = unreachable{}
		if err := p.Refresh(t.Context(), id, "admin"); err == nil {
			t.Fatal("Refresh = nil: it read a topic held as taken after a check that learnt nothing")
		}
		if got := onTopic.asked(); len(got) != 0 {
			t.Errorf("asked %v on a topic held as taken", got)
		}
		if d, _ := p.Devices.GetDevice(t.Context(), id); d.Identity.Version != "15.1.0" || d.Identity.Family != "esp8266" {
			t.Errorf("identity %+v written from the taken topic", d.Identity)
		}
		if p.Topics.Since(id, "plug").IsZero() {
			t.Error("the mark ended")
		}
	})
}

// Why (#851 review, R6): a device answers only on its own topic, so over MQTT the topic it reports is always the stored
// one; only a reply some other client forged on stat/<topic>/ can differ. The identity probe's reads over MQTT took
// the reported topic and the stored topic followed it, so a forged Status moved the device's later writes and
// transfers to another topic, a trust path the threat model says MQTT does not have.
// The facts over MQTT keep the stored topic.
func TestTheIdentityProbeOverMQTTKeepsTheStoredTopic(t *testing.T) {
	onTopic := &probeTopic{mac: "AA:BB:CC:00:00:01", status: `{"Status":{"DeviceName":"x","Topic":"evil"}}`}
	p, _, id, get := probeOverRouter(t, onTopic, "Online")
	au := &idAudit{}
	p.Audit = au
	if err := p.Refresh(t.Context(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(onTopic.asked(), "Status") {
		t.Fatalf("asked %v on the topic: the test does not show the case it is about", onTopic.asked())
	}
	if d := get(id); d.Topic != "plug" {
		t.Errorf("stored topic %q, want plug: a reply on the topic moved it", d.Topic)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.ev {
		if e.Action == "device.topic_changed" {
			t.Errorf("audited %+v", e)
		}
	}
}

// Why (#852, fourth review): the router asks the device's stored topic, which an edit may change after a refresh read
// its snapshot. The probe's check on the topic marked the topic of the refresh's snapshot, so the stored topic, where
// the other device was just heard, stayed unmarked and the device's writes went there.
func TestTheIdentityProbeMarksTheStoredTopic(t *testing.T) {
	p, _, id, get := probeOverRouter(t, &probeTopic{mac: otherMAC}, "Online")
	reads := p.ReadsOverMQTT
	p.ReadsOverMQTT = func(ctx context.Context, d model.Device) bool { // runs between the refresh's snapshot and its check
		cur := get(id)
		cur.Topic = "plug-new"
		if err := p.Devices.(interface {
			UpdateDevice(context.Context, model.Device) error
		}).UpdateDevice(t.Context(), cur); err != nil {
			t.Error(err)
		}
		return reads(ctx, d)
	}
	if err := p.RefreshFacts(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if p.Topics.Since(id, "plug-new").IsZero() {
		t.Error("the stored topic, where another device answered, is not marked")
	}
	if !p.Topics.Since(id, "plug").IsZero() {
		t.Error("the topic of the refresh's snapshot, which nobody asked, is marked")
	}
}
