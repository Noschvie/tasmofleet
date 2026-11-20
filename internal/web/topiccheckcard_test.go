package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// topicFacts is a facts source that also gives the fleet's topic check (app's webFacts does both).
type topicFacts struct {
	fakeFacts
	check transport.TopicCheck
	err   error
	reads *int // counts the checks asked for
}

func (s topicFacts) TopicCheck(context.Context) (transport.TopicCheck, error) {
	if s.reads != nil {
		*s.reads++
	}
	return s.check, s.err
}

func withTopics(s topicFacts) func(*Deps) { return func(d *Deps) { d.Transport = s } }

// collisionCheck: GroupTopic2 of "flur <b>" (device 2) is kueche's Topic (device 1, whose name is "kitchen" in the env),
// and "garage" (device 3) has never had its group topics read but is offline.
func collisionCheck() transport.TopicCheck {
	return transport.TopicCheck{Checked: 3,
		Collisions: []transport.TopicCollision{{Topic: "kueche", Members: []transport.TopicMember{
			{ID: 1, Name: "kitchen", Topic: "kueche", Slot: transport.SlotTopic},
			{ID: 2, Name: "flur <b>", Topic: "kueche", Slot: transport.SlotGroupTopic2to4}}}},
		Unknown: []transport.TopicUnknown{{ID: 3, Name: "garage", Topic: "garage", Reason: transport.UnknownGroupTopicsUnread}},
		ByDevice: map[model.DeviceID][]transport.TopicConflict{1: {
			{Kind: transport.ConflictGroupIsTopic, Topic: "kueche", OtherID: 2, OtherName: "flur <b>", Slot: transport.SlotGroupTopic2to4}}},
	}
}

func mqttTab(t *testing.T, mutate func(*Deps)) string {
	t.Helper()
	e, _, _, _ := sysEnvWithDeps(t, mutate)
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	return body
}

// topicCardOf cuts the Topic check card out of the page.
func topicCardOf(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `id="card-topiccheck"`)
	if i < 0 {
		t.Fatal("no Topic check card on the MQTT tab")
	}
	rest := body[i:]
	if j := strings.Index(rest, "</section>"); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// Why (#1047): the card is where a person finds out why MQTT writes to a device fall back to HTTP. Each collision
// must name every device involved, each a link to its page, with how it uses the topic; a device not read yet must be
// listed apart (never counted as unique), with the note when it does not hold back the others; a name is escaped.
func TestTopicCheckCardListsCollisionsAndUnknownDevicesWithLinks(t *testing.T) {
	card := topicCardOf(t, mqttTab(t, withTopics(topicFacts{check: collisionCheck()})))
	for _, want := range []string{
		"1 collision · 1 device not fully read yet",
		`href="/tf/devices/1"`, `href="/tf/devices/2"`, `href="/tf/devices/3"`,
		"flur &lt;b&gt;", "one of GroupTopic2 to 4",
		"Group topics not read yet",
		"Does not hold back MQTT writes to other devices",
		"right after a start or a GroupTopic change",
		// the gate holds back writes only to the device whose Topic collides; a device listed by a group topic
		// only listens on it, and its own writes go over MQTT (its Identity card has no hint)
		"MQTT writes to a device listed with its Topic are held back",
		"A device listed with a group topic only listens on it; its own writes are not held back by it.",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "flur <b>") {
		t.Error("a device name reached the page unescaped")
	}
	if strings.Contains(card, "unique") {
		t.Error("the card says unique while devices collide")
	}
	if strings.Contains(card, "a device in a collision are held back") {
		t.Error("the card says every member of a collision is held back, also one listed only by a group topic")
	}
}

// Why (#1047): "All device topics are unique" is a promise that topics hold no MQTT write back; it may only show when
// the check found nothing at all. An unknown device, or a check that could not be read, must never read as unique.
func TestTopicCheckCardSaysUniqueOnlyWhenNothingIsInDoubt(t *testing.T) {
	cases := []struct {
		name   string
		src    topicFacts
		want   string
		unwant string
	}{
		{"unique", topicFacts{check: transport.TopicCheck{Checked: 4}}, "All device topics are unique", "not fully read"},
		{"an unknown device", topicFacts{check: transport.TopicCheck{Checked: 4, Unknown: []transport.TopicUnknown{{ID: 3, Name: "garage", Reason: transport.UnknownGroupTopicsUnread, HoldsBack: true}}}},
			"1 device not fully read yet", "unique"},
		{"a FullTopic that cannot be expanded", topicFacts{check: transport.TopicCheck{Checked: 4, Unknown: []transport.TopicUnknown{{ID: 3, Name: "host", Reason: transport.UnknownFullTopic, HoldsBack: true}}}},
			"1 device with a FullTopic TasmoFleet cannot expand", "unique"},
		{"the inventory cannot be read", topicFacts{err: errors.New("disk gone")}, "The topic check could not be read", "unique"},
		{"no device with a topic", topicFacts{}, "No device has an MQTT topic", "unique"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := topicCardOf(t, mqttTab(t, withTopics(tc.src)))
			status := card[strings.Index(card, `id="topiccheck-status"`):]
			status = status[strings.Index(status, "<b>"):strings.Index(status, "</b>")]
			if !strings.Contains(status, tc.want) {
				t.Errorf("status line %q lacks %q", status, tc.want)
			}
			if strings.Contains(status, tc.unwant) {
				t.Errorf("status line %q says %q", status, tc.unwant)
			}
			if strings.Contains(card, "Does not hold back") {
				t.Error("a device that holds writes back is marked as not holding them back")
			}
		})
	}
}

// Why (#1047): without a source of the check (a build without the transport facts) there is nothing true to show.
func TestTopicCheckCardIsAbsentWithoutTheSource(t *testing.T) {
	if body := mqttTab(t, withFacts(fakeFacts{})); strings.Contains(body, `id="card-topiccheck"`) {
		t.Error("the card shows without a topic check source")
	}
}

// topicDevices puts topics on the env's devices: kitchen (1) has the Topic kueche on TasmoFleet's broker.
func topicDevices(d *Deps) {
	d.Inventory = newInv(
		model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", Topic: "kueche", MQTT: model.MQTTOurs},
		model.Device{ID: 2, Name: "flur <b>", Address: "10.0.0.2", Topic: "flur", MQTT: model.MQTTOurs, GroupTopic: "tasmotas", MoreGroupTopics: []string{"kueche"}},
	)
}

func identityOf(t *testing.T, e *env, id, role string) string {
	t.Helper()
	_, body := e.do(t, "GET", "/devices/"+id, role, nil)
	i := strings.Index(body, `id="identity"`)
	if i < 0 {
		t.Fatal("no Identity card")
	}
	rest := body[i:]
	return rest[:strings.Index(rest, "</section>")]
}

// Why (#1047): the device page must say on the device itself that its MQTT writes are held back, naming the topic and
// the other device (a link) where it keeps it; a device whose writes are free gets no hint.
func TestIdentityCardHintsAtTheCollisionThatHoldsBackWrites(t *testing.T) {
	src := topicFacts{check: collisionCheck()}
	e := setupWith(t, func(d *Deps) { topicDevices(d); withTopics(src)(d) })
	card := identityOf(t, e, "1", "admin")
	for _, want := range []string{`id="topic-hint"`, "MQTT writes are held back: topic kueche is one of GroupTopic2 to 4 of",
		`<a href="/tf/devices/2">flur &lt;b&gt;</a>`} {
		if !strings.Contains(card, want) {
			t.Errorf("Identity card lacks %q:\n%s", want, card)
		}
	}
	// the hint sits under the topic rows, before the MAC row
	if h, m := strings.Index(card, `id="topic-hint"`), strings.Index(card, ">MAC<"); h < 0 || m < h || strings.Index(card, ">MQTT topic<") > h {
		t.Errorf("the hint is not between the MQTT topic and the MAC rows:\n%s", card)
	}
	if card := identityOf(t, e, "2", "admin"); strings.Contains(card, `id="topic-hint"`) {
		t.Errorf("flur's writes are not held back, yet it shows a hint:\n%s", card)
	}
}

// Why (#1047): a device that is not on TasmoFleet's broker gets no MQTT write at all, so "held back" would be false;
// the hint says what would happen once it is.
func TestIdentityHintForADeviceOffOurBrokerSaysWhatWouldHappen(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Inventory = newInv(model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", Topic: "kueche", MQTT: model.MQTTOtherBroker})
		withTopics(topicFacts{check: collisionCheck()})(d)
	})
	card := identityOf(t, e, "1", "admin")
	if !strings.Contains(card, "Once the device is on TasmoFleet&#39;s broker, MQTT writes would be held back") || strings.Contains(card, "MQTT writes are held back") {
		t.Errorf("hint for a device off our broker:\n%s", card)
	}
}

// Why (#1047): "+N more" leads to the Topic check card, which is on System > Settings, an admin page: an operator gets
// the count as text, not a link that would refuse them.
func TestIdentityHintLinksTheMoreReasonsOnlyForAdmins(t *testing.T) {
	check := collisionCheck()
	check.ByDevice[1] = append(check.ByDevice[1],
		transport.TopicConflict{Kind: transport.ConflictSameTopic, Topic: "kueche", OtherID: 4, OtherName: "twin", Slot: transport.SlotTopic},
		transport.TopicConflict{Kind: transport.ConflictDefaultGroupTopic, Topic: "kueche", Slot: transport.SlotTopic})
	e := setupWith(t, func(d *Deps) { topicDevices(d); withTopics(topicFacts{check: check})(d) })
	if card := identityOf(t, e, "1", "admin"); !strings.Contains(card, `<a href="/tf/settings/system?tab=mqtt#card-topiccheck">+2 more</a>`) {
		t.Errorf("admin: no link to the card:\n%s", card)
	}
	card := identityOf(t, e, "1", "operator")
	if !strings.Contains(card, "+2 more") || strings.Contains(card, "card-topiccheck") {
		t.Errorf("operator: want the count as text only:\n%s", card)
	}
}

// Why (#1047, #1003): the live refresh swaps the Identity card only when its fingerprint changes; a hint that appears
// or goes without changing it would stay stale on an open page.
func TestIdentSigChangesWithTheTopicHint(t *testing.T) {
	base := identCard{ID: 1}
	with := base
	with.Topic = &topicHint{Lead: "MQTT writes are held back: ", Before: "topic kueche is GroupTopic1 of ", Other: "flur", OtherAt: "/devices/2"}
	other := base
	other.Topic = &topicHint{Lead: "MQTT writes are held back: ", Before: "topic kueche is GroupTopic1 of ", Other: "bad", OtherAt: "/devices/3"}
	if identSig(base) == identSig(with) || identSig(with) == identSig(other) {
		t.Error("identSig ignores the topic hint")
	}
}

// Why (#1047): a check that cannot be read must not break the device page; the card simply shows no hint.
func TestIdentityCardWithoutAReadableCheckShowsNoHint(t *testing.T) {
	e := setupWith(t, func(d *Deps) { topicDevices(d); withTopics(topicFacts{err: errors.New("disk gone")})(d) })
	if card := identityOf(t, e, "1", "admin"); strings.Contains(card, `id="topic-hint"`) {
		t.Errorf("a hint without a check:\n%s", card)
	}
}
