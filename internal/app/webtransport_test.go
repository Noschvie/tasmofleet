package app

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// countingDevices counts the reads that cost a database round trip.
type countingDevices struct {
	fakeFactsDevices
	gets, lists int
}

func (c *countingDevices) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	c.gets++
	return c.fakeFactsDevices.GetDevice(ctx, id)
}

func (c *countingDevices) ListDevices(ctx context.Context) ([]model.Device, error) {
	c.lists++
	return c.fakeFactsDevices.ListDevices(ctx)
}

type memoryFor map[model.DeviceID]bool

func (m memoryFor) HTTPFailure(id model.DeviceID) (transport.HTTPFailKind, time.Time, bool) {
	return transport.HTTPFailUnreachable, factsNow.Add(-time.Hour), m[id]
}

func (memoryFor) LastFailure(model.DeviceID) (time.Time, bool) { return time.Time{}, false }

// Why (review H2): the device list asks for the MQTT marker on every render; with 500 devices a
// Facts read each (GetDevice plus a full ListDevices) cost seconds. Facts are read only for a device
// whose HTTP is failing, and a fleet without failures (or without the router's memory) reads nothing.
func TestFailingHTTPReadsOnlyFailingDevices(t *testing.T) {
	var devs []model.Device
	for i := 1; i <= 200; i++ {
		devs = append(devs, model.Device{ID: model.DeviceID(i), Topic: "t" + strconv.Itoa(i), MQTT: model.MQTTOurs})
	}
	ids := make([]model.DeviceID, len(devs))
	for i, d := range devs {
		ids[i] = d.ID
	}
	h := fakeFactsHealth{}

	count := &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f := newFacts(devs, h, nil)
	f.Devices = count
	if m, err := (webFacts{f}).FailingHTTP(t.Context(), ids); err != nil || len(m) != 0 || count.gets+count.lists != 0 {
		t.Fatalf("no memory: %d facts, %d gets, %d lists, err %v", len(m), count.gets, count.lists, err)
	}

	count = &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f = newFacts(devs, h, memoryFor{}) // memory, no failure
	f.Devices = count
	if m, _ := (webFacts{f}).FailingHTTP(t.Context(), ids); len(m) != 0 || count.gets+count.lists != 0 {
		t.Fatalf("no failure: %d facts, %d gets, %d lists", len(m), count.gets, count.lists)
	}

	count = &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f = newFacts(devs, h, memoryFor{7: true})
	f.Devices = count
	m, err := (webFacts{f}).FailingHTTP(t.Context(), ids)
	if err != nil || len(m) != 1 || m[7].HTTP.State != transport.HTTPFailing {
		t.Fatalf("one failure: %v, err %v", m, err)
	}
	if count.gets != 0 || count.lists != 1 {
		t.Fatalf("failing devices must cost one ListDevices in all, got %d gets and %d lists", count.gets, count.lists)
	}
}

// Why (#1151): the device page needs the router's facts of a device and the topic conflicts of its Identity hint. It
// listed the inventory once for each; DeviceFacts is one list for both. The conflicts must be the Topic check card's own
// for that device (the hint and the card cannot drift), and the facts what FactsFor says (the Paths card shows what the
// router decides). The fleets are those of TestTopicCheckFollowsTheCollisionTable plus the unknown-device cases.
func TestDeviceFactsReadTheInventoryOnceAndAgreeWithTheCard(t *testing.T) {
	fleets := map[string][]model.Device{
		"shared tasmotas": {read(model.Device{ID: 1, Name: "kueche", Topic: "kueche"}), read(model.Device{ID: 2, Name: "flur", Topic: "flur"})},
		"group topic is a topic": {read(model.Device{ID: 1, Name: "kueche", Topic: "kueche"}),
			read(model.Device{ID: 2, Name: "flur", Topic: "flur", MoreGroupTopics: []string{"kueche"}})},
		"equal topics": {read(model.Device{ID: 1, Name: "one", Topic: "lamp"}), read(model.Device{ID: 2, Name: "two", Topic: "lamp"})},
		"default group topic": {read(model.Device{ID: 1, Name: "odd", Topic: transport.DefaultGroupTopic, GroupTopic: "g"}),
			read(model.Device{ID: 2, Name: "plug", Topic: "plug"})},
		"a device that listens on every command": {read(model.Device{ID: 1, Name: "a", Topic: "a"}), read(model.Device{ID: 2, Name: "b", Topic: "b"}),
			{ID: 3, Name: "bare", FullTopic: "%prefix%/%topic%/"}},
		"overlapping command topics": {read(model.Device{ID: 1, Name: "kitchen", Topic: "kitchen"}),
			read(model.Device{ID: 2, Name: "lamp", Topic: "lamp", FullTopic: "%prefix%/kitchen/%topic%/"})},
		"SetOption75 stem": {read(model.Device{ID: 1, Name: "kitchen", Topic: "kitchen"}),
			read(model.Device{ID: 2, Name: "g", Topic: "b", FullTopic: "x/%prefix%/%topic%", MoreGroupTopics: []string{"kitchen/sub"}})},
		"unread group topics": {read(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}),
			{ID: 2, Name: "garage", Topic: "b", GroupTopic: "tasmotas", GroupTopicReadAt: factsNow}},
		"a FullTopic that cannot be expanded": {read(model.Device{ID: 1, Name: "desk", Topic: "a", MQTT: model.MQTTOurs}),
			read(model.Device{ID: 2, Name: "host", Topic: "b", FullTopic: "%prefix%/%hostname%/%topic%/"})},
	}
	for name, devs := range fleets {
		t.Run(name, func(t *testing.T) {
			count := &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
			f := newFacts(devs, fakeFactsHealth{2: {LWT: "Online"}}, nil)
			f.Devices = count
			w := webFacts{f}
			card, err := w.TopicCheck(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range devs {
				want, err := f.FactsFor(t.Context(), d)
				if err != nil {
					t.Fatal(err)
				}
				*count = countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
				got, conflicts, err := w.DeviceFacts(t.Context(), d)
				if err != nil {
					t.Fatalf("device %d: %v", d.ID, err)
				}
				if count.lists != 1 || count.gets != 0 {
					t.Errorf("device %d: lists = %d, gets = %d; want one list", d.ID, count.lists, count.gets)
				}
				if !slices.Equal(conflicts, card.Of(d.ID)) {
					t.Errorf("device %d: conflicts = %+v, the Topic check card says %+v", d.ID, conflicts, card.Of(d.ID))
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("device %d: facts differ from FactsFor:\n got %+v\nwant %+v", d.ID, got, want)
				}
			}
		})
	}
}

// Why (#1151, fail closed): a device page that cannot read the inventory must get an error, never "no conflicts" (the
// hint would read as "no collision") or facts of a device the inventory does not have any more.
func TestDeviceFactsFailClosed(t *testing.T) {
	d := read(model.Device{ID: 1, Name: "kueche", Topic: "kueche"})
	t.Run("the inventory cannot be read", func(t *testing.T) {
		f := newFacts([]model.Device{d}, nil, nil)
		f.Devices = failingList{}
		if _, conflicts, err := (webFacts{f}).DeviceFacts(t.Context(), d); err == nil || conflicts != nil {
			t.Errorf("conflicts = %+v, err = %v; want the error and no conflicts", conflicts, err)
		}
	})
	t.Run("the device is not in the inventory any more", func(t *testing.T) {
		f := newFacts([]model.Device{read(model.Device{ID: 2, Name: "flur", Topic: "flur"})}, nil, nil)
		if _, conflicts, err := (webFacts{f}).DeviceFacts(t.Context(), d); err == nil || conflicts != nil {
			t.Errorf("conflicts = %+v, err = %v; want the error and no conflicts", conflicts, err)
		}
	})
}
