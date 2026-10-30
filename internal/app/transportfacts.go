package app

import (
	"context"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// httpFreshFor is how long a successful HTTP call counts as "HTTP works" (D2).
const httpFreshFor = 15 * time.Minute

// HTTPMemory is the router's memory of HTTP calls that failed (WP-A2 implements it): why and since
// when the last calls to a device failed, ok=false when they did not or it was cleared by a success.
type HTTPMemory interface {
	HTTPFailure(id model.DeviceID) (kind transport.HTTPFailKind, since time.Time, ok bool)
}

type factsDevices interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	ListDevices(ctx context.Context) ([]model.Device, error)
}

type factsHealth interface {
	Get(id model.DeviceID) model.Health
}

// TransportFacts gathers the transport.Facts of a device from the inventory, health, the router's
// HTTP-failure memory, the bus and the live settings. It only reads.
type TransportFacts struct {
	Devices factsDevices
	Health  factsHealth
	Memory  HTTPMemory // nil: no failure is ever known
	// BusConnected, FullTopic (MQTT_FULLTOPIC) and FirmwareBaseURL (FIRMWARE_BASE_URL) are read per
	// call: the settings page changes them live.
	BusConnected    func() bool
	FullTopic       func() string
	FirmwareBaseURL func() string
	// BrokerPassword is TasmoFleet's MQTT_PASSWORD; devices in broker mode send it.
	BrokerPassword func() string
	// CommandPrefix is the command prefix of the bus (mqttbus Options.Prefixes[0]); "" reads as cmnd.
	CommandPrefix func() string
	Now           func() time.Time
}

var _ transport.FactsSource = (*TransportFacts)(nil)

// UpdateRequest is what a person asked for when an update is planned.
type UpdateRequest struct {
	Delivery        model.Delivery
	AllowMQTTUpload bool
	ImageSize       int64 // the image the update would send, for the upload fit
	UpLayout        fwselect.UpLayoutHint
	// HTTPPush and HTTPPushNote are the OTA engine's decision for a device HTTP reaches (see
	// transport.Request).
	HTTPPush     transport.Path
	HTTPPushNote string
}

// Facts implements transport.FactsSource; the Request part stays empty.
func (s *TransportFacts) Facts(ctx context.Context, id model.DeviceID) (transport.Facts, error) {
	d, err := s.Devices.GetDevice(ctx, id)
	if err != nil {
		return transport.Facts{}, fmt.Errorf("transport facts: %w", err)
	}
	all, err := s.Devices.ListDevices(ctx)
	if err != nil {
		return transport.Facts{}, fmt.Errorf("transport facts: %w", err)
	}
	h := s.Health.Get(id)
	now := s.Now()

	probe := d.OTAURLReadAt // the Status 1 stamp
	if d.Identity.DetectedAt.After(probe) {
		probe = d.Identity.DetectedAt
	}
	return transport.Facts{
		Now:  now,
		HTTP: s.httpState(id, h, now),
		MQTT: transport.MQTTFacts{
			BusConnected: s.BusConnected(), Ours: d.MQTT == model.MQTTOurs,
			Topic: d.Topic, TopicExclusive: topicExclusive(d, all, s.commandPrefix()),
			OthersGroupUnread: othersGroupUnread(d, all),
			FullTopic:         d.FullTopic, OurFullTopic: s.FullTopic(),
			GroupTopic: d.GroupTopic, GroupTopicReadAt: d.GroupTopicReadAt, LastProbeAt: probe,
			LWTOnline: h.LWT == "Online",
		},
		Password:        s.password(d),
		FWReach:         d.FWReach,
		FirmwareBaseURL: s.FirmwareBaseURL() != "",
		Identity:        d.Identity,
	}, nil
}

// ForUpdate is Facts plus the request of an update: the upload fit is computed here, from the
// image size, so Choose stays pure.
func (s *TransportFacts) ForUpdate(ctx context.Context, id model.DeviceID, r UpdateRequest) (transport.Facts, error) {
	f, err := s.Facts(ctx, id)
	if err != nil {
		return f, err
	}
	ok, need, room, why := fwselect.MQTTUploadFits(f.Identity, r.UpLayout, r.ImageSize)
	f.Request = transport.Request{
		Delivery: r.Delivery, AllowMQTTUpload: r.AllowMQTTUpload,
		MQTTUploadFit: transport.MQTTFit{OK: ok, NeedKB: need, RoomKB: room, Reason: why},
		HTTPPush:      r.HTTPPush, HTTPPushNote: r.HTTPPushNote,
	}
	return f, nil
}

// httpState: a remembered failure wins unless a successful HTTP call came after it began; else a
// success within 15 minutes is ok; else unknown (which Choose treats as "try").
func (s *TransportFacts) httpState(id model.DeviceID, h model.Health, now time.Time) transport.HTTPState {
	if s.Memory != nil {
		if kind, since, ok := s.Memory.HTTPFailure(id); ok && !h.SeenHTTP.After(since) {
			return transport.HTTPState{State: transport.HTTPFailing, Kind: kind, Since: since}
		}
	}
	if !h.SeenHTTP.IsZero() && now.Sub(h.SeenHTTP) < httpFreshFor {
		return transport.HTTPState{State: transport.HTTPOK}
	}
	return transport.HTTPState{}
}

// password: a device in broker mode sends MQTT_PASSWORD, so without one it has no usable password.
func (s *TransportFacts) password(d model.Device) transport.Password {
	p := transport.Password{Mode: d.MQTTPasswordMode, BadAt: d.MQTTPasswordBadAt}
	if d.MQTTPasswordMode == model.MQTTPasswordBroker && s.BrokerPassword() == "" {
		p.Unusable = "TasmoFleet has no broker password configured"
	}
	return p
}

func (s *TransportFacts) commandPrefix() string {
	if s.CommandPrefix != nil {
		if p := s.CommandPrefix(); p != "" {
			return p
		}
	}
	return "cmnd"
}

// topicExclusive reports whether no device in the inventory listens on d's command stem: not as its
// Topic, not as its GroupTopic (fresh or stale, d's own included). Stems are compared the way
// Tasmota subscribes (stem + "/#"): equal or one a level prefix of the other. d's own FullTopic must
// expand (contain %topic%, no other placeholder). Another device whose stem cannot be known (a
// FullTopic without %topic%, or with %hostname%, %id%, ...) makes the answer false: unknown is not
// exclusive. A device with neither Topic nor FullTopic is HTTP only and listens on nothing.
// Not seen: GroupTopic2-4 (only GroupTopic is stored) and devices outside the inventory.
func topicExclusive(d model.Device, all []model.Device, prefix string) bool {
	mine, ok := transport.CommandTopic(d.FullTopic, d.Topic, prefix)
	if d.Topic == "" || !ok {
		return false
	}
	for _, o := range all {
		if o.Topic == "" && o.FullTopic == "" && o.GroupTopic == "" {
			continue
		}
		if o.ID != d.ID {
			if o.Topic == d.Topic {
				return false
			}
			x, ok := transport.CommandTopic(o.FullTopic, o.Topic, prefix)
			if !ok || transport.StemsOverlap(x, mine) {
				return false
			}
		}
		if o.GroupTopic != "" {
			if o.GroupTopic == d.Topic {
				return false
			}
			x, ok := transport.CommandTopic(o.FullTopic, o.GroupTopic, prefix)
			if !ok || transport.StemsOverlap(x, mine) {
				return false
			}
		}
	}
	return true
}

// othersGroupUnread counts the other devices with an MQTT topic whose GroupTopic was never read.
func othersGroupUnread(d model.Device, all []model.Device) int {
	n := 0
	for _, o := range all {
		if o.ID != d.ID && o.Topic != "" && (o.GroupTopic == "" || o.GroupTopicReadAt.IsZero()) {
			n++
		}
	}
	return n
}
