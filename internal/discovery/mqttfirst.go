package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// MQTT-first discovery (spec 11.3a): devices that publish no
// tasmota/discovery config are found from their own topics. LWT, STATE and
// INFO say that a topic exists and whether it is online; they carry no MAC.
// For such a topic discovery asks once for Status 5 (the smallest reply with
// the MAC) and treats the answer like a discovery config.

// Limits of the topic side (security doc D12).
const (
	maxTopics    = 1024             // topics tracked; the oldest sighting is dropped
	askGap       = time.Second      // between two Status 5 queries on the whole bus
	askEvery     = 10 * time.Minute // between two queries of one unresolved topic
	reprobeEvery = 10 * time.Minute // between two first probes of one device without identity
	// askGrace is how long a topic must be online before it is asked: Tasmota
	// publishes its discovery config a few seconds after the LWT, and a device
	// that announces itself needs no question.
	askGrace = 30 * time.Second
	// askReplyWindow is how long after the question a STATUS5 reply is taken (D13).
	askReplyWindow = 30 * time.Second
)

// topicState is what MQTT told about one %topic%.
type topicState struct {
	lwt    string // "Online", "Offline" or "" (none seen)
	live   bool   // a non-retained message arrived since the last Offline
	seen   time.Time
	up     time.Time // since when it is online (zero while offline)
	mac    string    // resolved MAC (discovery config or Status 5), "" if unknown
	known  bool      // an inventory device has this topic
	asked  time.Time
	host   string // INFO2 / Status 5 hints, for the list only
	ip     string
	reason string // why it is not a proposal, for the list
}

func (s *topicState) online() bool { return s.live || s.lwt == "Online" }

// Unidentified is a topic seen on MQTT that is not (yet) a proposal or an
// inventory device: no MAC known, or its address cannot be used.
type Unidentified struct {
	Topic    string
	LWT      string
	Online   bool
	SeenAt   time.Time
	Hostname string
	IP       string
	Reason   string
}

// Unidentified lists the topics seen on MQTT that discovery could not turn
// into a proposal, sorted by topic.
func (m *Manager) Unidentified() []Unidentified {
	m.tmu.Lock()
	defer m.tmu.Unlock()
	var out []Unidentified
	for t, s := range m.topics {
		if s.known || (s.mac != "" && s.reason == "") {
			continue
		}
		out = append(out, Unidentified{Topic: t, LWT: s.lwt, Online: s.online(), SeenAt: s.seen, Hostname: s.host, IP: s.ip, Reason: s.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// topicLWT returns the last LWT of topic ("" if none or the topic is unknown).
func (m *Manager) topicLWT(topic string) string {
	if topic == "" {
		return ""
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		return s.lwt
	}
	return ""
}

// stateFor returns the entry of topic, creating it (and dropping the oldest
// one when the table is full). The caller holds tmu.
func (m *Manager) stateFor(topic string, now time.Time) *topicState {
	if s := m.topics[topic]; s != nil {
		return s
	}
	if len(m.topics) >= maxTopics {
		oldest := ""
		for t, s := range m.topics {
			if oldest == "" || s.seen.Before(m.topics[oldest].seen) {
				oldest = t
			}
		}
		delete(m.topics, oldest)
	}
	s := &topicState{seen: now}
	m.topics[topic] = s
	return s
}

// noteDiscoveryTopic records that a discovery config accounts for topic.
func (m *Manager) noteDiscoveryTopic(c candidate) {
	if c.topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if !m.attached {
		return
	}
	s := m.stateFor(c.topic, m.clk.Now())
	s.mac, s.ip = c.mac, c.ip.String()
}

// setReason records why a resolved topic did not become a proposal.
func (m *Manager) setReason(topic, reason string) {
	if topic == "" {
		return
	}
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if s := m.topics[topic]; s != nil {
		s.reason = reason
	}
}

// handleTopic evaluates one LWT, STATE, INFO or STATUS5 message.
func (m *Manager) handleTopic(ctx context.Context, msg mqttbus.Message) {
	now := m.clk.Now()
	m.tmu.Lock()
	if !m.attached {
		m.tmu.Unlock()
		return
	}
	s := m.stateFor(msg.Topic, now)
	s.seen = now
	var answer []byte
	switch msg.Kind {
	case mqttbus.KindLWT:
		switch p := strings.TrimSpace(string(msg.Payload)); {
		case strings.EqualFold(p, "online"):
			s.lwt = "Online"
		case strings.EqualFold(p, "offline"):
			s.lwt, s.live = "Offline", false
		}
	case mqttbus.KindState, mqttbus.KindInfo:
		if !msg.Retained {
			s.live = true // a retained telemetry message says nothing about now
		}
		if msg.Suffix == "INFO2" {
			var p struct {
				Info2 struct{ Hostname, IPAddress string }
			}
			if json.Unmarshal(msg.Payload, &p) == nil {
				s.host, s.ip = cleanName(p.Info2.Hostname, "000000000000"), strings.TrimSpace(p.Info2.IPAddress)
			}
		}
	case mqttbus.KindStatus:
		if !s.asked.IsZero() && now.Sub(s.asked) <= askReplyWindow && s.mac == "" && !s.known {
			answer = msg.Payload // only a timely reply to discovery's own question (D13)
		}
		if !msg.Retained {
			s.live = true
		}
	case mqttbus.KindSensor, mqttbus.KindResult, mqttbus.KindPower, mqttbus.KindDiscovery, mqttbus.KindUpgrade:
		// not queued for the topic side (HandleMQTT, handleMessage)
	}
	online := s.online()
	switch {
	case !online:
		s.up = time.Time{}
	case s.up.IsZero():
		s.up = now
	}
	m.tmu.Unlock()

	devs, err := m.o.Inventory.List(ctx)
	if err != nil {
		m.log.Warn("discovery: list devices", "err", err)
		return
	}
	for _, d := range devs {
		if d.Topic == msg.Topic {
			m.tmu.Lock()
			s.known = true
			m.tmu.Unlock()
			if online {
				m.reprobe(ctx, d, now)
			}
			return
		}
	}
	m.tmu.Lock()
	s.known = false
	switch {
	case s.mac != "":
	case !online:
		s.reason = "offline: it cannot be asked for its MAC until it comes online"
	case s.asked.IsZero():
		s.reason = "no tasmota/discovery config; asking it for its MAC (Status 5)"
	default:
		s.reason = "no answer to Status 5 yet"
	}
	m.tmu.Unlock()
	if answer != nil {
		m.handleStatus5(ctx, msg.Topic, answer)
	}
}

// handleStatus5 turns the Status 5 answer of an asked topic into a candidate.
func (m *Manager) handleStatus5(ctx context.Context, topic string, payload []byte) {
	c, err := parseStatus5(topic, payload)
	if err != nil {
		m.log.Debug("discovery: Status 5 answer ignored", "topic", topic, "err", err)
		m.setReason(topic, "its Status 5 answer was not usable")
		return
	}
	m.tmu.Lock()
	if s := m.topics[topic]; s != nil {
		s.mac, s.ip, s.host, s.reason = c.mac, c.ip.String(), c.name, ""
	}
	m.tmu.Unlock()
	out, err := m.consider(ctx, c)
	switch {
	case err != nil && ctx.Err() == nil:
		m.log.Warn("discovery: could not evaluate device", "mac", c.mac, "err", err)
	case out == outIgnored:
		m.setReason(topic, fmt.Sprintf("its address %s is outside the device networks", c.ip))
	}
}

// parseStatus5 decodes a stat/<topic>/STATUS5 reply. The Wi-Fi MAC is the
// main one (it is the MAC of tasmota/discovery too); the Ethernet MAC is an
// additional one. The address is the Wi-Fi one, or the Ethernet one when the
// device has no Wi-Fi address.
func parseStatus5(topic string, payload []byte) (candidate, error) {
	if len(payload) > maxDiscoveryPayload {
		return candidate{}, badClaim("payload of %d bytes", len(payload))
	}
	var p struct {
		StatusNET struct {
			Hostname, IPAddress, Mac string
			Ethernet                 struct{ IPAddress, Mac string }
		}
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return candidate{}, badClaim("not JSON")
	}
	n := p.StatusNET
	mac, err := inventory.NormaliseMAC(n.Mac)
	if err != nil || mac == "" || mac == "00:00:00:00:00:00" {
		return candidate{}, badClaim("no valid MAC")
	}
	ip := usableIP(n.IPAddress)
	if !ip.IsValid() {
		ip = usableIP(n.Ethernet.IPAddress)
	}
	if !ip.IsValid() {
		return candidate{}, badClaim("no valid IP address")
	}
	c := candidate{via: ViaMQTT, mac: mac, ip: ip, topic: cleanTopic(topic)}
	if eth, err := inventory.NormaliseMAC(n.Ethernet.Mac); err == nil && eth != "" && eth != mac && eth != "00:00:00:00:00:00" {
		c.macs = []string{eth}
	}
	c.name = cleanName(firstNonEmpty(n.Hostname, topic), mac)
	return c, nil
}

func usableIP(s string) netip.Addr {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.IsUnspecified() {
		return netip.Addr{}
	}
	return a.Unmap()
}

// askDue sends at most one Status 5 to the oldest unresolved online topic,
// keeping askGap on the bus and askEvery per topic (D12). Never Status 0.
func (m *Manager) askDue(ctx context.Context) {
	now := m.clk.Now()
	m.tmu.Lock()
	bus := m.bus
	m.tmu.Unlock()
	// Connected is asked outside tmu (a bus may take its own lock), and while the
	// broker is away nothing is picked: a query could not be sent anyway.
	if bus == nil || !bus.Connected() {
		return
	}
	m.tmu.Lock()
	if m.bus != bus || !m.attached || (!m.lastAsk.IsZero() && now.Sub(m.lastAsk) < askGap) {
		m.tmu.Unlock()
		return
	}
	topic := ""
	var pick *topicState
	for t, s := range m.topics {
		if s.known || s.mac != "" || !s.online() || now.Sub(s.up) < m.askGrace || (!s.asked.IsZero() && now.Sub(s.asked) < askEvery) {
			continue
		}
		if pick == nil || s.asked.Before(pick.asked) || (s.asked.Equal(pick.asked) && t < topic) {
			topic, pick = t, s
		}
	}
	if pick == nil {
		m.tmu.Unlock()
		return
	}
	pick.asked, m.lastAsk = now, now
	m.tmu.Unlock()

	why := "on the broker without a tasmota/discovery config; Status 5 reports its MAC"
	err := bus.Command(ctx, topic, "STATUS", "5")
	if err != nil {
		m.tmu.Lock()
		if s := m.topics[topic]; s == pick {
			s.asked = time.Time{} // not sent: try again at the next gap
		}
		m.tmu.Unlock()
		m.log.Warn("discovery: Status 5 not sent", "topic", topic, "err", err)
		return
	}
	m.log.Info("discovery: asked a topic for its MAC", "topic", topic, "command", "Status 5", "reason", why)
	m.audit(ctx, "discovery", "discovery.identify", "mqtt:"+topic, map[string]any{"command": "Status 5", "reason": why}, nil)
}

// markProbed records that the first probe of id was just started (accept),
// so a message before it stored the identity does not start a second one.
func (m *Manager) markProbed(id model.DeviceID) {
	m.tmu.Lock()
	m.reprobed[id] = m.clk.Now()
	m.tmu.Unlock()
}

// reprobe starts the first probe again for an inventory device that came
// online without a detected identity (added while offline), at most once per
// reprobeEvery per device (spec 11.3a "Filled in later").
//
// The device was added without MAC proof, so its stored address must first
// answer with its MAC (credential-less Status 5 through the Verifier): another
// MAC there means another device, whose identity must not be stored under this
// one. The refusal is audited; no answer (a password-protected device) lets the
// probe run, it goes to the address the admin accepted.
func (m *Manager) reprobe(ctx context.Context, d model.Device, now time.Time) {
	if m.o.OnAccepted == nil || !d.Identity.DetectedAt.IsZero() {
		return
	}
	m.tmu.Lock()
	if t, ok := m.reprobed[d.ID]; ok && now.Sub(t) < reprobeEvery {
		m.tmu.Unlock()
		return
	}
	if len(m.reprobed) >= maxTopics {
		for id, t := range m.reprobed {
			if now.Sub(t) >= reprobeEvery {
				delete(m.reprobed, id)
			}
		}
	}
	m.reprobed[d.ID] = now
	m.tmu.Unlock()
	if m.o.Verifier != nil && d.MAC != "" { // no stored MAC: nothing to compare, the admin set the address
		if mac, err := m.o.Verifier.VerifyMAC(ctx, d.Address, portOf(d)); err == nil {
			if got, nerr := inventory.NormaliseMAC(mac); nerr != nil || !d.HasMAC(got) {
				m.log.Warn("discovery: stored address answers with another MAC, identity probe not started", "device", d.ID, "address", d.Address)
				m.audit(ctx, "discovery", "discovery.identify_refused", fmt.Sprintf("device:%d", d.ID),
					map[string]any{"address": d.Address, "reason": "the stored address answers with another MAC"}, nil)
				return
			}
		}
	}
	m.log.Info("discovery: device came online without identity, probing it", "device", d.ID, "topic", d.Topic)
	m.o.OnAccepted(d.ID)
}
