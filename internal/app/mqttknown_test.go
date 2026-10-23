package app

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// The bus reserves retained-cache space for inventory devices; the index must
// recognise them by topic and, for discovery messages, by MAC in the compact
// form Tasmota uses in tasmota/discovery/<MAC>/config.
func TestKnownIndex(t *testing.T) {
	k := newKnownIndex()
	k.set([]model.Device{{Topic: "lamp", MAC: "02:00:5e:d2:2c:14"}, {Topic: ""}})
	cases := []struct {
		m    mqttbus.Message
		want bool
	}{
		{mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp"}, true},
		{mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "junk"}, false},
		{mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: "02005ED22C14"}, true},
		{mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: "02005E433E52"}, false},
		{mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: "lamp"}, false},
		{mqttbus.Message{Kind: mqttbus.KindLWT, Topic: ""}, false},
	}
	for _, c := range cases {
		if got := k.Known(c.m); got != c.want {
			t.Errorf("%+v: got %v", c.m, got)
		}
	}
}

// Fail closed: an empty index (inventory unreadable) knows nothing.
func TestKnownIndexEmptyKnowsNothing(t *testing.T) {
	k := newKnownIndex()
	k.set([]model.Device{{Topic: "lamp"}})
	k.set(nil)
	if k.Known(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "lamp"}) {
		t.Fatal("stale reservation kept after a failed refresh")
	}
}

// Why: an ESP32 with Ethernet announces itself with either MAC. If only the
// primary were indexed, the device would lose its retained-cache reservation
// and its announcements would count as junk from an unknown device.
func TestKnownIndexCoversEveryMACOfTheSet(t *testing.T) {
	k := newKnownIndex()
	k.set([]model.Device{{MAC: "02:00:5e:d2:2c:14", MACs: []string{"02:00:5e:d2:2c:14", "02:00:5e:43:3e:52"}}})
	for _, topic := range []string{"02005ED22C14", "02005E433E52"} {
		if !k.Known(mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: topic}) {
			t.Errorf("%s not known", topic)
		}
	}
}
