package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// unreadableDevices is an inventory whose device snapshot cannot be read (the device was deleted, the store failed).
type unreadableDevices struct{}

func (unreadableDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, errors.New("store: no such device")
}

// onDevices rebuilds the rig's router on another inventory, with the rig's HTTP, channel, facts and memory.
func (g *rig) onDevices(devs DeviceGetter) {
	g.r = NewRouter(Options{HTTP: g.http, Devices: devs, Facts: g.f, Memory: g.mem})
	g.r.SetMQTT(g.mq)
	g.f.unanswered = g.r.unanswered
	g.f.connChanged = g.r.MQTTConnChanged()
	g.f.topicTaken = g.r.TopicTaken()
}

// Why (#605, #826, #1053): a write takes one way to a device, and every caller that names or takes that way asks the
// router's one decision, never a copy. The power toggle publishes through the command channel itself and asks
// WritesOverMQTT; the console's confirmation names TypedRoute; every other write is sent by Command. If one of the
// three is changed alone, a switch is published where any other write would go over HTTP first (or the reverse), or
// the confirmation names a route the send does not take. For every state the router tells apart, the three must give
// the same answer for a plain write, and that answer must be the transport rule: MQTT first where the device is usable
// there under the strict rule, else HTTP first.
func TestTheToggleAsksTheWriteRuleTheSendFollows(t *testing.T) {
	const cmd = "Power1 ON"
	for _, tc := range []struct {
		name  string
		setup func(g *rig)
		want  Path
	}{
		{"HTTP unknown, usable on the broker", func(*rig) {}, MQTT},
		{"HTTP answers", func(g *rig) { g.f.httpOK = true }, MQTT},
		{"HTTP failing by the cheap state", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable) }, MQTT},
		// The cheap state says "try HTTP" (a health poll newer than the failure) while the facts still say failing:
		// Command goes HTTP first here; before #1053 the toggle's own copy of the rule published.
		{"HTTP failing by the facts only", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable); g.f.healthNewer = true }, HTTP},
		{"HTTP failing, the GroupTopic unread", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable); g.f.groupKnown = false }, HTTP},
		{"MQTT just went unanswered", func(g *rig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }, HTTP},
		{"the MQTT connection was just changed", func(g *rig) { g.r.connChanged.Fail(7, HTTPFailUnreachable) }, HTTP},
		{"another device answered on the topic", func(g *rig) { g.r.TopicTaken().Mark(7, "plug") }, HTTP},
		{"the GroupTopic unread", func(g *rig) { g.f.groupKnown = false }, HTTP},
		{"not on our broker", func(g *rig) { g.devs.d.MQTT = model.MQTTOtherBroker }, HTTP},
		{"on our broker without a topic", func(g *rig) { g.devs.d.Topic = "" }, HTTP},
		{"the bus is down", func(g *rig) { g.mq.down = true }, HTTP},
		{"the snapshot cannot be read", func(g *rig) { g.onDevices(unreadableDevices{}) }, HTTP},
		{"the snapshot cannot be read, HTTP failing", func(g *rig) {
			g.onDevices(unreadableDevices{})
			g.mem.Fail(7, HTTPFailUnreachable)
		}, HTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			tc.setup(g)
			if got := g.r.WritesOverMQTT(t.Context(), g.devs.d); got != (tc.want == MQTT) {
				t.Fatalf("WritesOverMQTT = %v, want %v", got, tc.want == MQTT)
			}
			if got := g.r.TypedRoute(t.Context(), 7, cmd); got != tc.want {
				t.Fatalf("TypedRoute = %s, want %s", got, tc.want)
			}
			ctx, tr := WithTrace(t.Context())
			_, _ = g.r.Command(ctx, g.target(), cmd) //nolint:errcheck // only the route matters here
			if tr.Via() != map[Path]string{HTTP: "http", MQTT: "mqtt"}[tc.want] {
				t.Fatalf("the send went %s, the toggle was told %s", tr.Via(), tc.want)
			}
		})
	}
}

// Why (#1053): without what the decision needs (a device id, the command channel, the facts) the router sends a write
// over HTTP, so the toggle must never be told to publish.
func TestTheWriteQuestionWithoutItsInputsIsHTTP(t *testing.T) {
	g := newRig(t)
	if !g.r.WritesOverMQTT(t.Context(), g.devs.d) {
		t.Fatal("fixture: a usable device must be asked over MQTT")
	}
	if g.r.WritesOverMQTT(t.Context(), model.Device{Topic: "plug", MQTT: model.MQTTOurs}) {
		t.Error("a target without a device id must not be published to")
	}
	noFacts := NewRouter(Options{HTTP: g.http, Devices: g.devs, Memory: g.mem})
	noFacts.SetMQTT(g.mq)
	if noFacts.WritesOverMQTT(t.Context(), g.devs.d) {
		t.Error("without facts no judgement is possible: a switch must not be published")
	}
	if got := noFacts.TypedRoute(t.Context(), 7, "Power1 ON"); got != HTTP {
		t.Errorf("without facts TypedRoute = %s, want http", got)
	}
}

// Why (T6, #605, #1053 review): the power toggle publishes with the snapshot it read itself, while the router decides
// on its own. A switch must only be published to the topic the rule judged: an edit or an identity refresh between
// the two reads (a new topic, a group topic set, another broker) would otherwise publish Power to a topic no
// exclusivity check, topic-taken mark or capability check ever saw, which may be another device's now. Command's own
// publish re-reads the device right before it sends (publish, mqttSettingsChanged); the toggle's answer must hold to
// the same standard, including a change that lands while the decision is being made.
func TestTheToggleIsNeverToldToPublishASnapshotTheRuleDidNotJudge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(g *rig)
		caller func(d model.Device) model.Device
		racing bool // the setup changes the store on every decision: no second fixture check
	}{
		{"the caller's topic is older", nil, func(d model.Device) model.Device { d.Topic = "plug-old"; return d }, false},
		{"the caller's topic is older, HTTP failing", func(g *rig) { g.mem.Fail(7, HTTPFailUnreachable) },
			func(d model.Device) model.Device { d.Topic = "plug-old"; return d }, false},
		{"the caller knows a group topic the store no longer has", nil,
			func(d model.Device) model.Device { d.MoreGroupTopics = []string{"kitchen"}; return d }, false},
		{"the caller's FullTopic differs", nil, func(d model.Device) model.Device { d.FullTopic = "%topic%/%prefix%/"; return d }, false},
		{"the caller's snapshot is on another broker", nil,
			func(d model.Device) model.Device { d.MQTT = model.MQTTOtherBroker; return d }, false},
		{"the topic changes while the decision is made", func(g *rig) { g.r.o.Facts = changingFacts{inner: g.f, devs: g.devs} },
			func(d model.Device) model.Device { return d }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			if !g.r.WritesOverMQTT(t.Context(), g.devs.d) {
				t.Fatal("fixture: the store's own snapshot must be asked over MQTT")
			}
			caller := tc.caller(g.devs.d)
			if tc.setup != nil {
				tc.setup(g)
				if !tc.racing && !g.r.WritesOverMQTT(t.Context(), g.devs.d) {
					t.Fatal("fixture: after the setup the store's own snapshot must still be asked over MQTT")
				}
			}
			if g.r.WritesOverMQTT(t.Context(), caller) {
				t.Fatal("told to publish a snapshot whose MQTT settings the rule did not judge")
			}
		})
	}
}
