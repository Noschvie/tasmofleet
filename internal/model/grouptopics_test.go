package model

import (
	"slices"
	"testing"
	"time"
)

// Why (#1005): GroupTopics and GroupTopicsReadAt are the one view of all four group topics that the shared-topic check,
// the command channel and the UI use. A device listens on every non-empty one; and the four count as known only when
// GroupTopic1 and GroupTopic2 to 4 were both read (the older stamp), never when one of them was not, or a device whose
// GroupTopic2 nobody read would pass as safe.
func TestGroupTopicsAndTheirReadStamp(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		d    Device
		want []string
		at   time.Time
	}{
		{"nothing read", Device{}, nil, time.Time{}},
		{"GroupTopic1 only, 2 to 4 never read", Device{GroupTopic: "tasmotas", GroupTopicReadAt: t0}, []string{"tasmotas"}, time.Time{}},
		{"2 to 4 read, GroupTopic1 never", Device{MoreGroupTopics: []string{"a"}, MoreGroupTopicsReadAt: t0}, []string{"a"}, time.Time{}},
		{"all read, 2 to 4 empty", Device{GroupTopic: "tasmotas", GroupTopicReadAt: t0, MoreGroupTopicsReadAt: t0}, []string{"tasmotas"}, t0},
		{"the older stamp counts", Device{GroupTopic: "tasmotas", GroupTopicReadAt: t0.Add(time.Hour), MoreGroupTopics: []string{"a", "", "b"},
			MoreGroupTopicsReadAt: t0}, []string{"tasmotas", "a", "b"}, t0},
		{"INFO1 older than the list", Device{GroupTopic: "x", GroupTopicReadAt: t0, MoreGroupTopicsReadAt: t0.Add(time.Hour)}, []string{"x"}, t0},
	} {
		if got := tc.d.GroupTopics(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: GroupTopics = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.d.GroupTopicsReadAt(); !got.Equal(tc.at) {
			t.Errorf("%s: GroupTopicsReadAt = %v, want %v", tc.name, got, tc.at)
		}
	}
}
