package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Identity over MQTT (#87, first step of #79): a device on our broker whose firmware version
// HTTP cannot provide (unreachable, refused, never probed) gets it from MQTT.
//
//   - INFO1, which every device publishes at boot, carries the version: it is read with the
//     Status 2 grammar (fwselect.DetectInfo1). It has no hardware, so on its own it never makes
//     the identity Known().
//   - Status 2 is asked once over MQTT (cmnd/<topic>/STATUS 2) for the hardware. Never Status 0,
//     one command at a time per device, once per device per 10 minutes, at most one per second
//     on the whole bus, only for devices that are online, and audited like discovery's Status 5.
//
// The HTTP probe stays the primary path. MQTT only fills a device that no probe has identified
// (DetectedAt zero; the store refuses the write otherwise) and never sets DetectedAt, so the
// first HTTP probe still runs and replaces whatever MQTT said. The OTA engine probes over HTTP
// before it plans, and Guard runs on that identity, unchanged.
const (
	mqttIDGap     = time.Second      // between two Status 2 questions on the whole bus
	mqttIDEvery   = 10 * time.Minute // between two questions to one device
	mqttIDGrace   = 30 * time.Second // how long a device must be online before it is asked: HTTP goes first
	mqttIDWindow  = 30 * time.Second // how long after the question a STATUS2 reply is taken
	mqttIDQueue   = 64               // INFO1 and STATUS2 messages waiting for the worker; more are dropped
	mqttIDTick    = time.Second
	mqttIDTimeout = 10 * time.Second
)

// identityMQTTStore is the part of the store the identity reader writes through.
type identityMQTTStore interface {
	UpdateIdentityFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic string, i model.Identity) error
}

type mqttIDTopic struct {
	lwt  string    // "Online", "Offline" or "" (none seen)
	live bool      // a non-retained message arrived since the last Offline
	up   time.Time // since when it is online (zero while offline)
}

type mqttIDPending struct {
	topic string
	at    time.Time
}

type mqttIdentity struct {
	bus      mqttbus.Bus
	list     func(context.Context) ([]model.Device, error)
	store    identityMQTTStore
	audit    audit.Recorder
	announce func(model.DeviceID)
	now      func() time.Time
	log      *slog.Logger
	grace    time.Duration

	msgs chan mqttbus.Message

	mu       sync.Mutex
	topics   map[string]*mqttIDTopic
	lastAsk  time.Time
	asked    map[model.DeviceID]time.Time
	pending  map[model.DeviceID]mqttIDPending
	answered map[model.DeviceID]bool // a Status 2 reply was read; not asked again until restart
}

func newMQTTIdentity(bus mqttbus.Bus, list func(context.Context) ([]model.Device, error), st identityMQTTStore,
	rec audit.Recorder, announce func(model.DeviceID), log *slog.Logger) *mqttIdentity {
	return &mqttIdentity{bus: bus, list: list, store: st, audit: rec, announce: announce, now: time.Now, log: log, grace: mqttIDGrace,
		msgs: make(chan mqttbus.Message, mqttIDQueue), topics: map[string]*mqttIDTopic{},
		asked: map[model.DeviceID]time.Time{}, pending: map[model.DeviceID]mqttIDPending{}, answered: map[model.DeviceID]bool{}}
}

// run reads the bus until ctx ends.
func (m *mqttIdentity) run(ctx context.Context) {
	unsub := m.bus.Subscribe([]mqttbus.Kind{mqttbus.KindLWT, mqttbus.KindState, mqttbus.KindInfo, mqttbus.KindStatus}, m.onMessage)
	defer unsub()
	tick := time.NewTicker(mqttIDTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.msgs:
			m.handle(ctx, msg)
		case <-tick.C:
			m.askDue(ctx)
		}
	}
}

// onMessage runs on the bus goroutine: it only keeps the online state and queues the two
// message types worth reading.
func (m *mqttIdentity) onMessage(msg mqttbus.Message) {
	now := m.now()
	m.mu.Lock()
	t := m.topics[msg.Topic]
	if t == nil {
		if len(m.topics) >= 1024 {
			m.mu.Unlock()
			return // the same bound as discovery's topic table; known devices are few
		}
		t = &mqttIDTopic{}
		m.topics[msg.Topic] = t
	}
	switch msg.Kind {
	case mqttbus.KindLWT:
		switch string(msg.Payload) {
		case "Online":
			t.lwt = "Online"
		case "Offline":
			t.lwt, t.live = "Offline", false
		}
	case mqttbus.KindState, mqttbus.KindInfo, mqttbus.KindStatus:
		if !msg.Retained {
			t.live = true // a retained telemetry message says nothing about now
		}
	case mqttbus.KindSensor, mqttbus.KindResult, mqttbus.KindPower, mqttbus.KindDiscovery, mqttbus.KindUpgrade:
		// not subscribed
	}
	online := t.live || t.lwt == "Online"
	switch {
	case !online:
		t.up = time.Time{}
	case t.up.IsZero():
		t.up = now
	}
	m.mu.Unlock()
	if (msg.Kind == mqttbus.KindInfo && msg.Suffix == "INFO1") || (msg.Kind == mqttbus.KindStatus && msg.Suffix == "STATUS2") {
		select {
		case m.msgs <- msg:
		default:
			m.log.Debug("mqtt identity: queue full, message dropped", "topic", msg.Topic, "suffix", msg.Suffix)
		}
	}
}

// deviceOf returns the inventory device that owns topic.
func (m *mqttIdentity) deviceOf(ctx context.Context, topic string) (model.Device, bool) {
	devs, err := m.list(ctx)
	if err != nil {
		m.log.Warn("mqtt identity: list devices", "err", err)
		return model.Device{}, false
	}
	for _, d := range devs {
		if d.Topic == topic {
			return d, true
		}
	}
	return model.Device{}, false
}

func (m *mqttIdentity) handle(ctx context.Context, msg mqttbus.Message) {
	d, ok := m.deviceOf(ctx, msg.Topic)
	if !ok || !d.Identity.DetectedAt.IsZero() {
		return // not ours, or HTTP has identified it: that identity is the one we keep
	}
	var next model.Identity
	var err error
	via := "INFO1"
	if msg.Suffix == "STATUS2" {
		via = "Status 2"
		m.mu.Lock()
		p, asked := m.pending[d.ID]
		if asked && p.topic == msg.Topic {
			delete(m.pending, d.ID)
		}
		m.mu.Unlock()
		if !asked || p.topic != msg.Topic || m.now().Sub(p.at) > mqttIDWindow {
			return // only a timely reply to our own question (a reply anyone can publish is no source)
		}
		if next, err = fwselect.Detect(fwselect.Inputs{Status2: msg.Payload}); err == nil {
			next.Language = d.Identity.Language
			m.mu.Lock()
			m.answered[d.ID] = true
			m.mu.Unlock()
		}
	} else {
		next, err = fwselect.DetectInfo1(msg.Payload, d.Identity)
	}
	if err != nil {
		m.log.Debug("mqtt identity: message not usable", "device", d.ID, "via", via, "err", err)
		return
	}
	cur := d.Identity
	if next.Family == cur.Family && next.Hardware == cur.Hardware && next.SingleCore == cur.SingleCore &&
		next.Version == cur.Version && next.Image == cur.Image && next.Variant == cur.Variant {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, mqttIDTimeout)
	defer cancel()
	err = m.store.UpdateIdentityFromMQTT(sctx, d.ID, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, d.Topic, next)
	switch {
	case errors.Is(err, store.ErrConflict):
		m.log.Debug("mqtt identity: device changed or was identified over HTTP meanwhile", "device", d.ID)
	case err != nil:
		m.log.Warn("mqtt identity: could not store the identity", "device", d.ID, "err", err)
	default:
		m.log.Info("mqtt identity: firmware read over MQTT", "device", d.ID, "topic", d.Topic, "via", via, "version", next.Version, "variant", next.Variant)
		if m.announce != nil {
			m.announce(d.ID)
		}
	}
}

// wantsStatus2 reports whether the device still lacks what only Status 2 gives (the hardware),
// and can be asked: minimal and safeboot firmware do not know Status 2.
func wantsStatus2(d model.Device) bool {
	i := d.Identity
	return d.Topic != "" && d.MQTT == model.MQTTOurs && i.DetectedAt.IsZero() && i.Hardware == "" &&
		i.Variant != "minimal" && i.Variant != "safeboot"
}

// askDue sends at most one Status 2 to the device that has waited longest, keeping mqttIDGap on
// the bus and mqttIDEvery per device. Never Status 0.
func (m *mqttIdentity) askDue(ctx context.Context) {
	now := m.now()
	if !m.bus.Connected() {
		return
	}
	m.mu.Lock()
	if !m.lastAsk.IsZero() && now.Sub(m.lastAsk) < mqttIDGap {
		m.mu.Unlock()
		return
	}
	waiting := false // any topic online long enough? then the inventory is worth reading
	for _, t := range m.topics {
		if !t.up.IsZero() && now.Sub(t.up) >= m.grace {
			waiting = true
			break
		}
	}
	m.mu.Unlock()
	if !waiting {
		return
	}
	devs, err := m.list(ctx)
	if err != nil {
		m.log.Warn("mqtt identity: list devices", "err", err)
		return
	}
	m.mu.Lock()
	var pick *model.Device
	for i := range devs {
		d := &devs[i]
		t := m.topics[d.Topic]
		if !wantsStatus2(*d) || m.answered[d.ID] || t == nil || t.up.IsZero() || now.Sub(t.up) < m.grace {
			continue
		}
		if p, ok := m.pending[d.ID]; ok && now.Sub(p.at) <= mqttIDWindow {
			continue // one command at a time per device
		}
		last, was := m.asked[d.ID]
		if was && now.Sub(last) < mqttIDEvery {
			continue
		}
		if pick == nil || m.asked[pick.ID].After(last) {
			pick = d
		}
	}
	if pick == nil {
		m.mu.Unlock()
		return
	}
	m.asked[pick.ID], m.lastAsk = now, now
	m.pending[pick.ID] = mqttIDPending{topic: pick.Topic, at: now}
	m.mu.Unlock()

	why := "on our broker without a usable firmware version over HTTP; Status 2 reports it"
	if err := m.bus.Command(ctx, pick.Topic, "STATUS", "2"); err != nil {
		m.mu.Lock()
		delete(m.asked, pick.ID)
		delete(m.pending, pick.ID)
		m.mu.Unlock()
		m.log.Warn("mqtt identity: Status 2 not sent", "device", pick.ID, "topic", pick.Topic, "err", err)
		return
	}
	m.log.Info("mqtt identity: asked a device for its firmware over MQTT", "device", pick.ID, "topic", pick.Topic, "command", "Status 2", "reason", why)
	detail, _ := json.Marshal(map[string]any{"command": "Status 2", "via": "mqtt", "reason": why}) // a map of strings cannot fail
	m.audit.Record(ctx, model.AuditEvent{Actor: "system", Action: "identity.mqtt_ask", Target: "device:" + strconv.FormatInt(int64(pick.ID), 10),
		Detail: string(detail), Result: "ok"})
}
