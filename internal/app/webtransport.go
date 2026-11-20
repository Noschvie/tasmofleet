package app

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// webFacts is what the web UI reads: the router's own transport facts (so the device page shows what
// the router decides), plus the batch read of the device list.
type webFacts struct{ *TransportFacts }

// FailingHTTP returns the facts of those of ids whose HTTP calls fail. The decision is made from the
// router's failure memory and the health, both in memory, so a fleet without a failure costs no
// database read at all; with failures the inventory is listed once for all of them.
func (w webFacts) FailingHTTP(ctx context.Context, ids []model.DeviceID) (map[model.DeviceID]transport.Facts, error) {
	var failing []model.DeviceID
	for _, id := range ids {
		if w.HTTPState(id).State == transport.HTTPFailing {
			failing = append(failing, id)
		}
	}
	if len(failing) == 0 {
		return nil, nil
	}
	all, err := w.Devices.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[model.DeviceID]model.Device, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	out := make(map[model.DeviceID]transport.Facts, len(failing))
	for _, id := range failing {
		d, ok := byID[id]
		if !ok {
			continue
		}
		f, err := w.factsFor(ctx, d, all)
		if err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, nil
}

// AllFacts returns the facts of every device from one read of the inventory (web.AllFactsSource): the card of
// the devices' MQTT passwords needs each device's word, and a read per device would list the inventory N times.
func (w webFacts) AllFacts(ctx context.Context) (map[model.DeviceID]transport.Facts, error) {
	all, err := w.Devices.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[model.DeviceID]transport.Facts, len(all))
	for _, d := range all {
		f, err := w.factsFor(ctx, d, all)
		if err != nil {
			return nil, err
		}
		out[d.ID] = f
	}
	return out, nil
}

// TopicCheck is the write gate's topic rule for the whole fleet (web.TopicCheckSource, #1047), from one read of the
// inventory. It only reads: no device is asked anything, and the router never reads the result (it computes
// MQTTFacts.TopicExclusive afresh per call).
func (w webFacts) TopicCheck(ctx context.Context) (transport.TopicCheck, error) {
	all, err := w.Devices.ListDevices(ctx)
	if err != nil {
		return transport.TopicCheck{}, fmt.Errorf("topic check: %w", err)
	}
	ev := w.brokerEvidence(w.Now())
	return topicCheckOf(all, w.commandPrefix(), func(o model.Device) bool { return holdsWritesBack(o, all, w.Health.Get, ev) }), nil
}

// topicCheckOf runs topicConflicts for every device with a Topic and gathers the fleet's view of it:
//   - the collisions, merged per topic: equal topic texts (the same Topic, a group topic that is a Topic, the device's
//     own group topic, the default group topic) under the topic, overlapping command topics under the stem the others
//     lie under, so a pair seen from both devices is listed once and a device that listens on everything is one entry;
//   - the unknown devices: group topics not read since the last probe (groupTopicStale, the gate's rule), each marked
//     whether it holds back MQTT writes to other devices (holds), and devices whose FullTopic cannot be expanded,
//     which hold back MQTT writes to every device;
//   - each device's own conflicts (ByDevice), plus the default group topic Choose refuses on its own (mqttBlocker reads
//     the same constant).
func topicCheckOf(all []model.Device, prefix string, holds func(model.Device) bool) transport.TopicCheck {
	c := transport.TopicCheck{ByDevice: map[model.DeviceID][]transport.TopicConflict{}}
	byKey := map[string]int{}
	unknownAt := map[model.DeviceID]int{}
	addUnknown := func(o model.Device, r transport.TopicUnknownReason, holdsBack bool) {
		if i, ok := unknownAt[o.ID]; ok {
			if r == transport.UnknownFullTopic { // the stronger finding: it holds back every device's writes
				c.Unknown[i].Reason, c.Unknown[i].HoldsBack = r, true
			}
			return
		}
		unknownAt[o.ID] = len(c.Unknown)
		c.Unknown = append(c.Unknown, transport.TopicUnknown{ID: o.ID, Name: deviceLabel(o), Topic: o.Topic, Reason: r, HoldsBack: holdsBack})
	}
	byID := make(map[model.DeviceID]model.Device, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	collide := func(key, topic string, set func(*transport.TopicCollision), members ...transport.TopicMember) {
		i, ok := byKey[key]
		if !ok {
			i = len(c.Collisions)
			byKey[key] = i
			c.Collisions = append(c.Collisions, transport.TopicCollision{Topic: topic})
		}
		col := &c.Collisions[i]
		set(col)
		for _, m := range members {
			if !slices.Contains(col.Members, m) {
				col.Members = append(col.Members, m)
			}
		}
	}
	for _, d := range all {
		if d.Topic == "" {
			continue
		}
		c.Checked++
		conflicts, ok := topicConflicts(d, all, prefix)
		if !ok {
			addUnknown(d, transport.UnknownFullTopic, true)
		}
		if groupTopicStale(d) {
			addUnknown(d, transport.UnknownGroupTopicsUnread, holds(d))
		}
		if d.Topic == transport.DefaultGroupTopic {
			conflicts = append(conflicts, transport.TopicConflict{Kind: transport.ConflictDefaultGroupTopic, Topic: d.Topic, Slot: transport.SlotTopic})
		}
		self := transport.TopicMember{ID: d.ID, Name: deviceLabel(d), Topic: d.Topic, Slot: transport.SlotTopic}
		for _, k := range conflicts {
			if k.Kind == transport.ConflictOtherStemUnknown {
				addUnknown(byID[k.OtherID], transport.UnknownFullTopic, true)
				continue
			}
			c.ByDevice[d.ID] = append(c.ByDevice[d.ID], k)
			other := transport.TopicMember{ID: k.OtherID, Name: deviceLabel(byID[k.OtherID]), Topic: k.Topic, Slot: k.Slot}
			switch k.Kind {
			case transport.ConflictDefaultGroupTopic:
				collide("eq|"+d.Topic, d.Topic, func(t *transport.TopicCollision) { t.Default = true }, self)
			case transport.ConflictStemOverlap, transport.ConflictGroupStemOverlap:
				collide("ov|"+k.Stem, k.Stem, func(t *transport.TopicCollision) { t.Overlap = true }, self, other)
			case transport.ConflictSetOption75:
				collide("so75|"+k.Stem, k.Stem, func(t *transport.TopicCollision) { t.Overlap, t.SetOption75 = true, true }, self, other)
			case transport.ConflictSameTopic, transport.ConflictGroupIsTopic, transport.ConflictOwnGroupIsTopic, transport.ConflictOtherStemUnknown:
				// equal topic texts (ConflictOtherStemUnknown went to Unknown above)
				collide("eq|"+d.Topic, d.Topic, func(*transport.TopicCollision) {}, self, other)
			}
		}
	}
	return c
}

// deviceLabel is how the check names a device: its name, else its topic, else its number.
func deviceLabel(d model.Device) string {
	switch {
	case d.Name != "":
		return d.Name
	case d.Topic != "":
		return d.Topic
	}
	return "device " + strconv.FormatInt(int64(d.ID), 10)
}
