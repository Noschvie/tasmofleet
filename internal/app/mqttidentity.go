package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Identity over MQTT (#87, first step of #79): a device on our broker whose firmware version
// HTTP cannot provide (unreachable, refused, never probed) gets it from MQTT.
//
//   - INFO1, which every device publishes at boot, carries the version: it is read with the
//     Status 2 grammar (fwselect.DetectInfo1). It has no hardware, so on its own it never makes
//     the identity Known().
//   - Status 2 is asked once over MQTT (cmnd/<topic>/Status 2) for the hardware. Never Status 0,
//     one command at a time per device, once per device per 10 minutes, at most one per second
//     on the whole bus, only for devices that are online, and audited like discovery's Status 5.
//     The question goes through the MQTT command channel (internal/mqttcmd), which owns the
//     publish, the reply correlation and the timeout; this file keeps the limits above.
//
// The HTTP probe stays the primary path. MQTT only fills a device that no probe has identified
// (DetectedAt zero; the store refuses the write otherwise) and never sets DetectedAt, so the
// first HTTP probe still runs and replaces whatever MQTT said. The OTA engine probes over HTTP
// before it plans, and Guard runs on that identity, unchanged.
const (
	mqttIDGap     = time.Second      // between two Status 2 questions on the whole bus
	mqttIDEvery   = 10 * time.Minute // between two questions to one device
	mqttIDGrace   = 30 * time.Second // how long a device must be online before it is asked: HTTP goes first
	mqttIDQueue   = 64               // INFO1 messages waiting for the worker; more are dropped
	mqttIDTick    = time.Second
	mqttIDTimeout = 10 * time.Second
)

// identityMQTTStore is the part of the store the identity reader writes through.
type identityMQTTStore interface {
	UpdateIdentityFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic string, i model.Identity) error
	UpdateGroupTopicFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, groupTopic string) error
}

type mqttIDTopic struct {
	lwt  string    // "Online", "Offline" or "" (none seen)
	live bool      // a non-retained message arrived since the last Offline
	up   time.Time // since when it is online (zero while offline)
}

type mqttIdentity struct {
	bus      mqttbus.Bus // online state of the topics; commands go through cmd
	cmd      mqttcmd.Doer
	list     func(context.Context) ([]model.Device, error)
	store    identityMQTTStore
	audit    audit.Recorder
	announce func(model.DeviceID)
	now      func() time.Time
	log      *slog.Logger
	grace    time.Duration

	// known, if set, limits the topic table to inventory devices (mqttKnown.Known): a flood of
	// topics on the broker must not fill it.
	known func(mqttbus.Message) bool
	msgs  chan mqttbus.Message

	mu       sync.Mutex
	topics   map[string]*mqttIDTopic
	lastAsk  time.Time
	asked    map[model.DeviceID]time.Time
	answered map[model.DeviceID]bool // a Status 2 reply was stored; not asked again until restart
	info1    map[string][]byte       // INFO1 of a topic whose device was not yet recorded as ours
	asking   map[string]bool         // topic -> a question (background or refresh) waits for its answer

	spawn func(func()) // runs the background question; tests run it inline
}

func newMQTTIdentity(bus mqttbus.Bus, cmd mqttcmd.Doer, list func(context.Context) ([]model.Device, error), st identityMQTTStore,
	rec audit.Recorder, announce func(model.DeviceID), log *slog.Logger) *mqttIdentity {
	return &mqttIdentity{bus: bus, cmd: cmd, list: list, store: st, audit: rec, announce: announce, now: time.Now, log: log, grace: mqttIDGrace,
		msgs: make(chan mqttbus.Message, mqttIDQueue), topics: map[string]*mqttIDTopic{},
		asked: map[model.DeviceID]time.Time{}, answered: map[model.DeviceID]bool{}, info1: map[string][]byte{},
		asking: map[string]bool{}, spawn: func(f func()) { go f() }}
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
	if m.known != nil && !m.known(msg) {
		return
	}
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
	case mqttbus.KindSensor, mqttbus.KindResult, mqttbus.KindPower, mqttbus.KindDiscovery, mqttbus.KindUpgrade, mqttbus.KindFile:
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
	if msg.Kind == mqttbus.KindInfo && msg.Suffix == "INFO1" {
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
	if ok && msg.Suffix == "INFO1" && !msg.Retained {
		m.applyGroupTopic(ctx, d, msg.Payload) // #239: whatever the identity rules below say
	}
	if !ok || !d.Identity.DetectedAt.IsZero() {
		return // not ours, or HTTP has identified it: that identity is the one we keep
	}
	if d.MQTT != model.MQTTOurs {
		if msg.Suffix == "INFO1" {
			// Published once per boot, possibly before health recorded the device as ours:
			// keep the last one and read it when the device is ours (applyStashed).
			m.mu.Lock()
			if len(m.info1) < mqttIDQueue {
				m.info1[msg.Topic] = msg.Payload
			}
			m.mu.Unlock()
		}
		return
	}
	m.applyInfo1(ctx, d, msg.Payload)
}

// info1GroupTopic is the GroupTopic of an INFO1 message as the plain topic. INFO1 writes it in
// its command-prefix form ("cmnd/tasmotas/"), Status 1 and the settings as "tasmotas"; the list
// shows one form. ok is false when the message has no GroupTopic, not in that form, or not usable.
func info1GroupTopic(payload []byte) (string, bool) {
	var m struct {
		Info1 *struct{ GroupTopic *string }
	}
	if json.Unmarshal(payload, &m) != nil || m.Info1 == nil || m.Info1.GroupTopic == nil {
		return "", false
	}
	g := strings.TrimSpace(*m.Info1.GroupTopic)
	if g == "" {
		return "", true
	}
	// Only the default FullTopic form "cmnd/<group>/" says what the group is. With another FullTopic
	// ("%topic%/%prefix%/", a custom prefix) the announcement cannot be told from the plain topic
	// without the device's patterns, so it is left to Status 1: guessing would make the value flip
	// between the two reads and miss a topic that equals it (#260).
	g, ok := strings.CutPrefix(g, "cmnd/")
	if !ok {
		return "", false
	}
	g, ok = strings.CutSuffix(g, "/")
	if !ok || strings.Contains(g, "/") {
		return "", false
	}
	if !validGroupTopic(g) {
		return "", false
	}
	return g, true
}

// applyGroupTopic stores the group topic a live INFO1 announced (#239). It is display only and
// costs no read; a retained INFO1 is no reading of now and never reaches this.
func (m *mqttIdentity) applyGroupTopic(ctx context.Context, d model.Device, payload []byte) {
	g, ok := info1GroupTopic(payload)
	if !ok {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, mqttIDTimeout)
	defer cancel()
	err := m.store.UpdateGroupTopicFromMQTT(sctx, d.ID, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, d.Topic, g)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		m.log.Warn("mqtt identity: could not store the group topic", "device", d.ID, "err", err)
	}
}

// applyInfo1 takes the version of an INFO1 and nothing else: it carries no hardware, so it must
// not choose image or variant (a forged "(release-minimal)" would turn the device into minimal
// firmware for the update form). A device whose hardware is known has nothing to learn from it.
func (m *mqttIdentity) applyInfo1(ctx context.Context, d model.Device, payload []byte) {
	if d.Identity.Hardware != "" {
		return
	}
	parsed, err := fwselect.DetectInfo1(payload, d.Identity)
	if err != nil {
		m.log.Debug("mqtt identity: INFO1 not usable", "device", d.ID, "err", err)
		return
	}
	next := d.Identity
	next.Version = parsed.Version
	m.store1(ctx, d, next, "INFO1")
}

// applyStatus2 takes the identity of a Status 2 reply. Minimal and safeboot images are refused:
// they do not answer Status 2, so such a reply is not from a device that runs them.
func (m *mqttIdentity) applyStatus2(ctx context.Context, d model.Device, payload []byte) {
	next, err := fwselect.Detect(fwselect.Inputs{Status2: payload})
	if err != nil {
		m.log.Debug("mqtt identity: Status 2 reply not usable", "device", d.ID, "err", err)
		return
	}
	if next.Variant == "minimal" || next.Variant == "safeboot" {
		m.log.Warn("mqtt identity: Status 2 reply claims an image that cannot answer it, ignored", "device", d.ID, "image", next.Image)
		return
	}
	next.Language = d.Identity.Language
	if m.store1(ctx, d, next, "Status 2") {
		m.mu.Lock()
		m.answered[d.ID] = true
		m.mu.Unlock()
	}
}

// store1 writes next if it differs from what is stored. It reports whether the stored identity
// now equals next (written, or already so).
func (m *mqttIdentity) store1(ctx context.Context, d model.Device, next model.Identity, via string) bool {
	cur := d.Identity
	if next.Family == cur.Family && next.Hardware == cur.Hardware && next.SingleCore == cur.SingleCore &&
		next.Version == cur.Version && next.Image == cur.Image && next.Variant == cur.Variant {
		return true
	}
	sctx, cancel := context.WithTimeout(ctx, mqttIDTimeout)
	defer cancel()
	err := m.store.UpdateIdentityFromMQTT(sctx, d.ID, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, d.Topic, next)
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
		return true
	}
	return false
}

// applyStashed reads the INFO1 kept for devices that were not yet recorded as ours.
func (m *mqttIdentity) applyStashed(ctx context.Context) {
	m.mu.Lock()
	n := len(m.info1)
	m.mu.Unlock()
	if n == 0 {
		return
	}
	devs, err := m.list(ctx)
	if err != nil {
		m.log.Warn("mqtt identity: list devices", "err", err)
		return
	}
	for _, d := range devs {
		m.mu.Lock()
		payload, ok := m.info1[d.Topic]
		m.mu.Unlock()
		if !ok || (d.MQTT != model.MQTTOurs && d.Identity.DetectedAt.IsZero()) {
			continue // not yet ours: keep it
		}
		m.mu.Lock()
		delete(m.info1, d.Topic)
		m.mu.Unlock()
		if d.MQTT == model.MQTTOurs && d.Identity.DetectedAt.IsZero() {
			m.applyInfo1(ctx, d, payload)
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
		// What was known about "online" is stale once the connection is lost: after the reconnect
		// the retained LWTs say it again, and the grace period for HTTP starts anew.
		m.mu.Lock()
		m.topics = map[string]*mqttIDTopic{}
		m.mu.Unlock()
		return
	}
	m.applyStashed(ctx)
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
		if m.asking[d.Topic] {
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
	m.asking[pick.Topic] = true
	m.mu.Unlock()
	d := *pick
	m.spawn(func() { m.askStatus2(ctx, d) })
}

// askStatus2 puts the one background Status 2 to d. Only the reply the command channel matched to
// the question can become an identity (a reply anyone can publish is no source).
func (m *mqttIdentity) askStatus2(ctx context.Context, d model.Device) {
	defer func() {
		m.mu.Lock()
		delete(m.asking, d.Topic)
		m.mu.Unlock()
	}()
	raw, err := m.cmd.Do(ctx, d, "Status 2")
	if notSent(err) {
		m.mu.Lock()
		delete(m.asked, d.ID) // nothing was sent: no reason to wait ten minutes
		m.mu.Unlock()
		m.log.Warn("mqtt identity: Status 2 not sent", "device", d.ID, "topic", d.Topic, "err", err)
		return
	}
	why := "on our broker without a usable firmware version over HTTP; Status 2 reports it"
	m.log.Info("mqtt identity: asked a device for its firmware over MQTT", "device", d.ID, "topic", d.Topic, "command", "Status 2", "reason", why)
	detail, _ := json.Marshal(map[string]any{"command": "Status 2", "via": "mqtt", "reason": why}) // a map of strings cannot fail
	m.audit.Record(ctx, model.AuditEvent{Actor: "system", Action: "identity.mqtt_ask", Target: "device:" + strconv.FormatInt(int64(d.ID), 10),
		Detail: string(detail), Result: "ok"})
	if err != nil {
		m.log.Debug("mqtt identity: no usable Status 2 answer", "device", d.ID, "err", err)
		return
	}
	m.applyStatus2(ctx, d, raw)
}

// notSent says the channel refused or could not publish: the device was not asked.
func notSent(err error) bool {
	if errors.Is(err, tasmota.ErrTimeout) {
		return false // published, outcome unknown (this includes a caller that gave up after the publish)
	}
	return errors.Is(err, mqttcmd.ErrRefused) || errors.Is(err, tasmota.ErrUnreachable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) // still queued
}

// statusObject asks the plain "Status" (no number) over MQTT.
const statusObject = -1

var (
	errMQTTUnavailable = errors.New("mqtt: not connected, or the device is not on our broker")
	errMQTTBusy        = errors.New("mqtt: a question to this device is still waiting for its answer")
)

// askStatus asks d one Status n over MQTT for a refresh (#91) and returns the answer. It keeps
// the limits of the background reader: never Status 0, one question at a time per device,
// mqttIDGap between two questions on the whole bus (it waits for its turn), and every question
// audited. The command channel publishes, takes only the live STATUSn on the device's own topic
// as the answer and times out after its reply wait.
func (m *mqttIdentity) askStatus(ctx context.Context, d model.Device, n int, actor string) (json.RawMessage, error) {
	if n == 0 || n < statusObject {
		return nil, errors.New("mqtt: Status 0 is never sent")
	}
	// statusObject is the plain "Status": the Status object only (DeviceName, Topic), as over HTTP.
	name := "Status " + strconv.Itoa(n)
	if n == statusObject {
		name = "Status"
	}
	if d.Topic == "" || d.MQTT != model.MQTTOurs || !m.bus.Connected() {
		return nil, errMQTTUnavailable
	}
	now := m.now()
	m.mu.Lock()
	if m.asking[d.Topic] {
		m.mu.Unlock()
		return nil, errMQTTBusy
	}
	turn := now
	if next := m.lastAsk.Add(mqttIDGap); !m.lastAsk.IsZero() && next.After(turn) {
		turn = next
	}
	m.lastAsk = turn
	m.asking[d.Topic] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.asking, d.Topic)
		m.mu.Unlock()
	}()
	if wait := turn.Sub(now); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	raw, err := m.cmd.Do(ctx, d, name)
	if notSent(err) {
		return nil, fmt.Errorf("mqtt: %s not sent: %w", name, err)
	}
	if actor == "" {
		actor = "system"
	}
	why := "refresh identity: HTTP cannot reach the device"
	m.log.Info("mqtt identity: asked a device over MQTT", "device", d.ID, "topic", d.Topic, "command", name, "reason", why)
	detail, _ := json.Marshal(map[string]any{"command": name, "via": "mqtt", "reason": why}) // a map of strings cannot fail
	m.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "identity.mqtt_ask", Target: "device:" + strconv.FormatInt(int64(d.ID), 10),
		Detail: string(detail), Result: "ok"})
	if err != nil {
		return nil, err
	}
	return raw, nil
}
