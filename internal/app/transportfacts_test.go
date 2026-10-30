package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type fakeFactsDevices struct{ devs []model.Device }

func (f fakeFactsDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	for _, d := range f.devs {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, errors.New("not found")
}
func (f fakeFactsDevices) ListDevices(context.Context) ([]model.Device, error) { return f.devs, nil }

type fakeFactsHealth map[model.DeviceID]model.Health

func (f fakeFactsHealth) Get(id model.DeviceID) model.Health { return f[id] }

type fakeMemory struct {
	kind  transport.HTTPFailKind
	since time.Time
	ok    bool
}

func (m fakeMemory) HTTPFailure(model.DeviceID) (transport.HTTPFailKind, time.Time, bool) {
	return m.kind, m.since, m.ok
}

var factsNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func newFacts(devs []model.Device, h fakeFactsHealth, mem HTTPMemory) *TransportFacts {
	return &TransportFacts{
		Devices: fakeFactsDevices{devs}, Health: h, Memory: mem,
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" },
		FirmwareBaseURL: func() string { return "http://fw" }, Now: func() time.Time { return factsNow },
	}
}

// Why: a topic that two inventory devices share must reach Choose as non-unique, or a transfer to it
// would hit the wrong device; the device itself and an empty topic must not count.
func TestFactsTopicUniqueness(t *testing.T) {
	devs := []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "a"}, {ID: 3, Topic: "b"}, {ID: 4}, {ID: 5}}
	s := newFacts(devs, fakeFactsHealth{}, nil)
	for id, want := range map[model.DeviceID]bool{1: false, 2: false, 3: true} {
		f, err := s.Facts(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if f.MQTT.TopicUnique != want {
			t.Errorf("device %d: unique = %v", id, f.MQTT.TopicUnique)
		}
	}
	// no topic: not "unique", Choose refuses on the empty topic anyway
	if f, _ := s.Facts(context.Background(), 4); f.MQTT.TopicUnique {
		t.Error("empty topic must not be unique")
	}
	if _, err := s.Facts(context.Background(), 99); err == nil {
		t.Error("unknown device must fail")
	}
}

func TestFactsHTTPState(t *testing.T) {
	d := []model.Device{{ID: 1}}
	since := factsNow.Add(-time.Hour)
	cases := []struct {
		name string
		h    model.Health
		mem  HTTPMemory
		want transport.HTTPState
	}{
		{"nothing known", model.Health{}, nil, transport.HTTPState{}},
		{"recent success", model.Health{SeenHTTP: factsNow.Add(-14 * time.Minute)}, nil, transport.HTTPState{State: transport.HTTPOK}},
		// Why: after 15 minutes without a success HTTP is unknown, not ok.
		{"old success", model.Health{SeenHTTP: factsNow.Add(-15 * time.Minute)}, nil, transport.HTTPState{}},
		{"failure remembered", model.Health{}, fakeMemory{transport.HTTPFailAuth, since, true},
			transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAuth, Since: since}},
		// Why: a success after the failure began means the memory is stale; do not route around a working HTTP.
		{"success after failure", model.Health{SeenHTTP: factsNow.Add(-time.Minute)}, fakeMemory{transport.HTTPFailUnreachable, since, true},
			transport.HTTPState{State: transport.HTTPOK}},
		{"memory without failure", model.Health{}, fakeMemory{}, transport.HTTPState{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := newFacts(d, fakeFactsHealth{1: c.h}, c.mem).Facts(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if f.HTTP != c.want {
				t.Errorf("HTTP = %+v, want %+v", f.HTTP, c.want)
			}
		})
	}
}

// Why: Choose can only judge a GroupTopic stale against the last Status 1 read; the Status 1 stamp
// (OTAURLReadAt) and an HTTP probe's DetectedAt both are such reads, the later one counts.
func TestFactsLastProbeAndMQTT(t *testing.T) {
	stamp := factsNow.Add(-2 * time.Hour)
	det := factsNow.Add(-time.Hour)
	d := model.Device{ID: 1, Topic: "t", MQTT: model.MQTTOurs, FullTopic: "x/%topic%/", GroupTopic: "g", GroupTopicReadAt: stamp,
		OTAURLReadAt: stamp, Identity: model.Identity{DetectedAt: det}, MQTTPasswordMode: model.MQTTPasswordOwn,
		FWReach: model.FWReach{State: model.FWReachNo, At: stamp}}
	f, err := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, nil).Facts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	m := f.MQTT
	if !m.LastProbeAt.Equal(det) || !m.LWTOnline || !m.Ours || m.GroupTopic != "g" || m.FullTopic != "x/%topic%/" || m.OurFullTopic != "%prefix%/%topic%/" || !m.BusConnected {
		t.Errorf("mqtt facts: %+v", m)
	}
	if f.Password.Mode != model.MQTTPasswordOwn || f.FWReach.State != model.FWReachNo || !f.FirmwareBaseURL || !f.Now.Equal(factsNow) {
		t.Errorf("facts: %+v", f)
	}
	d.OTAURLReadAt = factsNow // Status 1 newer than the identity
	f, _ = newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Offline"}}, nil).Facts(context.Background(), 1)
	if !f.MQTT.LastProbeAt.Equal(factsNow) || f.MQTT.LWTOnline {
		t.Errorf("mqtt facts: %+v", f.MQTT)
	}
	s := newFacts([]model.Device{d}, fakeFactsHealth{}, nil)
	s.FirmwareBaseURL = func() string { return "" }
	if f, _ = s.Facts(context.Background(), 1); f.FirmwareBaseURL {
		t.Error("empty FIRMWARE_BASE_URL must read as not configured")
	}
}

// Why: the fit comes from fwselect.MQTTUploadFits with the real identity, so a layout or free-space
// problem reaches Choose as data and the upload is refused before anything is sent (D19).
func TestForUpdateFitsFromIdentity(t *testing.T) {
	d := model.Device{ID: 1, Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", FreeKB: 2000}}
	s := newFacts([]model.Device{d}, fakeFactsHealth{}, nil)
	f, err := s.ForUpdate(context.Background(), 1, UpdateRequest{Delivery: model.DeliveryPush, AllowMQTTUpload: true, ImageSize: 1 << 20,
		UpLayout: fwselect.UpLayoutUnknown, HTTPPush: transport.PushHTTP, HTTPPushNote: "n"})
	if err != nil {
		t.Fatal(err)
	}
	r := f.Request
	if !r.MQTTUploadFit.OK || r.MQTTUploadFit.NeedKB != 1024 || r.MQTTUploadFit.RoomKB != 2000-192 || r.Delivery != model.DeliveryPush || !r.AllowMQTTUpload || r.HTTPPush != transport.PushHTTP || r.HTTPPushNote != "n" {
		t.Errorf("request: %+v", r)
	}
	f, _ = s.ForUpdate(context.Background(), 1, UpdateRequest{ImageSize: 1 << 20, UpLayout: fwselect.UpLayoutSafeboot})
	if f.Request.MQTTUploadFit.OK || f.Request.MQTTUploadFit.Reason == "" {
		t.Errorf("safeboot layout must be refused with a reason: %+v", f.Request.MQTTUploadFit)
	}
	if _, err := s.ForUpdate(context.Background(), 9, UpdateRequest{}); err == nil {
		t.Error("unknown device must fail")
	}
}
