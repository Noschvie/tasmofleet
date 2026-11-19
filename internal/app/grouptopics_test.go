package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/devicecmd"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// allRead marks a device's four group topics as read at at: GroupTopic1 as given, GroupTopic2 to 4 as more.
func allRead(d model.Device, at time.Time, first string, more ...string) model.Device {
	d.GroupTopic, d.GroupTopicReadAt = first, at
	d.MoreGroupTopics, d.MoreGroupTopicsReadAt = more, at
	return d
}

// Why (#1005, the issue): a device listens on every one of its four group topics. One whose GroupTopic2 is another
// device's topic receives every command published to that device, so a write to it over MQTT would run on both. The
// facts must see GroupTopic2 to GroupTopic4 like GroupTopic1, and Choose must then keep the write off MQTT.
func TestADeviceListeningViaGroupTopic2HoldsBackTheMQTTWrite(t *testing.T) {
	d := allRead(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}, factsNow, "tasmotas")
	for _, more := range [][]string{{"a"}, {"hall", "a"}, {"hall", "x", "a"}} {
		o := allRead(model.Device{ID: 2, Name: "lamp", Topic: "b", MQTT: model.MQTTOurs}, factsNow, "tasmotas", more...)
		f, err := newFacts([]model.Device{d, o}, fakeFactsHealth{1: {LWT: "Online"}, 2: {LWT: "Online"}}, nil).Facts(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if f.MQTT.TopicExclusive {
			t.Errorf("lamp listens on desk's topic as one of %v, but desk's topic counts as exclusive", more)
		}
		if p := transport.Choose(f); p.Writes == transport.MQTT {
			t.Errorf("group topics %v: the write goes over MQTT and would reach lamp too", more)
		}
	}
	// the same device with group topics that are not desk's topic does not hold desk back
	o := allRead(model.Device{ID: 2, Name: "lamp", Topic: "b", MQTT: model.MQTTOurs}, factsNow, "tasmotas", "hall", "kitchen")
	f, err := newFacts([]model.Device{d, o}, fakeFactsHealth{1: {LWT: "Online"}, 2: {LWT: "Online"}}, nil).Facts(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := transport.Choose(f); p.Writes != transport.MQTT {
		t.Errorf("writes = %s (%s), want MQTT: no group topic of lamp is desk's topic", p.Writes, p.Reason(transport.ActWrites))
	}
}

// Why (#1005, design check): a device whose own GroupTopic3 is its own topic passes every command to it on to the whole
// group. Choose compares the own topic with GroupTopic1 only; the facts must catch the others.
func TestADeviceWhoseOwnGroupTopic3IsItsTopicGetsNoMQTTWrite(t *testing.T) {
	d := allRead(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}, factsNow, "tasmotas", "hall", "a")
	f, err := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, nil).Facts(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := transport.Choose(f); p.Writes == transport.MQTT {
		t.Error("the device's GroupTopic3 is its topic, yet the write goes over MQTT")
	}
}

// Why (#1005, the maintainer's decision): GroupTopic2 to GroupTopic4 never read (every device right after the upgrade)
// could be anything, exactly like an unread GroupTopic1: they hold back other devices' MQTT writes under the same rules
// (#812, #972: a last will Offline and "provably not on our broker" still exempt the device), and a device's own unread
// ones keep its writes off MQTT while its reads stay there. A list that was read and is empty is known: that is the
// normal case, and treating it as unread would hold back the whole fleet forever.
func TestUnreadGroupTopics2To4HoldBackLikeAnUnreadGroupTopic1(t *testing.T) {
	desk := allRead(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}, factsNow, "tasmotas")
	gt1Only := func(m model.MQTTCapability) model.Device { // GroupTopic1 read and fresh, 2 to 4 never read
		return model.Device{ID: 2, Name: "garage", Topic: "b", MQTT: m, GroupTopic: "tasmotas", GroupTopicReadAt: factsNow}
	}
	for _, tc := range []struct {
		name string
		x    model.Device
		h    model.Health
		want []string
	}{
		{"2 to 4 never read: counted", gt1Only(model.MQTTOurs), model.Health{LWT: "Online"}, []string{"garage"}},
		{"2 to 4 older than the last probe: counted", func() model.Device {
			x := allRead(model.Device{ID: 2, Name: "garage", Topic: "b", MQTT: model.MQTTOurs}, factsNow, "tasmotas")
			x.MoreGroupTopicsReadAt = factsNow.Add(-time.Hour)
			x.OTAURLReadAt = factsNow.Add(-time.Minute)
			return x
		}(), model.Health{LWT: "Online"}, []string{"garage"}},
		{"2 to 4 read and empty: known", allRead(model.Device{ID: 2, Name: "garage", Topic: "b", MQTT: model.MQTTOurs}, factsNow, "tasmotas"),
			model.Health{LWT: "Online"}, nil},
		{"2 to 4 never read, last will Offline: exempt", gt1Only(model.MQTTOurs), model.Health{LWT: "Offline"}, nil},
		{"2 to 4 never read, provably not on our broker: exempt", gt1Only(model.MQTTOtherBroker), model.Health{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := newFacts([]model.Device{desk, tc.x}, fakeFactsHealth{1: {LWT: "Online"}, 2: tc.h}, nil).Facts(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.MQTT.OthersGroupUnread, tc.want) {
				t.Errorf("held back by %v, want %v", f.MQTT.OthersGroupUnread, tc.want)
			}
			if wantMQTT := tc.want == nil; (transport.Choose(f).Writes == transport.MQTT) != wantMQTT {
				t.Errorf("writes = %s, want MQTT %v", transport.Choose(f).Writes, wantMQTT)
			}
		})
	}
	// the device's own GroupTopic2 to 4 unread: no MQTT write, but its reads stay on MQTT (the reader needs them)
	own := desk
	own.MoreGroupTopicsReadAt = time.Time{}
	f, err := newFacts([]model.Device{own}, fakeFactsHealth{1: {LWT: "Online"}}, nil).Facts(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := transport.Choose(f); p.Writes == transport.MQTT || p.Commands != transport.MQTT {
		t.Errorf("own 2 to 4 unread: writes %s, reads %s; want writes off MQTT and reads on it", p.Writes, p.Commands)
	}
	if !wantsGroupTopic(own) {
		t.Error("the reader must ask a device whose GroupTopic2 to 4 were never read")
	}
	if wantsGroupTopic(desk) {
		t.Error("the reader must leave a device alone whose four group topics are known")
	}
}

// Why (#1005): the reader stores all four group topics of one bare GroupTopic answer with one stamp, and nothing from
// an answer that does not hold all four usable keys: an older or foreign answer must never look like "no further group
// topics" (fail closed), and the device is asked again later.
func TestTheGroupTopicReaderStoresAllFourOrNothing(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		reply string
		want  []string // nil: nothing stored
	}{
		{"all four", `{"GroupTopic1":"tasmotas","GroupTopic2":"kitchen","GroupTopic3":"","GroupTopic4":"floor/1"}`, []string{"tasmotas", "kitchen", "floor/1"}},
		{"only GroupTopic1 set", `{"GroupTopic1":"tasmotas","GroupTopic2":"","GroupTopic3":"","GroupTopic4":""}`, []string{"tasmotas"}},
		{"a key missing", `{"GroupTopic1":"tasmotas","GroupTopic2":"kitchen","GroupTopic3":""}`, nil},
		{"the single-key form", `{"GroupTopic":"tasmotas"}`, nil},
		{"a wildcard in GroupTopic3", `{"GroupTopic1":"tasmotas","GroupTopic2":"","GroupTopic3":"a/#","GroupTopic4":""}`, nil},
		{"not a string", `{"GroupTopic1":"tasmotas","GroupTopic2":2,"GroupTopic3":"","GroupTopic4":""}`, nil},
		{"Command Unknown", `{"Command":"Unknown"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}}}
			c := &gtClient{reply: map[model.DeviceID]string{1: tc.reply}}
			r := newTestReader(st, c, &now, online)
			r.tick(t.Context())
			d, _ := st.GetDevice(t.Context(), 1)
			if tc.want == nil {
				if !d.GroupTopicReadAt.IsZero() || !d.MoreGroupTopicsReadAt.IsZero() || len(d.GroupTopics()) != 0 {
					t.Fatalf("stored %v at %v/%v from an incomplete answer", d.GroupTopics(), d.GroupTopicReadAt, d.MoreGroupTopicsReadAt)
				}
				if _, tried := r.tried[1]; !tried {
					t.Error("an incomplete answer must count as a failed read, so the device is asked again after the backoff")
				}
				return
			}
			if !slices.Equal(d.GroupTopics(), tc.want) || !d.GroupTopicReadAt.Equal(now) || !d.MoreGroupTopicsReadAt.Equal(now) {
				t.Fatalf("stored %v at %v/%v, want %v with one stamp", d.GroupTopics(), d.GroupTopicReadAt, d.MoreGroupTopicsReadAt, tc.want)
			}
			if groupTopicStale(d) {
				t.Error("a device the reader has read must not count as unread")
			}
		})
	}
}

// replyCmdService answers every command with reply and err, as a device that took a GroupTopic write does.
type replyCmdService struct {
	devicecmd.Service
	reply string
	err   error
}

func (f replyCmdService) Run(context.Context, model.Principal, model.DeviceID, string) (json.RawMessage, error) {
	return json.RawMessage(f.reply), f.err
}

// Why (#1005, review): TasmoFleet's own GroupTopic<n> write (the console, a bulk command and an update job all send
// through holdingCmd) may give a device another device's topic as GroupTopic2. INFO1 after the restart refreshes
// GroupTopic1 only, and the reply is not stored, so without a mark the stored list would stay "known" and desk's MQTT
// writes and transfers (an update job's OtaUrl and Upgrade too) would also run on lamp. The list must count as unread
// at once, without any probe, and hold desk back; a bare GroupTopic (a read) and a write that never left must not.
func TestTasmoFleetsOwnGroupTopicWriteMakesTheListUnread(t *testing.T) {
	const answer = `{"GroupTopic1":"tasmotas","GroupTopic2":"a","GroupTopic3":"","GroupTopic4":""}`
	for _, tc := range []struct {
		name string
		cmd  string
		err  error
		held bool
	}{
		{"GroupTopic2 set", "GroupTopic2 a", nil, true},
		{"inside a Backlog, not the first part", "Backlog Power1 ON; GroupTopic3 a", nil, true},
		{"reply lost: it may have run", "GroupTopic2 a", errors.New("timeout"), true},
		{"the bare read", "GroupTopic", nil, false},
		{"refused by the policy: never sent", "GroupTopic2 a", cmdpolicy.ErrForbidden, false},
		{"device busy: never sent", "GroupTopic2 a", devlock.BusyError{Holder: "ota"}, false},
		{"another command", "Power1 ON", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desk := allRead(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}, factsNow, "tasmotas")
			lamp := allRead(model.Device{ID: 2, Name: "lamp", Topic: "b", MQTT: model.MQTTOurs}, factsNow, "tasmotas")
			devs := []model.Device{desk, lamp}
			facts := newFacts(devs, fakeFactsHealth{1: {LWT: "Online"}, 2: {LWT: "Online"}}, nil)
			forget := func(id model.DeviceID) { // the store's ForgetGroupTopics on the facts' inventory
				for i := range devs {
					if devs[i].ID == id {
						devs[i].MoreGroupTopicsReadAt = time.Time{}
					}
				}
			}
			h := holdingCmd{Service: replyCmdService{reply: answer, err: tc.err}, holds: devlock.NewFirmwareHolds(time.Now, time.Minute), onGroupTopic: forget}
			_, _ = h.Run(t.Context(), model.Principal{}, 2, tc.cmd) // the outcome is in tc.err; the facts are what matter
			f, err := facts.Facts(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			held := slices.Contains(f.MQTT.OthersGroupUnread, "lamp")
			if held != tc.held {
				t.Errorf("lamp named as unread: %v, want %v (%v)", held, tc.held, f.MQTT.OthersGroupUnread)
			}
			if mqtt := transport.Choose(f).Writes == transport.MQTT; mqtt == tc.held {
				t.Errorf("desk's writes over MQTT: %v, want %v", mqtt, !tc.held)
			}
		})
	}
}
