package web

import (
	"context"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// The Topic check card on System, MQTT and the hint on the Identity card (#1047): what the write gate's topic rule
// finds, for the fleet and for one device. Both only show transport.TopicCheck, which app computes with the gate's own
// rule; nothing here decides whether a topic collides, and nothing is sent to a device.

// TopicCheckSource gives the write gate's topic rule for the whole fleet; app's webFacts implements it with one read of
// the inventory. Without it the card is not shown and the Identity card has no hint.
type TopicCheckSource interface {
	TopicCheck(ctx context.Context) (transport.TopicCheck, error)
}

// DeviceFactsSource is the device page's read (#1151): the transport facts of the device and the topic conflicts that hold
// back MQTT writes to it, from ONE read of the inventory (the page used to list it once for Facts and once for TopicCheck).
// app's webFacts implements it; the conflicts are the Topic check card's own for that device, so the hint cannot drift from
// the card. Optional: without it the page reads Facts and TopicCheck apart, as before. The router never uses it: it reads
// its own list per call.
type DeviceFactsSource interface {
	DeviceFacts(ctx context.Context, d model.Device) (transport.Facts, []transport.TopicConflict, error)
}

// topicCheckAnchor is the card on the Settings page; the hint's "+N more" links there (admins only: the page is theirs).
const topicCheckAnchor = sysPath + "?tab=mqtt#card-topiccheck"

// topicsCard is the card: the status line, the collisions and the unknown devices.
type topicsCard struct {
	Tone, Title, Text string
	Failed            bool // the check could not be read: the status line says so and nothing else is shown
	Collisions        []topicCollisionView
	Unknown           []topicUnknownView
}

type topicCollisionView struct {
	Topic   string
	Note    string // what kind of collision it is
	Members []topicMemberView
}

type topicMemberView struct {
	Name, Href, Use string
}

type topicUnknownView struct {
	Name, Href, Reason string
	Muted              string // the device does not hold back other devices' writes: why that is so
}

// foldOpen is how many entries a list shows open; a longer one starts folded.
const foldOpen = 5

// topicCheckOf reads the check for the card; nil without a source (the card is not shown).
func (h *handler) topicCheckOf(ctx context.Context, base string) *topicsCard {
	src, ok := h.d.Transport.(TopicCheckSource)
	if !ok {
		return nil
	}
	c, err := src.TopicCheck(ctx)
	if err != nil {
		h.log.Warn("topic check: could not read it", "err", err)
		return &topicsCard{Failed: true, Tone: "danger", Title: "The topic check could not be read",
			Text: "TasmoFleet could not list the devices. MQTT writes still follow the check on every call."}
	}
	return topicsCardOf(c, base)
}

// topicsCardOf words the check. "All device topics are unique" only when there is no collision and no unknown
// device: an unknown device could listen on any topic.
func topicsCardOf(c transport.TopicCheck, base string) *topicsCard {
	v := &topicsCard{}
	href := func(id model.DeviceID) string { return base + devPath(id, "") }
	for _, col := range c.Collisions {
		cv := topicCollisionView{Topic: col.Topic}
		switch {
		case col.SetOption75:
			cv.Note = "With SetOption75 on, a group topic is subscribed as " + col.Topic + "/…, whatever the FullTopic says. TasmoFleet does not read SetOption75, so it counts."
		case col.Overlap:
			cv.Note = "Command topics under " + col.Topic + ": a command to one also reaches the other."
		case col.Default:
			cv.Note = "Tasmota's default group topic, which many devices listen on."
		}
		for _, m := range col.Members {
			use := m.Slot
			switch {
			case col.Overlap && m.Topic == "":
				use = "no Topic: listens on every command under " + col.Topic
			case col.Overlap:
				use += " " + m.Topic
			}
			cv.Members = append(cv.Members, topicMemberView{Name: m.Name, Href: href(m.ID), Use: use})
		}
		v.Collisions = append(v.Collisions, cv)
	}
	for _, u := range c.Unknown {
		uv := topicUnknownView{Name: u.Name, Href: href(u.ID), Reason: "Group topics not read yet"}
		if u.Reason == transport.UnknownFullTopic {
			uv.Reason = "FullTopic cannot be expanded (no %topic%, or another placeholder): holds back MQTT writes to every device"
		}
		if !u.HoldsBack {
			uv.Muted = "Does not hold back MQTT writes to other devices (offline, or not on TasmoFleet's broker)."
		}
		v.Unknown = append(v.Unknown, uv)
	}
	var parts []string
	if n := len(c.Collisions); n > 0 {
		parts = append(parts, plural(n, "collision"))
	}
	if n := c.UnknownCount(transport.UnknownGroupTopicsUnread); n > 0 {
		parts = append(parts, plural(n, "device")+" not fully read yet")
	}
	if n := c.UnknownCount(transport.UnknownFullTopic); n > 0 {
		parts = append(parts, plural(n, "device")+" with a FullTopic TasmoFleet cannot expand")
	}
	switch {
	case c.Checked == 0 && len(parts) == 0:
		v.Tone, v.Title, v.Text = "neutral", "No device has an MQTT topic", "Devices reached over HTTP only have nothing to check."
	case len(parts) == 0 && c.Unique():
		v.Tone, v.Title, v.Text = "ok", "All device topics are unique", "Checked "+plural(c.Checked, "device")+" with an MQTT topic."
	case len(c.Collisions) > 0:
		v.Tone, v.Title = "warn", strings.Join(parts, " · ")
		v.Text = "MQTT writes to a device listed with its Topic are held back; they go over HTTP where it works. " +
			"A device listed with a group topic only listens on it; its own writes are not held back by it."
	default:
		v.Tone, v.Title = "neutral", strings.Join(parts, " · ")
		v.Text = "No collision among the devices TasmoFleet has read."
	}
	return v
}

// topicHint is the Identity card's line when MQTT writes to the device are held back by a collision: the first
// reason, worded around the other device's name (a link), and how many more there are.
type topicHint struct {
	Lead           string // "MQTT writes are held back: "
	Before, After  string // around the other device's name; After alone when there is no other device
	Other, OtherAt string // the other device and its page; "" when the reason is the device itself
	More           int
	MoreHref       string // the Topic check card, for admins; "" leaves "+N more" as text
}

// sig is the hint for identSig: the live refresh swaps the card when it appears, changes or goes.
func (t *topicHint) sig() string {
	if t == nil {
		return ""
	}
	return t.Lead + t.Before + t.Other + "|" + t.OtherAt + "|" + t.After + "|" + strconv.Itoa(t.More)
}

// topicHintOf words the device's first conflict; nil when it has none (the hint covers collisions only: devices not
// read yet are the Paths card's business). A device not on TasmoFleet's broker sends no MQTT write at all, so for it
// the sentence says what would happen once it is.
func topicHintOf(d model.Device, conflicts []transport.TopicConflict, base string, admin bool) *topicHint {
	if len(conflicts) == 0 {
		return nil
	}
	k := conflicts[0]
	t := &topicHint{Lead: "MQTT writes are held back: ", More: len(conflicts) - 1}
	if d.MQTT != model.MQTTOurs {
		t.Lead = "Once the device is on TasmoFleet's broker, MQTT writes would be held back: "
	}
	if admin && t.More > 0 {
		t.MoreHref = base + topicCheckAnchor
	}
	own := k.OtherID == d.ID
	if !own && k.OtherID != 0 {
		t.Other, t.OtherAt = k.OtherName, base+devPath(k.OtherID, "")
	}
	switch k.Kind {
	case transport.ConflictSameTopic:
		t.Before = "topic " + d.Topic + " is also the Topic of "
	case transport.ConflictGroupIsTopic:
		t.Before = "topic " + d.Topic + " is " + k.Slot + " of "
	case transport.ConflictOwnGroupIsTopic:
		t.After = "topic " + d.Topic + " is also " + ownSlot(k.Slot) + " of this device"
	case transport.ConflictStemOverlap:
		if k.Topic == "" {
			t.After = " has no Topic and listens on every command under " + k.Stem
		} else {
			t.Before, t.After = "its command topic overlaps the one of ", " (Topic "+k.Topic+", both under "+k.Stem+")"
		}
	case transport.ConflictGroupStemOverlap:
		if own {
			t.After = "its own group topic " + k.Topic + " lies on its command topic (under " + k.Stem + ")"
		} else {
			t.Before, t.After = "group topic "+k.Topic+" of ", " lies on its command topic (under "+k.Stem+")"
		}
	case transport.ConflictSetOption75:
		if own {
			t.After = "its own group topic " + k.Topic + " lies on its command topic with SetOption75 on (" + k.Stem + ")"
		} else {
			t.Before, t.After = "group topic "+k.Topic+" of ", " lies on its command topic with SetOption75 on ("+k.Stem+")"
		}
	case transport.ConflictDefaultGroupTopic:
		t.After = "topic " + d.Topic + " is Tasmota's default group topic, which many devices listen on"
	case transport.ConflictOtherStemUnknown: // never in a device's conflicts (the check lists it under Unknown)
		t.After = "its topic is not exclusive"
	}
	return t
}

// ownSlot names the device's own group topic slot.
func ownSlot(slot string) string {
	if slot == transport.SlotGroupTopic2to4 {
		return "one of the GroupTopic2 to 4"
	}
	return slot
}
