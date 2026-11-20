package app

import (
	"context"
	"fmt"
	"time"

	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// httpFreshFor is how long a successful HTTP call counts as "HTTP works" (D2).
const httpFreshFor = 15 * time.Minute

// HTTPMemory is the router's memory of HTTP calls that failed (WP-A2 implements it): why and since
// when the last calls to a device failed, ok=false when they did not or it was cleared by a success.
type HTTPMemory interface {
	HTTPFailure(id model.DeviceID) (kind transport.HTTPFailKind, since time.Time, ok bool)
	// LastFailure is when the last failing call was made, also after HTTPFailure forgot it; ok=false when no call
	// failed since the last answer (#818).
	LastFailure(id model.DeviceID) (at time.Time, ok bool)
}

// topicMarks is the part of transport.TopicMarks the facts read.
type topicMarks interface {
	Since(id model.DeviceID, topic string) time.Time
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
	// Unanswered is the router's memory of devices that left a read over MQTT unanswered (the kind is
	// not used); nil: none.
	Unanswered HTTPMemory
	// ConnChanged is the router's memory of devices whose MQTT connection it just changed over MQTT while HTTP
	// answered (the kind is not used); nil: none.
	ConnChanged HTTPMemory
	// TopicTaken is the router's memory of devices on whose MQTT topic another device answered a MAC check (#852,
	// transport.TopicMarks); nil: none.
	TopicTaken topicMarks
	// BusConnected, FullTopic (MQTT_FULLTOPIC) and FirmwareBaseURL (FIRMWARE_BASE_URL) are read per
	// call: the settings page changes them live.
	BusConnected    func() bool
	FullTopic       func() string
	FirmwareBaseURL func() string
	// BusConnectedSince is since when the bus's subscriptions are in place (mqttbus.ConnectedSincer); zero while it is not
	// subscribed. nil: unknown, and "nothing heard" proves nothing (offOurBroker).
	BusConnectedSince func() time.Time
	// BusWills is what the bus heard of the last wills on its current connection (mqttbus.WillLedger, #972). nil:
	// unknown, and "nothing heard" proves nothing (offOurBroker).
	BusWills func() mqttbus.Wills
	// BrokerSame is liveBroker.Same: whether a device configured for host:port talks to our broker, broker aliases
	// included (#597). It only ever denies the exemption of offOurBroker; nil: no such evidence.
	BrokerSame func(host string, port int) bool
	// BrokerPassword is TasmoFleet's MQTT_PASSWORD; devices in broker mode send it.
	BrokerPassword func() string
	// CommandPrefix is the command prefix of the bus (mqttbus Options.Prefixes[0]); "" reads as cmnd.
	CommandPrefix func() string
	Now           func() time.Time
}

var (
	_ transport.FactsSource = (*TransportFacts)(nil)
	_ transport.DeviceFacts = (*TransportFacts)(nil) // what the router uses
)

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
	return s.FactsFor(ctx, d)
}

// HTTPState is the cheap part of the facts (the router's memory and the health service, no
// store): the router asks it before every call to decide whether HTTP is worth trying.
func (s *TransportFacts) HTTPState(id model.DeviceID) transport.HTTPState {
	return s.httpState(id, s.Health.Get(id), s.Now())
}

// FactsFor is Facts for a device snapshot the caller holds: the router decides and sends from
// the same snapshot.
func (s *TransportFacts) FactsFor(ctx context.Context, d model.Device) (transport.Facts, error) {
	all, err := s.Devices.ListDevices(ctx)
	if err != nil {
		return transport.Facts{}, fmt.Errorf("transport facts: %w", err)
	}
	return s.factsFor(ctx, d, all)
}

// factsFor is FactsFor with the inventory the caller already listed.
func (s *TransportFacts) factsFor(_ context.Context, d model.Device, all []model.Device) (transport.Facts, error) {
	id := d.ID
	h := s.Health.Get(id)
	now := s.Now()

	probe := lastProbe(d)
	var unanswered time.Time
	if s.Unanswered != nil {
		if _, since, ok := s.Unanswered.HTTPFailure(id); ok {
			unanswered = since
		}
	}
	var connChanged time.Time
	if s.ConnChanged != nil {
		if _, since, ok := s.ConnChanged.HTTPFailure(id); ok {
			connChanged = since
		}
	}
	var topicTaken time.Time
	if s.TopicTaken != nil && d.Topic != "" {
		topicTaken = s.TopicTaken.Since(id, d.Topic)
	}
	return transport.Facts{
		Now:  now,
		HTTP: s.httpState(id, h, now),
		MQTT: transport.MQTTFacts{
			BusConnected: s.BusConnected(), Ours: d.MQTT == model.MQTTOurs,
			Topic: d.Topic, TopicExclusive: topicExclusive(d, all, s.commandPrefix()),
			OthersGroupUnread: othersGroupUnread(d, all, s.Health.Get, s.brokerEvidence(now)),
			FullTopic:         d.FullTopic, OurFullTopic: s.FullTopic(),
			// GroupTopicReadAt is the stamp of all four group topics together (#1005): an unread GroupTopic2 to 4 makes
			// the device's own group topics unknown exactly like an unread GroupTopic1 (groupTopicStale's rule).
			GroupTopic: d.GroupTopic, GroupTopicReadAt: d.GroupTopicsReadAt(), LastProbeAt: probe,
			LWTOnline: h.LWT == "Online", ReadsUnansweredSince: unanswered, ConnChangedSince: connChanged,
			TopicTakenSince: topicTaken,
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

// httpState: a remembered failure wins unless a successful HTTP call came after it began; else the
// health tracker's finding that the HTTP API is off (Health.APIOff, #447), which outlives the
// memory's ten minutes, so the router, the page and the gates decide alike; else a success within
// 15 minutes is ok unless an HTTP call failed after it (the memory forgets the failure for routing after ten minutes,
// the time of it stays, #818); else unknown (which Choose treats as "try").
func (s *TransportFacts) httpState(id model.DeviceID, h model.Health, now time.Time) transport.HTTPState {
	if s.Memory != nil {
		if kind, since, ok := s.Memory.HTTPFailure(id); ok && !h.SeenHTTP.After(since) {
			return transport.HTTPState{State: transport.HTTPFailing, Kind: kind, Since: since}
		}
	}
	if h.APIOff() {
		return transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: h.HTTPRefusedSince}
	}
	if !h.SeenHTTP.IsZero() && now.Sub(h.SeenHTTP) < httpFreshFor {
		if s.Memory != nil {
			if last, ok := s.Memory.LastFailure(id); ok && last.After(h.SeenHTTP) {
				return transport.HTTPState{} // an answer from before a failed attempt: not checked lately, not "reachable"
			}
		}
		return transport.HTTPState{State: transport.HTTPOK}
	}
	return transport.HTTPState{}
}

// password: the password a transfer would send is model.FilePassword's. A device in broker mode sends
// MQTT_PASSWORD, so without one it has no usable password; a password the transfer command cannot carry
// (a quote, a backslash, a control character; one learned from a backup can hold them, #609) is unusable
// too, said before any transfer instead of as a late refusal. The value never leaves this function.
func (s *TransportFacts) password(d model.Device) transport.Password {
	p := transport.Password{Mode: d.MQTTPasswordMode, BadAt: d.MQTTPasswordBadAt}
	pw, ok := model.FilePassword(d, s.BrokerPassword())
	switch {
	case !ok && d.MQTTPasswordMode == model.MQTTPasswordBroker:
		p.Unusable = "TasmoFleet has no broker password configured"
	case ok && mqttcmd.CheckFilePassword(pw) != nil:
		p.Unusable = "the device's MQTT password contains a character that cannot be sent in a file transfer command (a quote, a backslash or a control character)"
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
// Topic, not as any of its group topics GroupTopic1 to GroupTopic4 (fresh or stale, d's own included, #1005). Stems
// are compared the way Tasmota subscribes (stem + "/#"): equal or one a level prefix of the other; a group topic also
// under SetOption75's stem, which ignores the FullTopic. d's own FullTopic must
// expand (contain %topic%, no other placeholder). Another device whose stem cannot be known (a
// FullTopic without %topic%, or with %hostname%, %id%, ...) makes the answer false: unknown is not
// exclusive. A device with neither Topic nor FullTopic is HTTP only and listens on nothing.
// Not seen: devices outside the inventory, and group topics never read (othersGroupUnread and the device's own
// GroupTopicsReadAt hold writes back for those). The rule itself is topicConflicts, which the Topic check card and the
// Identity card's hint show (#1047): one rule, so the view cannot drift from the gate.
func topicExclusive(d model.Device, all []model.Device, prefix string) bool {
	conflicts, ok := topicConflicts(d, all, prefix)
	return ok && len(conflicts) == 0
}

// topicConflicts is the write gate's topic rule with its reasons (#1047): every conflict that makes d's topic not
// exclusive, from the same loop and the same fail-closed cases topicExclusive used to stop at, in inventory order.
// ok is false when d has no Topic or its own FullTopic cannot be expanded: then nothing about d's topic is known and
// no conflict is listed. A pair is listed once per reason: an equal topic is not reported again as overlapping
// command topics, and a group topic's SetOption75 stem only when its FullTopic stem does not already overlap.
func topicConflicts(d model.Device, all []model.Device, prefix string) (conflicts []transport.TopicConflict, ok bool) {
	mine, ok := transport.CommandTopic(d.FullTopic, d.Topic, prefix)
	if d.Topic == "" || !ok {
		return nil, false
	}
	for _, o := range all {
		groups := o.GroupTopics()
		if o.Topic == "" && o.FullTopic == "" && len(groups) == 0 {
			continue
		}
		add := func(kind transport.TopicConflictKind, topic, stem, slot string) {
			conflicts = append(conflicts, transport.TopicConflict{Kind: kind, Topic: topic, Stem: stem, OtherID: o.ID, OtherName: o.Name, Slot: slot})
		}
		unknown := false // o's stem cannot be known: listed once, whatever else of o is checked
		addUnknown := func() {
			if !unknown {
				unknown = true
				add(transport.ConflictOtherStemUnknown, o.Topic, "", transport.SlotTopic)
			}
		}
		if o.ID != d.ID {
			same := o.Topic == d.Topic
			if same {
				add(transport.ConflictSameTopic, o.Topic, "", transport.SlotTopic)
			}
			x, ok := transport.CommandTopic(o.FullTopic, o.Topic, prefix)
			switch {
			case !ok:
				addUnknown()
			case !same && transport.StemsOverlap(x, mine):
				add(transport.ConflictStemOverlap, o.Topic, shorterStem(x, mine), transport.SlotTopic)
			}
		}
		for i, g := range groups {
			slot := transport.SlotGroupTopic2to4
			if i == 0 && o.GroupTopic != "" {
				slot = transport.SlotGroupTopic1
			}
			if g == d.Topic {
				kind := transport.ConflictGroupIsTopic
				if o.ID == d.ID {
					kind = transport.ConflictOwnGroupIsTopic
				}
				add(kind, g, "", slot)
				continue
			}
			x, ok := transport.CommandTopic(o.FullTopic, g, prefix)
			if !ok {
				addUnknown()
			} else if transport.StemsOverlap(x, mine) {
				add(transport.ConflictGroupStemOverlap, g, shorterStem(x, mine), slot)
				continue
			}
			// With SetOption75 1 Tasmota subscribes a group topic as <prefix>/<group>/#, whatever the FullTopic says
			// (GetGroupTopic_P). SetOption75 is not read, so that stem counts too, with our prefix and Tasmota's
			// default "cmnd" (it only adds refusals).
			for _, s := range []string{prefix + "/" + g, "cmnd/" + g} {
				if transport.StemsOverlap(s, mine) {
					add(transport.ConflictSetOption75, g, shorterStem(s, mine), slot)
					break
				}
			}
		}
	}
	return conflicts, true
}

// shorterStem is the stem of two overlapping ones that the other lies under (or equals).
func shorterStem(a, b string) string {
	if len(a) < len(b) {
		return a
	}
	return b
}

// lastProbe is the time of the device's last Status 1 read or identity detection.
func lastProbe(d model.Device) time.Time {
	probe := d.OTAURLReadAt // the Status 1 stamp
	if d.Identity.DetectedAt.After(probe) {
		probe = d.Identity.DetectedAt
	}
	return probe
}

// groupTopicStale says the device's group topics are not known: GroupTopic1 never read or empty, GroupTopic2 to
// GroupTopic4 never read (#1005: every device right after the upgrade), or either older than the device's last probe
// (model.Device.GroupTopicsReadAt is the older stamp). An empty GroupTopic2 to 4 list that was read is known: it is the
// normal case. The one rule for the device's own group topics (transport.groupTopicKnown, through the facts'
// GroupTopicReadAt), for the other devices' (othersGroupUnread) and for the background reader.
func groupTopicStale(d model.Device) bool {
	at := d.GroupTopicsReadAt()
	return d.GroupTopic == "" || at.IsZero() || at.Before(lastProbe(d))
}

// othersGroupUnread names the other devices with an MQTT topic whose group topics (any of GroupTopic1 to 4, #1005) are
// not known and that a message on our broker could reach. Their GroupTopic could be anything (Tasmota takes any text, slashes included), so any of
// them could listen on d's topic; that is why the strict rule holds d back, and why no such device can be left out by
// comparing topics (#812). Left out are only the devices no message on our broker can reach, whatever their GroupTopic:
//   - a device whose last will is Offline (the broker confirmed it disconnected, not merely "unknown"): our commands
//     are never published retained, so it cannot receive one; one dead device must not block MQTT writes for the
//     whole fleet;
//   - a device provably not on our broker (offOurBroker).
//
// Their topic and stale GroupTopic still count for TopicExclusive, which stays fail-closed. A device outside the
// inventory is not seen here either, as in topicExclusive.
func othersGroupUnread(d model.Device, all []model.Device, health func(model.DeviceID) model.Health, ev brokerEvidence) []string {
	var names []string
	for _, o := range all {
		if o.ID == d.ID || o.Topic == "" || !groupTopicStale(o) || !holdsWritesBack(o, all, health, ev) {
			continue
		}
		name := o.Name
		if name == "" {
			name = o.Topic
		}
		names = append(names, name)
	}
	return names
}

// holdsWritesBack says a message on our broker could reach o, so its unknown group topics hold back MQTT writes to the
// other devices: false when its last will is Offline or it is provably not on our broker (offOurBroker). The one rule
// for othersGroupUnread and the Topic check card (#1047).
func holdsWritesBack(o model.Device, all []model.Device, health func(model.DeviceID) model.Health, ev brokerEvidence) bool {
	return health(o.ID).LWT != "Offline" && !offOurBroker(o, all, health, ev)
}

// willReplayGrace is how long the bus's subscriptions must have been in place before "nothing heard from a topic"
// counts (offOurBroker). The broker replays the retained last wills right after the subscriptions, and the health
// tracker takes them in as they arrive; until then it knows nothing, at every start, after a reconnect and after a
// broker switch alike. 30 s covers that replay many times over.
const willReplayGrace = 30 * time.Second

// brokerEvidence is what offOurBroker weighs besides the inventory and the health tracker.
type brokerEvidence struct {
	fullTopic string        // MQTT_FULLTOPIC
	listening time.Duration // how long the bus's subscriptions have been in place; 0 when not subscribed or unknown
	same      func(host string, port int) bool
	wills     *mqttbus.Wills // what the bus heard of the wills on its current connection (#972); nil when it cannot say
}

func (s *TransportFacts) brokerEvidence(now time.Time) brokerEvidence {
	ev := brokerEvidence{fullTopic: s.FullTopic(), same: s.BrokerSame}
	if s.BusConnectedSince != nil {
		if since := s.BusConnectedSince(); !since.IsZero() && now.After(since) {
			ev.listening = now.Sub(since)
		}
	}
	if s.BusWills != nil {
		w := s.BusWills()
		ev.wills = &w
	}
	return ev
}

// willsAllKnown reports that silence can mean something on the bus's current connection (#972): it heard wills (reads
// are not denied), kept all of them, and every topic that is up (a will that is not Offline, or a live telemetry or
// reply message; only an Offline will takes it down, a cleared will keeps its state) is the topic of a stored device
// (no device outside the inventory is on our broker, so none of the exempt ones moved here under a new topic). False
// when w is nil. A topic whose latest will is Offline is not up and does not count: Tasmota leaves a retained Offline on its old topic at
// every topic change (#119), and an Offline device cannot receive a command (ours are never retained), the rule
// othersGroupUnread applies to stored devices too.
func willsAllKnown(w *mqttbus.Wills, all []model.Device) bool {
	if w == nil || !w.Heard || !w.Complete {
		return false
	}
	stored := make(map[string]struct{}, len(all))
	for _, d := range all {
		if d.Topic != "" {
			stored[d.Topic] = struct{}{}
		}
	}
	for _, t := range w.Up {
		if _, ok := stored[t]; !ok {
			return false
		}
	}
	return true
}

// offOurBroker reports that o is provably not on TasmoFleet's broker (#812). All of these must hold:
//   - its last Status 6 put it on another broker or on none (model.MQTTOtherBroker, model.MQTTNone; not unknown,
//     not ours). Status 6 can be stale, and it is compared without broker aliases (#597), so this alone is not enough;
//   - nothing TasmoFleet knows ties its configured broker to ours: no device stored as ours names the same host:port
//     (mqttbus.HostKey; the inventory, so it holds from the first second after a start), and liveBroker.Same (the
//     matcher and the broker aliases) does not accept it. A device configured with an alias of our broker is on it
//     however little is heard from it;
//   - nothing from its topic was ever heard on our broker: no last will (Tasmota publishes "Online" retained on every
//     connect, and the broker replays it to the bus) and no other message, under any device that has the topic (the
//     health tracker files a topic under one of them). That counts only where its will would be heard at all: its
//     FullTopic is TasmoFleet's (transport.SameFullTopic, the rule of Choose) and its topic is one level
//     (transport.TopicShapeOK), the topics the bus decodes; and only once the bus has been subscribed for
//     willReplayGrace, so the replay of the retained wills has arrived;
//   - silence means something on the current connection (#972, willsAllKnown): the bus heard at least one will since
//     it subscribed (a broker ACL that lets TasmoFleet publish but not read hears none, and every device would look
//     "never heard"), and every topic that is up (a will that is not Offline, or a live telemetry or reply message;
//     only an Offline will takes it down, a cleared will keeps its state) is the topic of a stored device (a device
//     that moved to our broker with a new topic is heard only there, and could be this one).
//
// A device that does connect to our broker with its stored topic is heard at once and stops being exempt from the next
// call on: the router computes the facts afresh for every call it routes.
func offOurBroker(o model.Device, all []model.Device, health func(model.DeviceID) model.Health, ev brokerEvidence) bool {
	if o.MQTT != model.MQTTOtherBroker && o.MQTT != model.MQTTNone {
		return false
	}
	if !transport.SameFullTopic(o.FullTopic, ev.fullTopic) || !transport.TopicShapeOK(o.Topic) || ev.listening < willReplayGrace ||
		!willsAllKnown(ev.wills, all) {
		return false
	}
	host := ""
	if o.MQTTHost != "" {
		host = mqttbus.HostKey(o.MQTTHost, o.MQTTPort)
	}
	for _, x := range all {
		if host != "" && x.MQTT == model.MQTTOurs && x.MQTTHost != "" && mqttbus.HostKey(x.MQTTHost, x.MQTTPort) == host {
			return false
		}
		if x.Topic != o.Topic {
			continue
		}
		if h := health(x.ID); h.LWT != "" || !h.SeenMQTT.IsZero() {
			return false
		}
	}
	return host == "" || ev.same == nil || !ev.same(o.MQTTHost, o.MQTTPort)
}
