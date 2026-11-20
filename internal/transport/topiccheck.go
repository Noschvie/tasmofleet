package transport

import "github.com/hellwand/tasmofleet/internal/model"

// TopicConflictKind is why a device's topic is not exclusive (#1047): one branch of the write gate's topic rule (the
// facts source's topicConflicts, which MQTTFacts.TopicExclusive is computed from), plus the default group topic that
// Choose refuses on its own (mqttBlocker).
type TopicConflictKind string

// Conflict kinds. "The device" is the one whose writes are held back, "the other" the device that listens on its
// topic (the device itself for its own group topics).
const (
	ConflictSameTopic         TopicConflictKind = "same_topic"          // the other device has the same Topic
	ConflictStemOverlap       TopicConflictKind = "stem_overlap"        // the command topics of the two Topics overlap
	ConflictGroupIsTopic      TopicConflictKind = "group_is_topic"      // a group topic of the other device is the Topic
	ConflictGroupStemOverlap  TopicConflictKind = "group_stem_overlap"  // a group topic's command topic overlaps the device's
	ConflictSetOption75       TopicConflictKind = "setoption75_stem"    // a group topic's SetOption75 stem overlaps the device's
	ConflictOwnGroupIsTopic   TopicConflictKind = "own_group_is_topic"  // one of the device's own group topics is its Topic
	ConflictDefaultGroupTopic TopicConflictKind = "default_group_topic" // the Topic is DefaultGroupTopic
	ConflictOtherStemUnknown  TopicConflictKind = "other_stem_unknown"  // the other device's FullTopic cannot be expanded
)

// Where a device keeps a topic. The model stores GroupTopic2 to GroupTopic4 as a list of the non-empty ones (#1005),
// so their number is not known.
const (
	SlotTopic          = "Topic"
	SlotGroupTopic1    = "GroupTopic1"
	SlotGroupTopic2to4 = "one of GroupTopic2 to 4"
)

// TopicConflict is one reason a device's topic is not exclusive.
type TopicConflict struct {
	Kind TopicConflictKind
	// Topic is the other side's topic text: the other device's Topic or the group topic (equal to the device's
	// Topic for the kinds that compare texts).
	Topic string
	// Stem is, for the overlap kinds, the command topic both listen under: the shorter of the two stems.
	Stem      string
	OtherID   model.DeviceID // 0 for ConflictDefaultGroupTopic; the device's own ID for its own group topics
	OtherName string
	Slot      string // where the other device keeps Topic: SlotTopic, SlotGroupTopic1 or SlotGroupTopic2to4
}

// TopicUnknownReason is why a device's topics are not fully known.
type TopicUnknownReason string

// Unknown reasons.
const (
	UnknownGroupTopicsUnread TopicUnknownReason = "group_topics_unread" // GroupTopic1 to 4 not read since the last probe
	UnknownFullTopic         TopicUnknownReason = "fulltopic"           // the FullTopic cannot be expanded
)

// TopicUnknown is a device whose topics TasmoFleet does not fully know.
type TopicUnknown struct {
	ID     model.DeviceID
	Name   string
	Topic  string
	Reason TopicUnknownReason
	// HoldsBack says the device holds back MQTT writes to other devices; false for an unread device that no message on
	// TasmoFleet's broker can reach (last will Offline, provably not on the broker). Its own writes are held back
	// either way.
	HoldsBack bool
}

// TopicMember is one device of a collision and how it uses the topic.
type TopicMember struct {
	ID    model.DeviceID
	Name  string
	Topic string // the device's Topic or group topic that collides ("" for a device without a Topic)
	Slot  string
}

// TopicCollision is a topic (or, for overlapping command topics, the stem) more than one device listens on, with
// every device involved, each pair listed once.
type TopicCollision struct {
	Topic       string
	Overlap     bool // Topic is a command stem the members' command topics lie under or equal
	SetOption75 bool // the overlap is through SetOption75's group stem (SetOption75 is not read)
	Default     bool // Topic is DefaultGroupTopic
	Members     []TopicMember
}

// TopicCheck is the result of the write gate's topic rule for the whole fleet (#1047), read from one inventory list.
// It is advisory, for the UI only: the router never reads it, MQTTFacts.TopicExclusive is computed afresh per call.
type TopicCheck struct {
	Checked    int // devices with an MQTT topic
	Collisions []TopicCollision
	Unknown    []TopicUnknown
	// ByDevice holds each device's own conflicts, the reasons MQTT writes to it are held back
	// (ConflictOtherStemUnknown is under Unknown instead).
	ByDevice map[model.DeviceID][]TopicConflict
}

// Of returns the conflicts that hold back MQTT writes to the device, none when it has none.
func (c TopicCheck) Of(id model.DeviceID) []TopicConflict { return c.ByDevice[id] }

// UnknownCount counts the devices in Unknown for one reason.
func (c TopicCheck) UnknownCount(r TopicUnknownReason) int {
	n := 0
	for _, u := range c.Unknown {
		if u.Reason == r {
			n++
		}
	}
	return n
}

// Unique says every device topic is exclusive and fully known: no collision, no unknown device.
func (c TopicCheck) Unique() bool { return len(c.Collisions) == 0 && len(c.Unknown) == 0 }
