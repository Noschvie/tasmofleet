package app

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#1047): the Topic check card and the Identity card's hint must say WHY MQTT writes to a device are held back,
// naming the topic, the other device and where it keeps the topic. Each row is one branch of the write gate's topic
// rule; a reason the view leaves out, or words differently from the gate, would send the person looking in the wrong
// place while the gate keeps the writes off MQTT.
func TestTopicConflictsNamesEveryReasonTheWriteGateRefuses(t *testing.T) {
	type c = transport.TopicConflict
	cases := []struct {
		name   string
		devs   []model.Device // device 1 is the one asked about
		prefix string
		want   []c
		ok     bool
	}{
		{"alone", []model.Device{{ID: 1, Topic: "a"}}, "cmnd", nil, true},
		{"a shared tasmotas is intended, no conflict",
			[]model.Device{{ID: 1, Topic: "a", GroupTopic: "tasmotas"}, {ID: 2, Name: "b", Topic: "b", GroupTopic: "tasmotas"}}, "cmnd", nil, true},
		{"the same Topic", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Name: "twin", Topic: "a"}}, "cmnd",
			[]c{{Kind: transport.ConflictSameTopic, Topic: "a", OtherID: 2, OtherName: "twin", Slot: transport.SlotTopic}}, true},
		{"another device's FullTopic without %topic%", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Name: "fixed", Topic: "x", FullTopic: "%prefix%/kitchen/"}}, "cmnd",
			[]c{{Kind: transport.ConflictOtherStemUnknown, Topic: "x", OtherID: 2, OtherName: "fixed", Slot: transport.SlotTopic}}, true},
		{"another device's FullTopic with %hostname%, its group topics unknown too",
			[]model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Name: "host", Topic: "b", FullTopic: "%prefix%/%hostname%/", GroupTopic: "g", MoreGroupTopics: []string{"h"}}}, "cmnd",
			[]c{{Kind: transport.ConflictOtherStemUnknown, Topic: "b", OtherID: 2, OtherName: "host", Slot: transport.SlotTopic}}, true},
		{"command topics overlap through a FullTopic", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Name: "lamp", Topic: "lamp", FullTopic: "%prefix%/kitchen/%topic%/"}}, "cmnd",
			[]c{{Kind: transport.ConflictStemOverlap, Topic: "lamp", Stem: "cmnd/kitchen", OtherID: 2, OtherName: "lamp", Slot: transport.SlotTopic}}, true},
		{"a FullTopic without a Topic listens on every command", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Name: "bare", FullTopic: "%prefix%/%topic%/"}}, "cmnd",
			[]c{{Kind: transport.ConflictStemOverlap, Topic: "", Stem: "cmnd", OtherID: 2, OtherName: "bare", Slot: transport.SlotTopic}}, true},
		{"another device's GroupTopic1 is the Topic", []model.Device{{ID: 1, Topic: "kueche"}, {ID: 2, Name: "flur", Topic: "flur", GroupTopic: "kueche"}}, "cmnd",
			[]c{{Kind: transport.ConflictGroupIsTopic, Topic: "kueche", OtherID: 2, OtherName: "flur", Slot: transport.SlotGroupTopic1}}, true},
		{"another device's GroupTopic3 is the Topic: one of 2 to 4", []model.Device{{ID: 1, Topic: "kueche"},
			{ID: 2, Name: "flur", Topic: "flur", GroupTopic: "tasmotas", MoreGroupTopics: []string{"", "kueche", ""}}}, "cmnd",
			[]c{{Kind: transport.ConflictGroupIsTopic, Topic: "kueche", OtherID: 2, OtherName: "flur", Slot: transport.SlotGroupTopic2to4}}, true},
		{"without a GroupTopic1 the first listed group is still one of 2 to 4", []model.Device{{ID: 1, Topic: "kueche"},
			{ID: 2, Name: "flur", Topic: "flur", MoreGroupTopics: []string{"kueche"}}}, "cmnd",
			[]c{{Kind: transport.ConflictGroupIsTopic, Topic: "kueche", OtherID: 2, OtherName: "flur", Slot: transport.SlotGroupTopic2to4}}, true},
		{"a group topic's command topic overlaps", []model.Device{{ID: 1, Topic: "kitchen", FullTopic: "%prefix%/room/%topic%/"}, {ID: 2, Name: "room", Topic: "r", GroupTopic: "room"}}, "cmnd",
			[]c{{Kind: transport.ConflictGroupStemOverlap, Topic: "room", Stem: "cmnd/room", OtherID: 2, OtherName: "room", Slot: transport.SlotGroupTopic1}}, true},
		{"SetOption75 with our custom prefix", []model.Device{{ID: 1, Topic: "kitchen", FullTopic: "%prefix%/%topic%/"}, {ID: 2, Name: "g", Topic: "b", FullTopic: "x/%prefix%/%topic%", MoreGroupTopics: []string{"kitchen/sub"}}}, "cmd",
			[]c{{Kind: transport.ConflictSetOption75, Topic: "kitchen/sub", Stem: "cmd/kitchen", OtherID: 2, OtherName: "g", Slot: transport.SlotGroupTopic2to4}}, true},
		{"SetOption75 with Tasmota's default cmnd", []model.Device{{ID: 1, Topic: "kit", FullTopic: "cmnd/%topic%chen/%prefix%"}, {ID: 2, Name: "g", Topic: "b", FullTopic: "x/%prefix%/%topic%", GroupTopic: "kitchen"}}, "cmd",
			[]c{{Kind: transport.ConflictSetOption75, Topic: "kitchen", Stem: "cmnd/kitchen", OtherID: 2, OtherName: "g", Slot: transport.SlotGroupTopic1}}, true},
		{"the device's own GroupTopic1 is its Topic", []model.Device{{ID: 1, Name: "self", Topic: "a", GroupTopic: "a"}}, "cmnd",
			[]c{{Kind: transport.ConflictOwnGroupIsTopic, Topic: "a", OtherID: 1, OtherName: "self", Slot: transport.SlotGroupTopic1}}, true},
		{"the device's own GroupTopic2 is its Topic", []model.Device{{ID: 1, Name: "self", Topic: "a", GroupTopic: "tasmotas", MoreGroupTopics: []string{"a"}}}, "cmnd",
			[]c{{Kind: transport.ConflictOwnGroupIsTopic, Topic: "a", OtherID: 1, OtherName: "self", Slot: transport.SlotGroupTopic2to4}}, true},
		{"every reason of one pair is listed, the same Topic once",
			[]model.Device{{ID: 1, Topic: "a"}, {ID: 2, Name: "twin", Topic: "a", GroupTopic: "a"}}, "cmnd",
			[]c{{Kind: transport.ConflictSameTopic, Topic: "a", OtherID: 2, OtherName: "twin", Slot: transport.SlotTopic},
				{Kind: transport.ConflictGroupIsTopic, Topic: "a", OtherID: 2, OtherName: "twin", Slot: transport.SlotGroupTopic1}}, true},
		{"no Topic: nothing is known", []model.Device{{ID: 1, FullTopic: "%prefix%/%topic%/"}, {ID: 2, Topic: "b"}}, "cmnd", nil, false},
		{"own FullTopic cannot be expanded: nothing is known", []model.Device{{ID: 1, Topic: "a", FullTopic: "%prefix%/%hostname%/%topic%/"}, {ID: 2, Topic: "a"}}, "cmnd", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := topicConflicts(tc.devs[0], tc.devs, tc.prefix)
			if ok != tc.ok || !slices.Equal(got, tc.want) {
				t.Errorf("conflicts = %+v, ok %v; want %+v, ok %v", got, ok, tc.want, tc.ok)
			}
			if ex := topicExclusive(tc.devs[0], tc.devs, tc.prefix); ex != (tc.ok && len(tc.want) == 0) {
				t.Errorf("topicExclusive = %v, but the reasons say %v", ex, tc.ok && len(tc.want) == 0)
			}
		})
	}
}

// Why (#1047): topicExclusive is the write gate and topicConflicts is what the card and the hint show; they must stay
// one rule. This test guards the wrapper: topicExclusive must not turn back into a loop of its own that answers apart
// from the conflicts, and no conflict may be reported for a device whose own topic is not known (!ok), where the gate
// refuses without a reason to name. It cannot catch a change of the rule itself, since both sides follow it: each
// branch of the gate is pinned by TestTopicConflictsNamesEveryReasonTheWriteGateRefuses and TestFactsTopicExclusive.
// The generated fleets aim at the hard shapes on purpose: FullTopics without %topic%, with %hostname%, a FullTopic
// without a Topic, stems that nest (a and a/b), group topics on every slot, the same group topic on several slots,
// and both command prefixes for SetOption75's stem.
func TestTopicExclusiveIsNoConflictsOverGeneratedFleets(t *testing.T) {
	rng := rand.New(rand.NewPCG(1047, 2026))
	topics := []string{"", "a", "a/b", "b", "kitchen", "kitchen/lamp", "chen", "k", "room", "tasmotas"}
	fulls := []string{"", "", "%prefix%/%topic%/", "%prefix%/%topic%", "x/%topic%/", "%prefix%/room/%topic%/",
		"%prefix%//kit%topic%/", "%prefix%/kitchen/", "%prefix%/%hostname%/%topic%/", "%prefix%/", "cmd/a/%topic%", "%topic%/%prefix%"}
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	exclusive, held := 0, 0
	for range 4000 {
		n := 1 + rng.IntN(4)
		devs := make([]model.Device, n)
		for i := range devs {
			devs[i] = model.Device{ID: model.DeviceID(i + 1), Name: "d" + pick(topics), Topic: pick(topics), FullTopic: pick(fulls), GroupTopic: pick(topics)}
			for range rng.IntN(4) {
				devs[i].MoreGroupTopics = append(devs[i].MoreGroupTopics, pick(topics))
			}
		}
		prefix := pick([]string{"cmnd", "cmd"})
		for _, d := range devs {
			conflicts, ok := topicConflicts(d, devs, prefix)
			if !ok && len(conflicts) > 0 {
				t.Fatalf("%+v: conflicts %+v listed for a topic that is not known", d, conflicts)
			}
			ex := topicExclusive(d, devs, prefix)
			if ex != (ok && len(conflicts) == 0) {
				t.Fatalf("prefix %s, fleet %+v: topicExclusive(%d) = %v, conflicts %+v ok %v", prefix, devs, d.ID, ex, conflicts, ok)
			}
			if ex {
				exclusive++
			} else {
				held++
			}
		}
	}
	// the generator must reach both answers often, or the comparison proves nothing
	if exclusive < 200 || held < 200 {
		t.Fatalf("generated %d exclusive and %d held back topics: too few of one kind", exclusive, held)
	}
}

// fleetCheck runs the report the way webFacts.TopicCheck does, with the test's facts (health, wills).
func fleetCheck(t *testing.T, devs []model.Device, h fakeFactsHealth) transport.TopicCheck {
	t.Helper()
	c, err := (webFacts{newFacts(devs, h, nil)}).TopicCheck(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// read stamps a device's group topics as read now (known).
func read(d model.Device) model.Device {
	d.GroupTopicReadAt, d.MoreGroupTopicsReadAt = factsNow, factsNow
	if d.GroupTopic == "" {
		d.GroupTopic = "tasmotas"
	}
	return d
}

func members(col transport.TopicCollision) []string {
	var out []string
	for _, m := range col.Members {
		out = append(out, m.Name+"|"+m.Topic+"|"+m.Slot)
	}
	return out
}

// Why (#1047, Norbert's table): a shared GroupTopic is intended and no collision; a GroupTopic equal to another
// device's Topic is one, and so are two equal Topics, each listed once although both devices see it; and the device
// in the collision gets the reason that holds back its writes, the other device none when its own writes are free.
func TestTopicCheckFollowsTheCollisionTable(t *testing.T) {
	t.Run("shared tasmotas is unique", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "kueche", Topic: "kueche"}), read(model.Device{ID: 2, Name: "flur", Topic: "flur"})}, nil)
		if !c.Unique() || c.Checked != 2 || len(c.ByDevice) != 0 {
			t.Errorf("check = %+v, want unique over 2 devices", c)
		}
	})
	t.Run("GroupTopic2 of flur is kueche's Topic", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "kueche", Topic: "kueche"}),
			read(model.Device{ID: 2, Name: "flur", Topic: "flur", MoreGroupTopics: []string{"kueche"}})}, nil)
		if len(c.Collisions) != 1 || c.Collisions[0].Topic != "kueche" || c.Collisions[0].Overlap ||
			!slices.Equal(members(c.Collisions[0]), []string{"kueche|kueche|Topic", "flur|kueche|one of GroupTopic2 to 4"}) {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
		if got := c.Of(1); len(got) != 1 || got[0].OtherName != "flur" || got[0].Slot != transport.SlotGroupTopic2to4 {
			t.Errorf("kueche's reasons = %+v", got)
		}
		if got := c.Of(2); len(got) != 0 {
			t.Errorf("flur's writes are not held back by kueche, reasons = %+v", got)
		}
	})
	t.Run("two equal Topics are one collision", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "one", Topic: "lamp"}), read(model.Device{ID: 2, Name: "two", Topic: "lamp"})}, nil)
		if len(c.Collisions) != 1 || !slices.Equal(members(c.Collisions[0]), []string{"one|lamp|Topic", "two|lamp|Topic"}) {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
		if len(c.Of(1)) != 1 || len(c.Of(2)) != 1 {
			t.Errorf("both devices are held back: %+v", c.ByDevice)
		}
	})
	t.Run("a Topic equal to the default group topic", func(t *testing.T) {
		// Choose refuses it on its own (mqttBlocker), whether or not another device keeps tasmotas as its group.
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "odd", Topic: transport.DefaultGroupTopic, GroupTopic: "g"}),
			read(model.Device{ID: 2, Name: "plug", Topic: "plug"})}, nil)
		if len(c.Collisions) != 1 || !c.Collisions[0].Default || c.Collisions[0].Topic != transport.DefaultGroupTopic {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
		if !slices.Equal(members(c.Collisions[0]), []string{"odd|tasmotas|Topic", "plug|tasmotas|GroupTopic1"}) {
			t.Errorf("members = %v", members(c.Collisions[0]))
		}
		if got := c.Of(1); len(got) != 2 || got[1].Kind != transport.ConflictDefaultGroupTopic {
			t.Errorf("reasons = %+v, want plug's group topic and the default group topic", got)
		}
		if len(c.Of(2)) != 0 {
			t.Errorf("plug keeps tasmotas as its group; its own writes are free: %+v", c.Of(2))
		}
	})
	t.Run("a device without a Topic that listens on every command is one entry", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "a", Topic: "a"}), read(model.Device{ID: 2, Name: "b", Topic: "b"}),
			{ID: 3, Name: "bare", FullTopic: "%prefix%/%topic%/"}}, nil)
		if len(c.Collisions) != 1 || !c.Collisions[0].Overlap || c.Collisions[0].Topic != "cmnd" ||
			!slices.Equal(members(c.Collisions[0]), []string{"a|a|Topic", "bare||Topic", "b|b|Topic"}) {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
		if c.Checked != 2 {
			t.Errorf("checked = %d: bare has no Topic", c.Checked)
		}
	})
	// lamp's FullTopic puts its group topic under kitchen's command topic too: the same entry, a member more.
	t.Run("overlapping command topics are one collision under the shorter stem", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "kitchen", Topic: "kitchen"}),
			read(model.Device{ID: 2, Name: "lamp", Topic: "lamp", FullTopic: "%prefix%/kitchen/%topic%/"})}, nil)
		if len(c.Collisions) != 1 || c.Collisions[0].Topic != "cmnd/kitchen" || !c.Collisions[0].Overlap ||
			!slices.Equal(members(c.Collisions[0]), []string{"kitchen|kitchen|Topic", "lamp|lamp|Topic", "lamp|tasmotas|GroupTopic1"}) {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
	})
	t.Run("SetOption75's stem is its own entry", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{read(model.Device{ID: 1, Name: "kitchen", Topic: "kitchen"}),
			read(model.Device{ID: 2, Name: "g", Topic: "b", FullTopic: "x/%prefix%/%topic%", MoreGroupTopics: []string{"kitchen/sub"}})}, nil)
		if len(c.Collisions) != 1 || !c.Collisions[0].SetOption75 || c.Collisions[0].Topic != "cmnd/kitchen" {
			t.Fatalf("collisions = %+v", c.Collisions)
		}
	})
}

// Why (#1047): "All device topics are unique" is a promise that MQTT writes are not held back by topics. A device whose
// group topics are not read yet (every device right after a start of 0.42 or later, until the reader asked it) could
// listen on any topic, and a device whose FullTopic TasmoFleet cannot expand could too: both are unknown, never
// unique, and the card must say whether each one holds back the other devices' writes (the gate's exemptions).
func TestTopicCheckListsUnknownDevicesApartAndNeverAsUnique(t *testing.T) {
	me := read(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs})
	t.Run("GroupTopic2 to 4 never read", func(t *testing.T) {
		x := model.Device{ID: 2, Name: "garage", Topic: "b", GroupTopic: "tasmotas", GroupTopicReadAt: factsNow} // 2 to 4 never read
		c := fleetCheck(t, []model.Device{me, x}, fakeFactsHealth{2: {LWT: "Online"}})
		if c.Unique() || len(c.Collisions) != 0 {
			t.Fatalf("check = %+v: an unread device is not unique", c)
		}
		want := []transport.TopicUnknown{{ID: 2, Name: "garage", Topic: "b", Reason: transport.UnknownGroupTopicsUnread, HoldsBack: true}}
		if !slices.Equal(c.Unknown, want) || c.UnknownCount(transport.UnknownGroupTopicsUnread) != 1 {
			t.Errorf("unknown = %+v", c.Unknown)
		}
	})
	t.Run("an unread device whose last will is Offline holds nothing back", func(t *testing.T) {
		x := model.Device{ID: 2, Name: "garage", Topic: "b"}
		c := fleetCheck(t, []model.Device{me, x}, fakeFactsHealth{2: {LWT: "Offline"}})
		if c.Unique() || len(c.Unknown) != 1 || c.Unknown[0].HoldsBack {
			t.Errorf("unknown = %+v, want garage listed, not holding writes back", c.Unknown)
		}
	})
	t.Run("an unread device provably off our broker holds nothing back", func(t *testing.T) {
		x := model.Device{ID: 2, Name: "garage", Topic: "b", MQTT: model.MQTTOtherBroker}
		c := fleetCheck(t, []model.Device{me, x}, fakeFactsHealth{1: {LWT: "Online"}})
		if len(c.Unknown) != 1 || c.Unknown[0].HoldsBack {
			t.Errorf("unknown = %+v, want garage listed, not holding writes back", c.Unknown)
		}
	})
	t.Run("a FullTopic that cannot be expanded holds back every device", func(t *testing.T) {
		x := read(model.Device{ID: 2, Name: "host", Topic: "b", FullTopic: "%prefix%/%hostname%/%topic%/"})
		c := fleetCheck(t, []model.Device{me, x}, fakeFactsHealth{2: {LWT: "Offline"}})
		want := []transport.TopicUnknown{{ID: 2, Name: "host", Topic: "b", Reason: transport.UnknownFullTopic, HoldsBack: true}}
		if !slices.Equal(c.Unknown, want) || len(c.Collisions) != 0 || c.Unique() {
			t.Errorf("check = %+v, want host unknown once (FullTopic), holding back", c)
		}
		if len(c.Of(1)) != 0 {
			t.Errorf("the unknown FullTopic is not a collision of desk: %+v", c.Of(1))
		}
	})
	t.Run("only devices with a topic count", func(t *testing.T) {
		c := fleetCheck(t, []model.Device{{ID: 1, Name: "http only"}}, nil)
		if c.Checked != 0 || !c.Unique() {
			t.Errorf("check = %+v", c)
		}
	})
}

// Why (#1047, fail closed): an inventory that cannot be read must never come back as a check without findings, which
// the card would show as "unique".
func TestTopicCheckReturnsTheInventoryError(t *testing.T) {
	f := newFacts(nil, nil, nil)
	f.Devices = failingList{}
	if _, err := (webFacts{f}).TopicCheck(t.Context()); err == nil {
		t.Fatal("want the error")
	}
}

type failingList struct{ fakeFactsDevices }

func (failingList) ListDevices(context.Context) ([]model.Device, error) {
	return nil, errors.New("disk gone")
}

// Why (#1047): the card and every Identity card render ask for the check, the device page on each live tick; it must
// cost one inventory read for the whole fleet, never one per device.
func TestTopicCheckReadsTheInventoryOnce(t *testing.T) {
	var devs []model.Device
	for i := 1; i <= 50; i++ {
		devs = append(devs, read(model.Device{ID: model.DeviceID(i), Topic: "t" + strconv.Itoa(i)}))
	}
	count := &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f := newFacts(devs, nil, nil)
	f.Devices = count
	c, err := (webFacts{f}).TopicCheck(t.Context())
	if err != nil || c.Checked != 50 || !c.Unique() {
		t.Fatalf("check = %+v, %v", c, err)
	}
	if count.lists != 1 || count.gets != 0 {
		t.Errorf("lists = %d, gets = %d; want one list", count.lists, count.gets)
	}
}
