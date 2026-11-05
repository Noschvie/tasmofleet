package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// readsRig is a real transport.Router in front of a device on our broker that is online on MQTT.
// Every caller below must reach the device over MQTT and leave HTTP alone (#456 A).
type readsRig struct {
	router *transport.Router
	dev    model.Device
	mu     sync.Mutex
	http   []string
	mqtt   []string
	// mqttErr, if set, is what the channel answers (nothing is published unless the test says so).
	mqttErr error
}

func newReadsRig(t *testing.T) *readsRig {
	t.Helper()
	g := &readsRig{dev: model.Device{ID: 3, Name: "plug", Topic: "plug", Address: "192.0.2.3", MQTT: model.MQTTOurs}}
	g.router = transport.NewRouter(transport.Options{HTTP: (*readsHTTP)(g), Devices: g, Facts: (*readsFacts)(g)})
	g.router.SetMQTT((*readsMQTT)(g))
	return g
}

func (g *readsRig) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return g.dev, nil
}
func (g *readsRig) Get(context.Context, model.DeviceID) (model.Device, error) { return g.dev, nil }

type readsHTTP readsRig

func (h *readsHTTP) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	h.mu.Lock()
	h.http = append(h.http, c)
	h.mu.Unlock()
	return json.RawMessage(`{"HTTP":"yes"}`), nil
}
func (h *readsHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (h *readsHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type readsMQTT readsRig

func (m *readsMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mqtt = append(m.mqtt, c)
	if m.mqttErr != nil {
		return nil, m.mqttErr
	}
	switch c {
	case "Status 1":
		return json.RawMessage(`{"StatusPRM":{"GroupTopic":"tasmotas"}}`), nil
	case "Status 5":
		return json.RawMessage(status5Net), nil
	case "Status 11":
		return json.RawMessage(status11Net), nil
	case "Status 3":
		return json.RawMessage(`{"StatusLOG":{"TelePeriod":300}}`), nil
	}
	return json.RawMessage(`{"Topic":"plug"}`), nil
}
func (m *readsMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m *readsMQTT) Connected() bool { return true }

type readsFacts readsRig

func (f *readsFacts) HTTPState(model.DeviceID) transport.HTTPState { return transport.HTTPState{} }
func (f *readsFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	return transport.Facts{Now: time.Now(), MQTT: transport.MQTTFacts{
		BusConnected: true, Ours: d.MQTT == model.MQTTOurs, Topic: d.Topic, TopicExclusive: true, LWTOnline: true,
	}}, nil
}

func (g *readsRig) sent() (httpCalls, mqttCalls []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.http...), append([]string(nil), g.mqtt...)
}

func (g *readsRig) wantOnlyMQTT(t *testing.T, caller string, mqtt ...string) {
	t.Helper()
	h, m := g.sent()
	if len(h) != 0 {
		t.Errorf("%s: HTTP was used: %v", caller, h)
	}
	if len(m) != len(mqtt) {
		t.Fatalf("%s: MQTT got %v, want %v", caller, m, mqtt)
	}
	for i := range m {
		if m[i] != mqtt[i] {
			t.Errorf("%s: MQTT got %v, want %v", caller, m, mqtt)
		}
		if m[i] == "Status 0" {
			t.Errorf("%s: Status 0 was sent", caller)
		}
	}
}

func TestSettingsReadGoesOverMQTT(t *testing.T) {
	g := newReadsRig(t)
	m := settings.New(settings.Options{Devices: g, Client: g.router})
	got, err := m.ReadKeys(t.Context(), g.dev.ID, []string{"TelePeriod"})
	if err != nil || got["TelePeriod"] != "300" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	g.wantOnlyMQTT(t, "settings read", "Status 3")
}

func TestConsoleReadGoesOverMQTT(t *testing.T) {
	g := newReadsRig(t)
	svc := devicecmd.New(devicecmd.Options{Devices: g, Client: g.router, Audit: nopAudit{}})
	if _, err := svc.Run(t.Context(), model.Principal{Subject: "t", Role: model.RoleOperator}, g.dev.ID, "Topic"); err != nil {
		t.Fatal(err)
	}
	g.wantOnlyMQTT(t, "console read", "Topic")
}

func TestGroupTopicStatus1ReadGoesOverMQTT(t *testing.T) {
	g := newReadsRig(t)
	st := &gtStore{now: time.Now, devs: []model.Device{g.dev}}
	r := newGroupTopicReader(st.list, online, g.router, st, slog.New(slog.DiscardHandler))
	if !r.read(t.Context(), g.dev) {
		t.Fatal("Status 1 was not read")
	}
	g.wantOnlyMQTT(t, "Status 1", "Status 1")
}

func TestNetFactsReadGoesOverMQTT(t *testing.T) {
	g := newReadsRig(t)
	p, st, id, _, _ := raceSetup(t)
	p.Reads = g.router
	d, _ := st.GetDevice(t.Context(), id)
	d.MQTT, d.Topic = model.MQTTOurs, "plug"
	g.dev = d
	// the router reads the device from the rig; the prober stores into the real store
	if err := p.ReadNet(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	g.wantOnlyMQTT(t, "network facts", "Status 5", "Status 11")
}

// Why: a published read that gets no answer is an unknown outcome; the settings read reports it and
// HTTP is not asked for the same value behind its back.
func TestPublishedReadThatTimesOutIsNotRepeatedOverHTTP(t *testing.T) {
	g := newReadsRig(t)
	g.mqttErr = fmt.Errorf("mqtt: no answer to Status 3: %w", tasmota.ErrTimeout)
	m := settings.New(settings.Options{Devices: g, Client: g.router})
	_, err := m.ReadKeys(t.Context(), g.dev.ID, []string{"TelePeriod"})
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err=%v, want the timeout", err)
	}
	if h, _ := g.sent(); len(h) != 0 {
		t.Fatalf("HTTP was used after a published read: %v", h)
	}
}
