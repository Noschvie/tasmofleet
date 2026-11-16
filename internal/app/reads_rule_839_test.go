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

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/health"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #839: every read follows the product owner's transport rule (2026-11-14): MQTT first for a device usable on
// TasmoFleet's broker, HTTP only where MQTT cannot do it or the device is not usable there
// (docs/security/mqtt-transport.md, "What still uses HTTP"). These tests drive the read paths that went HTTP first
// before #839 through the real router, as the app wires them.

// brokerAnswers answers the broker read (Status 5, Status 6, SetOption3) as a device configured for host:1883 with
// MQTT switched on, reporting mac, and counts what it was asked.
type brokerAnswers struct {
	mu   sync.Mutex
	mac  string
	host string
	got  []string
}

func (b *brokerAnswers) answer(cmd string) (json.RawMessage, error) {
	b.mu.Lock()
	b.got = append(b.got, cmd)
	b.mu.Unlock()
	switch cmd {
	case "Status 5":
		return mac5(b.mac), nil
	case "Status 6":
		return json.RawMessage(`{"StatusMQT":{"MqttHost":"` + b.host + `","MqttPort":1883}}`), nil
	case "SetOption3":
		return json.RawMessage(`{"SetOption3":"ON"}`), nil
	}
	return nil, tasmota.ErrUnknownCommand
}

func (b *brokerAnswers) asked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.got...)
}

// brokerMQTT is the command channel answering as the device on the topic.
type brokerMQTT struct{ dev *brokerAnswers }

func (m brokerMQTT) Connected() bool { return true }
func (m brokerMQTT) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	return m.dev.answer(cmd)
}
func (m brokerMQTT) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}

// DoAll hears every reply within the window, as the production channel does (transport.MQTTMultiReader): one device
// listens on the topic, so one reply.
func (m brokerMQTT) DoAll(ctx context.Context, d model.Device, cmd string, _ time.Duration) ([]json.RawMessage, error) {
	raw, err := m.Do(ctx, d, cmd)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}

// brokerHTTP is the device at the stored address.
type brokerHTTP struct {
	tasmota.Client
	dev *brokerAnswers
}

func (h brokerHTTP) Command(_ context.Context, _ tasmota.Target, cmd string) (json.RawMessage, error) {
	return h.dev.answer(cmd)
}

// brokerReadOverRouter stores a device on our broker (MAC storedMAC, broker "old.example") and returns a prober wired
// as production wires it (Client: the router's raw HTTP client, Reads: the router), with onTopic answering over MQTT
// and atAddr over HTTP. lwt is the device's last will.
func brokerReadOverRouter(t *testing.T, onTopic, atAddr *brokerAnswers, lwt string) (*Prober, *store.SQLite, model.DeviceID) {
	t.Helper()
	st := openBrokerStore(t)
	id, err := st.CreateDevice(t.Context(), model.Device{Name: "plug", Topic: "plug", Address: "192.0.2.1", MQTT: model.MQTTOurs,
		MQTTHost: "old.example", MQTTPort: 1883, MAC: storedMAC, MACs: []string{storedMAC},
		Identity: model.Identity{Version: "15.1.0", Variant: "tasmota", Family: "esp8266"}})
	if err != nil {
		t.Fatal(err)
	}
	router := transport.NewRouter(transport.Options{HTTP: brokerHTTP{dev: atAddr}, Devices: st})
	facts := routerFacts(st, router, id, lwt)
	router.SetFacts(facts)
	router.SetMQTT(brokerMQTT{dev: onTopic})
	rt := &runtime{facts: facts}
	return &Prober{Client: router.HTTPOnly(), Reads: router, Devices: st, Gap: time.Millisecond, Broker: fixedBroker{host: "mqtt.home"},
		Topics: router.TopicTaken(), ReadsOverMQTT: rt.readsOverMQTT}, st, id
}

// routerFacts are the facts of brokerReadOverRouter's router as the app wires them (its memories, its topic marks):
// a runtime given them decides as the router does (runtime.readsOverMQTT).
func routerFacts(st *store.SQLite, router *transport.Router, id model.DeviceID, lwt string) *TransportFacts {
	return &TransportFacts{Devices: st, Health: fakeFactsHealth{id: {LWT: lwt}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		TopicTaken:   router.TopicTaken(),
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now}
}

// Why (#839): the background broker read asked Status 5, Status 6 and SetOption3 over HTTP although the device was
// live on TasmoFleet's broker, against the transport rule (the live test of 2026-11-13 saw its Status 5 over HTTP).
// Over MQTT first, the device on the topic answers all three and nothing goes to the address; when MQTT is not usable
// (the last will says Offline) HTTP carries the read as before.
func TestTheBrokerReadGoesOverMQTTFirst(t *testing.T) {
	t.Run("MQTT usable", func(t *testing.T) {
		onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "http.example"}
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
		if err := p.ReadBroker(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if got := atAddr.asked(); len(got) != 0 {
			t.Errorf("asked over HTTP: %v, want nothing while MQTT is usable", got)
		}
		if got := onTopic.asked(); len(got) != 3 || got[0] != "Status 5" || got[1] != "Status 6" || got[2] != "SetOption3" {
			t.Errorf("asked over MQTT: %v, want the check, Status 6 and SetOption3", got)
		}
		if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "mqtt.home" {
			t.Errorf("stored broker %q, want the one the device reported over MQTT", d.MQTTHost)
		}
	})
	t.Run("MQTT not usable", func(t *testing.T) {
		onTopic, atAddr := &brokerAnswers{mac: storedMAC, host: "mqtt.home"}, &brokerAnswers{mac: storedMAC, host: "http.example"}
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Offline")
		if err := p.ReadBroker(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if got := onTopic.asked(); len(got) != 0 {
			t.Errorf("asked over MQTT: %v, want nothing while the device is offline there", got)
		}
		if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "http.example" {
			t.Errorf("stored broker %q, want the one the device reported over HTTP", d.MQTTHost)
		}
	})
}

// Why (#839, #810): over MQTT the answer comes from whoever listens on the topic. The broker read's check goes the way
// its reads go: a device on the topic that reports another MAC gets nothing but the check, and its broker is not
// stored on this device; the device at the address (the stored one) is not asked instead.
func TestTheBrokerReadOverMQTTRefusesAnotherDeviceOnTheTopic(t *testing.T) {
	onTopic, atAddr := &brokerAnswers{mac: otherMAC, host: "elsewhere.example"}, &brokerAnswers{mac: storedMAC, host: "http.example"}
	p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
	if err := p.ReadBroker(t.Context(), id); !errors.Is(err, errAnotherDevice) {
		t.Fatalf("ReadBroker = %v, want errAnotherDevice", err)
	}
	if got := onTopic.asked(); len(got) != 1 || got[0] != "Status 5" {
		t.Errorf("asked over MQTT: %v, want only the check", got)
	}
	if got := atAddr.asked(); len(got) != 0 {
		t.Errorf("asked over HTTP: %v", got)
	}
	if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "old.example" {
		t.Errorf("stored broker %q: another device's broker was stored", d.MQTTHost)
	}
}

// restartBus delivers the tracker's subscription one message.
type restartBus struct {
	mu sync.Mutex
	h  func(mqttbus.Message)
}

func (b *restartBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.mu.Lock()
	b.h = h
	b.mu.Unlock()
	return func() {}
}
func (b *restartBus) Command(context.Context, string, string, string) error { return nil }
func (b *restartBus) CommandTopic(topic, command string) string {
	return "cmnd/" + topic + "/" + command
}
func (b *restartBus) Connected() bool { return true }
func (b *restartBus) send(m mqttbus.Message) bool {
	b.mu.Lock()
	h := b.h
	b.mu.Unlock()
	if h != nil {
		h(m)
	}
	return h != nil
}

// restartReads counts the Status 1 a client was asked and answers it with a restart reason.
type restartReads struct {
	tasmota.Client
	mu      sync.Mutex
	status1 int
	other   []string
}

func (c *restartReads) Command(_ context.Context, _ tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cmd == "Status 1" {
		c.status1++
		return json.RawMessage(`{"StatusPRM":{"RestartReason":"Software/System restart","BootCount":7}}`), nil
	}
	c.other = append(c.other, cmd)
	return nil, tasmota.ErrTimeout
}

func (c *restartReads) count() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status1, append([]string(nil), c.other...)
}

// Why (#839): the health tracker read the reason of a restart (Status 1) with its HTTP-only client, which is right for
// its polls (they measure HTTP) but not for this read: a device live on TasmoFleet's broker had its restart read over
// HTTP, and one whose HTTP is blocked never had it read at all. The tracker the app builds (healthDeps) must ask the
// router, which reads MQTT first; the raw HTTP client is asked nothing for it.
func TestTheTrackersRestartCheckAsksTheRouter(t *testing.T) {
	st := openBrokerStore(t)
	if _, err := st.CreateDevice(t.Context(), model.Device{Name: "plug", Topic: "plug", Address: "192.0.2.1", MQTT: model.MQTTOurs}); err != nil {
		t.Fatal(err)
	}
	httpOnly, router := &restartReads{}, &restartReads{}
	bus := &restartBus{}
	a := &runtime{svc: &Services{Store: st, HTTP: httpOnly, Client: router, Hub: events.NewHub(8)}, bus: bus}
	deps := a.healthDeps() // the wiring under test: which client carries which call
	deps.OnMQTTSeen = nil  // no inventory in this rig
	deps.Expected = nil    // nothing works on the device
	tr := health.New(deps, health.Options{})
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = tr.Run(ctx) }() // ends with ctx
	defer func() { stop(); <-done }()

	for deadline := time.Now().Add(5 * time.Second); !bus.send(mqttbus.Message{}); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the tracker never subscribed")
		}
	}
	// The first uptime the tracker sees makes it read the restart that started this run (Status 1).
	bus.send(mqttbus.Message{Kind: mqttbus.KindState, Topic: "plug", Suffix: "STATE", Payload: []byte(`{"UptimeSec":7200}`), At: time.Now()})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		a.svc.Hub.Publish(events.Event{Topic: events.TopicDevice, At: time.Now()}) // a tick now, not in five seconds
		if n, _ := router.count(); n > 0 {
			break
		}
		if n, _ := httpOnly.count(); n > 0 || time.Now().After(deadline) {
			break
		}
	}
	if n, _ := httpOnly.count(); n != 0 {
		t.Errorf("%d x Status 1 over the HTTP-only client: the restart check bypassed the router", n)
	}
	if n, _ := router.count(); n != 1 {
		t.Errorf("%d x Status 1 through the router, want 1", n)
	}
}

// brokerSilentMQTT publishes on the topic and hears no answer: the device left TasmoFleet's broker after a clean
// disconnect, so its retained "Online" will stays. It records what was published.
type brokerSilentMQTT struct{ dev *brokerAnswers }

func (m brokerSilentMQTT) Connected() bool { return true }
func (m brokerSilentMQTT) Do(_ context.Context, _ model.Device, cmd string) (json.RawMessage, error) {
	m.dev.mu.Lock()
	m.dev.got = append(m.dev.got, cmd)
	m.dev.mu.Unlock()
	return nil, tasmota.ErrTimeout
}
func (m brokerSilentMQTT) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}

// Why (#598, #839): the broker read exists to notice a device that moved to another broker outside TasmoFleet. After
// a clean disconnect such a device keeps a retained "Online" will on ours, so MQTT first asks the topic, where nobody
// answers. The router must then ask the address once, and the reads must follow the check there: otherwise the move
// is never noticed. With another device at the address nothing is stored.
func TestTheBrokerReadOfADeviceThatLeftTheBrokerGoesOnOverHTTP(t *testing.T) {
	t.Run("the stored device at the address", func(t *testing.T) {
		onTopic, atAddr := &brokerAnswers{mac: storedMAC}, &brokerAnswers{mac: storedMAC, host: "other.example"}
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
		p.Reads.(*transport.Router).SetMQTT(brokerSilentMQTT{dev: onTopic})
		if err := p.ReadBroker(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if got := onTopic.asked(); !slices.Equal(got, []string{"Status 5"}) {
			t.Errorf("asked over MQTT: %v, want only the check, once", got)
		}
		if got := atAddr.asked(); !slices.Equal(got, []string{"Status 5", "Status 6", "SetOption3"}) {
			t.Errorf("asked over HTTP: %v, want the check and the reads it vouches for", got)
		}
		if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "other.example" {
			t.Errorf("stored broker %q, want the new one the device reported at the address", d.MQTTHost)
		}
	})
	t.Run("another device at the address", func(t *testing.T) {
		onTopic, atAddr := &brokerAnswers{mac: storedMAC}, &brokerAnswers{mac: otherMAC, host: "other.example"}
		p, st, id := brokerReadOverRouter(t, onTopic, atAddr, "Online")
		p.Reads.(*transport.Router).SetMQTT(brokerSilentMQTT{dev: onTopic})
		if err := p.ReadBroker(t.Context(), id); !errors.Is(err, errAnotherDevice) {
			t.Fatalf("ReadBroker = %v, want errAnotherDevice", err)
		}
		if got := atAddr.asked(); !slices.Equal(got, []string{"Status 5"}) {
			t.Errorf("asked over HTTP: %v, want only the check", got)
		}
		if d, _ := st.GetDevice(t.Context(), id); d.MQTTHost != "old.example" {
			t.Errorf("stored broker %q: another device's broker was stored", d.MQTTHost)
		}
	})
}

// Why (#839, #852): the broker read's check goes MQTT first, so a foreign MAC in its answer now usually means another
// device listens on this device's topic, where its commands over MQTT go: that must be visible in the log (Warn, naming
// the topic, so the person knows which topic to fix). Another device at the address is mostly a DHCP lease handed on and
// stays a Debug line.
func TestTheBrokerReaderWarnsOfAnotherDeviceOnTheTopic(t *testing.T) {
	visit := func(t *testing.T, onTopic, atAddr *brokerAnswers, lwt string) string {
		t.Helper()
		p, _, id := brokerReadOverRouter(t, onTopic, atAddr, lwt)
		var buf bytes.Buffer
		r := newBrokerHostReader(nil, nil, p.ReadBroker, func(context.Context, model.DeviceID, string) (func(), error) { return func() {}, nil },
			slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		refused, err := r.readLocked(t.Context(), id, visitWhat{broker: true})
		if err != nil || !refused {
			t.Fatalf("readLocked = %v, %v; want refused", refused, err)
		}
		return buf.String()
	}
	t.Run("on the topic", func(t *testing.T) {
		log := visit(t, &brokerAnswers{mac: otherMAC, host: "elsewhere.example"}, &brokerAnswers{mac: storedMAC}, "Online")
		if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "another device answers on the device's MQTT topic") || !strings.Contains(log, "topic=plug") {
			t.Errorf("log %q, want a warning that names the topic", log)
		}
	})
	t.Run("at the address", func(t *testing.T) {
		log := visit(t, &brokerAnswers{mac: storedMAC}, &brokerAnswers{mac: otherMAC}, "Offline")
		if strings.Contains(log, "level=WARN") || !strings.Contains(log, "another device answers at the address") {
			t.Errorf("log %q, want a debug line that names the address", log)
		}
	})
}
