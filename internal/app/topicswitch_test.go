package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

type anyBroker struct{}

// subscribeSpy closes subscribed on the first Subscribe: the point from which the bus
// hands every message, live or retained, to the health tracker.
type subscribeSpy struct {
	mqttbus.Bus
	subscribed chan struct{}
	once       sync.Once
}

func (s *subscribeSpy) Subscribe(kinds []mqttbus.Kind, h func(mqttbus.Message)) func() {
	unsub := s.Bus.Subscribe(kinds, h)
	s.once.Do(func() { close(s.subscribed) })
	return unsub
}

func (anyBroker) Same(string, int) bool { return true }

// Why (#119, live QA of #111): after "Topic <new>" on the device and "Refresh identity", the
// stored topic follows the device, but the Network card kept reading "MQTT: Not connected" for a
// device that is Online on the broker, and so did the switch back. The device was already heard
// on its old topic, whose last LWT is the Offline Tasmota publishes there on a topic change; the
// retained Online of the new topic was not applied. The full path runs here: simulated device
// over HTTP, embedded broker, real bus, store, prober and health tracker. No message arrives
// after the refresh: the retained LWT of the new topic alone has to make the device connected.
func TestTopicChangeByRefreshAppliesTheNewTopicsRetainedLWT(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	p, d := probeFixture(t, simulator.DeviceSpec{Name: "tablet-charger", Profile: simulator.ProfileESP8266_1M})
	d, err = p.Describe(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	const oldTopic, newTopic = "tablet-charger", "tablet-charger-qa"
	if d.Topic != oldTopic {
		t.Fatalf("simulated device reports topic %q, want %q", d.Topic, oldTopic)
	}
	d.MQTT = model.MQTTOurs
	st := newTestStore(t)
	id, err := st.CreateDevice(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	d.ID = id
	hub := events.NewHub(64)
	p.Devices, p.Hub, p.Broker = st, hub, anyBroker{}

	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	spy := &subscribeSpy{Bus: bus, subscribed: make(chan struct{})}
	tr := health.New(health.Deps{Store: st, Client: p.Client, Bus: spy, Hub: hub}, health.Options{})
	go func() { _ = tr.Run(ctx) }() // ends with ctx
	// Why (#140): the broker hands a live (not retained) message to the bus the moment the bus
	// has subscribed, even if the tracker is still loading the inventory, and the bus does not
	// keep it. Publishing before the tracker has subscribed could lose the first Online
	// (seen under -race, where the start is slow), so wait for that point.
	select {
	case <-spy.subscribed:
	case <-time.After(30 * time.Second):
		t.Fatal("the health tracker never subscribed to the bus")
	}

	lwt := func(topic, payload string) {
		t.Helper()
		if err := br.Publish("tele/"+topic+"/LWT", []byte(payload), true); err != nil {
			t.Fatal(err)
		}
	}
	waitLWT := func(want, why string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if tr.Get(id).LWT == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		h := tr.Get(id)
		t.Fatalf("%s: LWT %q (state %s), want %q", why, h.LWT, h.State, want)
	}
	// switchTopic is what Tasmota does on "Topic <to>": Offline (retained) on the old LWT topic,
	// then a restart that connects with the new topic and publishes Online (retained) there.
	target := tasmota.TargetFor(d)
	switchTopic := func(from, to string) {
		t.Helper()
		if _, err := p.Client.Command(ctx, target, "Topic "+to); err != nil {
			t.Fatal(err)
		}
		lwt(from, "Offline")
		deadline := time.Now().Add(10 * time.Second)
		for {
			raw, err := p.Client.Command(ctx, target, "Status")
			var s struct{ Status struct{ Topic string } }
			if err == nil && json.Unmarshal(raw, &s) == nil && s.Status.Topic == to {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("device does not report topic %q after the restart (err %v)", to, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		lwt(to, "Online")
	}
	refresh := func(want string) {
		t.Helper()
		if err := p.Refresh(ctx, id, "admin"); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.GetDevice(ctx, id); got.Topic != want {
			t.Fatalf("stored topic %q after the refresh, want %q", got.Topic, want)
		}
	}

	lwt(oldTopic, "Online")
	waitLWT("Online", "device on its first topic")

	switchTopic(oldTopic, newTopic)
	waitLWT("Offline", "the old topic's Offline is still matched before the refresh")
	refresh(newTopic)
	waitLWT("Online", "after the refresh to the new topic")
	if h := tr.Get(id); h.State != model.HealthOnline {
		t.Fatalf("state %s after the refresh, want online", h.State)
	}

	// And back: the first topic's retained LWT is Online again, the -qa one Offline.
	switchTopic(newTopic, oldTopic)
	waitLWT("Offline", "the -qa topic's Offline before the second refresh")
	refresh(oldTopic)
	waitLWT("Online", "after the refresh back to the first topic")
}
