package transport

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#1047): the Identity card asks the check for one device's reasons, and the card's status line counts the unknown
// devices per reason and says "unique" only when nothing is in doubt. A device without reasons must get none (no hint),
// and one unknown device of either kind must keep the check from being unique.
func TestTopicCheckAnswersPerDeviceAndNeverUniqueWithAnUnknownDevice(t *testing.T) {
	c := TopicCheck{ByDevice: map[model.DeviceID][]TopicConflict{1: {{Kind: ConflictSameTopic, Topic: "a", OtherID: 2}}}}
	if got := c.Of(1); len(got) != 1 || got[0].OtherID != 2 {
		t.Errorf("Of(1) = %+v", got)
	}
	if got := c.Of(3); len(got) != 0 {
		t.Errorf("Of(3) = %+v, want none", got)
	}
	if !(TopicCheck{}).Unique() {
		t.Error("an empty check is unique")
	}
	if (TopicCheck{Collisions: []TopicCollision{{Topic: "a"}}}).Unique() {
		t.Error("a collision is not unique")
	}
	u := TopicCheck{Unknown: []TopicUnknown{{ID: 1, Reason: UnknownGroupTopicsUnread}, {ID: 2, Reason: UnknownFullTopic}, {ID: 3, Reason: UnknownGroupTopicsUnread}}}
	if u.Unique() {
		t.Error("an unknown device is not unique")
	}
	if u.UnknownCount(UnknownGroupTopicsUnread) != 2 || u.UnknownCount(UnknownFullTopic) != 1 {
		t.Errorf("counts = %d, %d", u.UnknownCount(UnknownGroupTopicsUnread), u.UnknownCount(UnknownFullTopic))
	}
}
