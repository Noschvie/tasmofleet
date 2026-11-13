package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// writesDevice is on our broker, live there, with its own GroupTopic read: usable under the strict rule.
func writesDevice() model.Device {
	return model.Device{ID: 1, Name: "plug", Address: "192.0.2.1", MQTT: model.MQTTOurs, Topic: "plug", FullTopic: "%prefix%/%topic%/",
		GroupTopic: "tasmotas", GroupTopicReadAt: factsNow}
}

// Why (#605): the power toggle asks this before it publishes a switch. It must be the router's write rule, so a
// switch goes over MQTT exactly where any other change would: not to a device whose own GroupTopic is unread
// (its topic might be shared, the switch would reach other devices), not while another device's GroupTopic is
// unread, and not without facts.
func TestPowerAsksTheRoutersWriteRule(t *testing.T) {
	ok := writesDevice()
	unread := writesDevice()
	unread.GroupTopic, unread.GroupTopicReadAt = "", time.Time{}
	other := model.Device{ID: 2, Name: "other", MQTT: model.MQTTOurs, Topic: "other", FullTopic: "%prefix%/%topic%/"}
	live := fakeFactsHealth{1: {LWT: "Online"}, 2: {LWT: "Online"}}
	for _, tc := range []struct {
		name string
		devs []model.Device
		want bool
	}{
		{"strict usable", []model.Device{ok}, true},
		{"own GroupTopic unread", []model.Device{unread}, false},
		{"another device's GroupTopic unread", []model.Device{ok, other}, false},
	} {
		a := &runtime{facts: newFacts(tc.devs, live, fakeMemory{})}
		if got := a.writesOverMQTT(t.Context(), tc.devs[0]); got != tc.want {
			t.Errorf("%s: writesOverMQTT = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (&runtime{}).writesOverMQTT(t.Context(), ok) {
		t.Error("without facts a switch must not be published")
	}
}

type writesHTTP struct{ n int }

func (h *writesHTTP) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	h.n++
	return json.RawMessage(`{"SaveData":1}`), nil
}
func (h *writesHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) { return nil, nil }
func (h *writesHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type writesMQTT struct{ n int }

func (m *writesMQTT) Do(context.Context, model.Device, string) (json.RawMessage, error) {
	m.n++
	return json.RawMessage(`{"SaveData":1}`), nil
}
func (m *writesMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m *writesMQTT) Connected() bool { return true }

// Why (#605, interim until #606): Make compliant keeps HTTP first while HTTP works; it moves to MQTT first in its
// own change. Through the real router, a device every other write reaches over MQTT first gets the fix over HTTP.
func TestMakeCompliantKeepsHTTPFirstUntilItsOwnChange(t *testing.T) {
	d := writesDevice()
	facts := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online", SeenHTTP: factsNow}}, fakeMemory{})
	httpc, mq := &writesHTTP{}, &writesMQTT{}
	r := transport.NewRouter(transport.Options{HTTP: httpc, Devices: facts.Devices})
	r.SetFacts(facts)
	r.SetMQTT(mq)
	if _, err := r.Command(t.Context(), tasmota.TargetFor(d), "SaveData 1"); err != nil || mq.n != 1 || httpc.n != 0 {
		t.Fatalf("fixture: an ordinary write must go over MQTT first (err=%v mqtt=%d http=%d)", err, mq.n, httpc.n)
	}
	rt := &runtime{}
	rt.fixer = &policy.Fixer{Client: r, Wait: func(context.Context, time.Duration) error { return nil }}
	if err := (policyApplier{a: rt}).Apply(t.Context(), d, []string{"SaveData 1"}); err != nil { // a SaveData line saves by itself: no read first
		t.Fatal(err)
	}
	if mq.n != 1 || httpc.n == 0 {
		t.Fatalf("Make compliant: mqtt=%d http=%d, want every line over HTTP", mq.n, httpc.n)
	}
}

// Why (#605 review, round 4): after TasmoFleet changed a device's MQTT connection over MQTT while HTTP answered, the
// device restarts and may not come back to the broker, its last will still Online. HTTP carries the next commands:
// the facts must carry the router's mark to Choose, so the device page says why and the power toggle, which asks
// the write rule itself, switches over HTTP too instead of publishing into the void.
func TestAfterAChangeOfTheMQTTConnectionTheFactsSendSwitchesOverHTTP(t *testing.T) {
	d := writesDevice()
	facts := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, fakeMemory{})
	httpc, mq := &writesHTTP{}, &writesMQTT{}
	r := transport.NewRouter(transport.Options{HTTP: httpc, Devices: facts.Devices})
	facts.ConnChanged = r.MQTTConnChanged()
	r.SetFacts(facts)
	r.SetMQTT(mq)
	a := &runtime{facts: facts}
	if !a.writesOverMQTT(t.Context(), d) {
		t.Fatal("fixture: before the change a switch must go over MQTT")
	}
	if _, err := r.Command(t.Context(), tasmota.TargetFor(d), "MqttPassword typo"); err != nil || mq.n != 1 || httpc.n != 1 {
		t.Fatalf("MqttPassword typo: err=%v mqtt=%d http=%d, want the probe and one publish", err, mq.n, httpc.n)
	}
	f, err := facts.Facts(t.Context(), d.ID)
	if err != nil || f.MQTT.ConnChangedSince.IsZero() {
		t.Fatalf("the facts do not see the mark: %+v %v", f.MQTT, err)
	}
	if p := transport.Choose(f); p.Writes != transport.HTTP || p.Commands != transport.HTTP {
		t.Fatalf("writes=%s reads=%s (%s), want HTTP while the mark is fresh", p.Writes, p.Commands, p.Reason(transport.ActWrites))
	}
	if a.writesOverMQTT(t.Context(), d) {
		t.Error("the power toggle would still publish a switch right after the change of the MQTT connection")
	}
}
