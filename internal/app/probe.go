package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/audit"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// identityStore is the part of the store the prober needs.
type identityStore interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	UpdateProbe(ctx context.Context, id model.DeviceID, g store.ProbeGuard, r store.ProbeResult) error
	UpdateNet(ctx context.Context, id model.DeviceID, g store.ProbeGuard, n model.NetFacts) error
	UpdateMQTTBroker(ctx context.Context, id model.DeviceID, g store.ProbeGuard, c model.MQTTCapability, host string, port int) error
}

// topicMarker is transport.TopicMarks as the prober uses it.
type topicMarker interface {
	Mark(id model.DeviceID, topic string)
	Clear(id model.DeviceID, topic string)
	Since(id model.DeviceID, topic string) time.Time
}

// topicChecker is the router's check that hears every reply to a Status 5 on a device's stored topic
// (transport.Router.TopicStatus5): the only check that may end a topic mark (#852).
type topicChecker interface {
	TopicStatus5(ctx context.Context, id model.DeviceID) (model.Device, []json.RawMessage, error)
}

// brokerMatcher tells whether a device's MQTT settings point at the broker
// tasmofleet uses (mqttbus.BrokerMatcher).
type brokerMatcher interface {
	Same(host string, port int) bool
}

// Prober learns a device's identity from individual Status queries (never
// Status 0, spec section 5), one at a time with a pause in between.
type Prober struct {
	Client tasmota.Client
	// Reads is the client of the background reads of a stored device (ReadNet): the router, so that a
	// device on our broker is read over MQTT first (#456). Nil: Client. The identity probe of a stored device
	// (Refresh, RefreshFacts) goes over it, MQTT first, after the check that hears every reply on the topic (#682);
	// Describe, and every probe MQTT cannot serve, use Client, the address.
	Reads   tasmota.Client
	Devices identityStore
	// Gap is the pause between two commands to the same device (default 200 ms).
	Gap time.Duration
	Now func() time.Time
	// Broker is the tasmofleet broker; nil when none is configured.
	Broker brokerMatcher
	// ReadNetLater queues the background read of a device's network facts (ReadNet) after a
	// refresh; nil in processes without one (the CLI): the server's reader catches up.
	ReadNetLater func(model.DeviceID)
	// Hub, if set, hears of every stored probe result (Refresh, ReadNet) as a device event, so open
	// device tables reload and show the new firmware, module and network facts. Nil in the CLI.
	Hub *events.Hub
	// Audit records refused refreshes, MAC bindings and a refresh's questions over MQTT. Nil: not recorded (tests).
	Audit audit.Recorder
	// Topics is the router's memory of devices on whose MQTT topic another device answered (#852,
	// transport.TopicMarks): every MAC check answered over MQTT alone that shows another MAC sets it (noteTopic), and
	// only a check that hears the device alone on its topic clears it (recheckTopic). Nil: not kept.
	Topics topicMarker
	// ReadsOverMQTT says whether the router reads d over MQTT now (transport.Choose: Commands is MQTT). The identity
	// probe of a stored device then reads it over MQTT first, identity and facts, after the check that hears every
	// reply on the topic (#682). Nil: the probe reads at the address (the CLI).
	ReadsOverMQTT func(context.Context, model.Device) bool
}

// announce tells open pages that the stored facts of a device changed.
func (p *Prober) announce(id model.DeviceID) {
	if p.Hub != nil {
		p.Hub.Publish(events.Event{Topic: events.TopicDevice, At: p.now(), Payload: model.Device{ID: id}})
	}
}

const unknownReply = `{"Command":"Unknown"}`

type probed struct {
	identity model.Identity
	mac      string
	ethMAC   string // Ethernet MAC of boards that have one, else ""
	topic    string // resolved: what the device publishes under, never a %06X placeholder
	hostname string
	devName  string // Tasmota DeviceName
	mqtt     model.MQTTCapability
	mqttHost string
	mqttPort int
	minimal  bool
	net      model.NetFacts // what Status 5 reported (Wi-Fi facts not included)
	// otaURL is the OtaUrl of Status 1 with its credentials removed; readOTAURL tells whether
	// Status 1 gave a usable answer (else the stored value is kept).
	otaURL     string
	readOTAURL bool
	// groupTopic is the GroupTopic of the same Status 1 answer; readGroupTopic tells whether it had one.
	groupTopic     string
	readGroupTopic bool
	relayNames     []string // FriendlyName array of plain Status
	// checkedTopic is the stored topic the check that hears every reply asked when the whole probe went over MQTT
	// (probeOverMQTT, #682), "" when it went to the address. The store writes the result only while the device
	// still has that topic.
	checkedTopic string
}

// via names the transport the probe's check and reads went over, for the audit and the logs.
func (o probed) via() string {
	if o.checkedTopic != "" {
		return "mqtt"
	}
	return "http"
}

func (p *Prober) pause(ctx context.Context) error {
	gap := p.Gap
	if gap == 0 {
		gap = 200 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(gap):
		return nil
	}
}

func (p *Prober) cmd(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if err := p.pause(ctx); err != nil {
		return nil, err
	}
	return p.Client.Command(ctx, t, c)
}

// probe queries the device, never with Status 0, one command at a time. A stored device (verify set) is read over
// MQTT first (probeOverMQTT, #682): its identity and its facts, once the check that hears every reply on its topic
// heard it alone there with a stored MAC. Otherwise, and when MQTT leaves a read unanswered, the whole probe runs at
// the address (probeAddress), whose own Status 5 vouches for it; nothing of the MQTT half is kept then. The error of
// an address probe that fails after MQTT learnt why it could not be used carries that reason along.
func (p *Prober) probe(ctx context.Context, d model.Device, verify func(probed) error) (probed, error) {
	if verify == nil {
		return p.probeAddress(ctx, d, nil)
	}
	out, used, merr := p.probeOverMQTT(ctx, d, verify)
	switch {
	case used:
		return out, merr
	case ctx.Err() != nil:
		return out, ctx.Err()
	}
	out, err := p.probeAddress(ctx, d, verify)
	if err != nil && merr != nil {
		return out, fmt.Errorf("%w (over MQTT: %s)", err, merr.Error())
	}
	return out, err
}

// probeOverMQTT is the probe of a stored device whose reads the router sends over MQTT (ReadsOverMQTT, transport.Choose)
// and that has a stored MAC and topic (#682: MQTT first for the identity too). It asks Status 5 on the stored topic and
// hears every reply within the router's identity window (hearTopic, the rule a restore over MQTT and the end of a topic
// mark follow): the identity selects and guards firmware, and a check that hears one reply hears whichever of two
// devices on the topic answers first. Only exactly one reply with a stored MAC goes on; its reply gives the network
// facts, MAC and hostname. Status 2, Status 4 (the language is its locale ID, #686) and the facts (probeFacts) follow,
// pinned to MQTT (pinnedToMQTTCheck: no read falls back to the address the check did not vouch for). The topic the
// device reports is not taken (a device answers only on its own topic, mqtt-ingest.md R6): out.topic stays "".
//
// used is false when the address must carry the probe instead: the device is not read over MQTT, the check learnt
// nothing (no reply, a window cut short, MQTT not usable, a reply without a MAC; err nil), another device answers on
// the topic (err errAnotherDeviceOnTopic, the topic is marked), or a read after the check went unanswered or could not
// be used (err says which: once, T2). Minimal and safeboot firmware answer no Status 5 there, so they never pass the
// check. used with an err: verify refused the device. A context that ended is the caller's to see (probe).
func (p *Prober) probeOverMQTT(ctx context.Context, d model.Device, verify func(probed) error) (out probed, used bool, err error) {
	tc, ok := p.Reads.(topicChecker)
	if !ok || d.MAC == "" || d.Topic == "" || p.ReadsOverMQTT == nil || !p.ReadsOverMQTT(ctx, d) {
		return out, false, nil
	}
	h, ok, err := p.hearTopic(ctx, tc, d)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "identity probe: "+topicTakenLog+"; the probe reads at the address", "device", d.ID, "topic", h.asked.Topic)
		return out, false, err
	case !ok:
		return out, false, nil
	}
	out, out.net, out.checkedTopic = h.reply, h.net, h.asked.Topic
	if err := verify(out); err != nil {
		return out, true, err
	}
	rp := *p
	rp.Client = p.Reads
	t := tasmota.TargetFor(d)
	var in fwselect.Inputs
	if in.Status2, err = rp.cmd(h.ctx, t, "Status 2"); err != nil {
		return probed{}, false, fmt.Errorf("status 2: %w", err)
	}
	if in.Status4, err = rp.cmd(h.ctx, t, "Status 4"); err != nil {
		return probed{}, false, fmt.Errorf("status 4: %w", err)
	}
	if out.identity, err = fwselect.Detect(in); err != nil {
		return probed{}, false, fmt.Errorf("identify: %w", err)
	}
	if v := out.identity.Variant; v == "minimal" || v == "safeboot" {
		// Such an image answers neither Status 5 nor Status 2: the reply cannot be the device's. The address decides.
		return probed{}, false, fmt.Errorf("identify: the answer claims a %s image, which cannot answer Status 2: %w", v, tasmota.ErrBadResponse)
	}
	out.identity.DetectedAt = p.now()
	if err := rp.probeFacts(h.ctx, t, &out); err != nil {
		return probed{}, false, err
	}
	out.topic = "" // the stored topic stays (R6)
	return out, true, nil
}

// probeAddress queries the device at its address. A device on minimal firmware only answers a few commands, so MAC,
// topic and MQTT capability stay empty there.
//
// Status 5 goes first, without credentials: it carries the MAC, and verify (if set) checks it
// before the stored credentials go anywhere (#65). A device that answers it without them has no
// web password, and the whole probe then runs without credentials. Only a device that asks for
// a password (401) gets Status 5 again with them, so a refused device receives them in that
// one command at most. Minimal firmware knows neither Status 5 nor Status 2: Status 2 still
// decides minimal (spec 5) and goes out (with the credentials if the device asked for them)
// before verify runs, which then sees a minimal device without a MAC; nothing follows a
// refusal.
//
// Every read goes to the address the Status 5 checked: a check vouches only for the transport its read used (#810).
// It is the probe of an address that is no stored device yet (Describe: first contact, no topic, no MAC to check),
// and of a stored device MQTT cannot serve (probe). The language is the locale ID of Status 4 (#686); no main page is
// read for full firmware. Minimal and safeboot firmware are named by their INFO1 since the last boot through the
// router when the device is usable over MQTT, and by the main page at the address otherwise.
func (p *Prober) probeAddress(ctx context.Context, d model.Device, verify func(probed) error) (probed, error) {
	t := tasmota.TargetFor(d)
	var out probed
	in := fwselect.Inputs{}

	no5 := false
	raw5, err := p.Client.Command(ctx, tasmota.TargetFor(model.Device{ID: d.ID, Address: d.Address, Port: d.Port}), "Status 5")
	switch {
	case errors.Is(err, tasmota.ErrAuth) && t.HasCredentials():
		raw5, err = p.cmd(ctx, t, "Status 5")
	case err == nil || errors.Is(err, tasmota.ErrUnknownCommand):
		// Answered without a password: the device needs none, so none is sent.
		t = tasmota.TargetFor(model.Device{ID: d.ID, Address: d.Address, Port: d.Port})
	}
	switch {
	case err == nil:
		if out.net, err = parseStatus5(raw5, &out); err != nil {
			return out, err
		}
	case errors.Is(err, tasmota.ErrUnknownCommand):
		no5 = true
	default:
		return out, err
	}
	check := func() error {
		if verify == nil {
			return nil
		}
		return verify(out)
	}
	if !no5 {
		if err := check(); err != nil {
			return out, err
		}
	}
	raw2, err := p.cmd(ctx, t, "Status 2")
	switch {
	case err == nil:
		in.Status2 = raw2
	case errors.Is(err, tasmota.ErrUnknownCommand):
		in.Status2, out.minimal = []byte(unknownReply), true
	default:
		return out, fmt.Errorf("status 2: %w", err)
	}
	if no5 {
		if err := check(); err != nil {
			return out, err
		}
	}
	if !out.minimal {
		raw4, err := p.cmd(ctx, t, "Status 4")
		if err != nil {
			return out, fmt.Errorf("status 4: %w", err)
		}
		in.Status4 = raw4
	} else {
		up, err := p.cmd(ctx, t, "Upgrade")
		if err != nil {
			return out, fmt.Errorf("upgrade probe: %w", err)
		}
		// The bare Upgrade reply's version is what ties the image source below to this device (R5), so an
		// unreadable reply fails the probe, as it fails deviceAPI.firmwareInputs.
		probe, err := tasmota.ParseUpgradeProbe(up)
		if err != nil {
			return out, fmt.Errorf("upgrade probe: %w", err)
		}
		in.Upgrade = &fwselect.UpgradeProbe{Version: probe.Version}
		// Only minimal and safeboot firmware need a source for their image (#686); full firmware is named by
		// Status 2 and its language by the Status 4 locale. The router reads a stored device's INFO1 since its
		// last boot first, when the device is usable over MQTT, and the main page only without one (and for an
		// address that is no stored device yet). A prober without a router (the command line) reads the page.
		if err := p.pause(ctx); err != nil {
			return out, err
		}
		pages := p.Client
		if p.Reads != nil {
			pages = p.Reads
		}
		root, err := pages.RootPage(ctx, t)
		if err != nil {
			return out, fmt.Errorf("main page: %w", err)
		}
		in.Root = &fwselect.RootInfo{Version: root.Version, Image: root.Image, Lang: root.Lang, LangKnown: root.LangKnown, Minimal: root.Minimal}
	}
	if out.identity, err = fwselect.Detect(in); err != nil {
		return out, fmt.Errorf("identify: %w", err)
	}
	out.identity.DetectedAt = p.now()
	if out.minimal {
		return out, nil
	}
	return out, p.probeFacts(ctx, t, &out)
}

// probeFacts reads the probe's facts after the identity: where the device reports over MQTT (Status 6, SetOption3), the
// OTA URL and GroupTopic (Status 1) and the Status object (DeviceName, topic, relay names). Only the MQTT facts must
// answer; Status 1 and the Status object are best effort. The probe calls it on itself (the address) or on a copy
// whose client is the router, with ctx pinned to MQTT after a check on the topic (#851).
func (p *Prober) probeFacts(ctx context.Context, t tasmota.Target, out *probed) error {
	var err error
	out.mqtt, out.mqttHost, out.mqttPort, err = p.readMQTT(ctx, t, false)
	if err != nil {
		return err
	}
	// Status 1 (read-only, once per refresh, never the OtaUrl command) for the OTA URL. Best
	// effort: a failed or unusable answer keeps the stored value.
	if raw, err := p.cmd(ctx, t, "Status 1"); err == nil {
		out.otaURL, out.readOTAURL = parseOtaURL(raw)
		out.groupTopic, out.readGroupTopic = parseGroupTopic(raw)
	}
	// Plain "Status" (not "Status 0") is the Status object only: DeviceName and the topic the
	// device actually uses. The "Topic" command would return the setting with its placeholders
	// ("tasmota_%06X") unresolved, and nothing on the bus ever matches that.
	if raw, err := p.cmd(ctx, t, "Status"); err == nil {
		var s struct {
			Status struct {
				DeviceName, Topic string
				FriendlyName      []string
			}
		}
		if json.Unmarshal(raw, &s) == nil {
			// Both come from the device: only a usable topic is taken, a name is cleaned.
			if inventory.ValidTopic(s.Status.Topic) {
				out.topic = s.Status.Topic
			}
			out.devName = inventory.CleanName(s.Status.DeviceName)
			out.relayNames = relayNames(s.Status.FriendlyName)
		}
	}
	return nil
}

// maxRelayNames bounds the stored FriendlyName array (Tasmota has at most 8 relays).
const maxRelayNames = 8

// relayNames cleans the FriendlyName array of Status; nil when there is no usable name.
func relayNames(in []string) []string {
	var out []string
	for i, n := range in {
		if i == maxRelayNames {
			break
		}
		out = append(out, inventory.CleanName(n))
	}
	for _, n := range out {
		if n != "" {
			return out
		}
	}
	return nil
}

// parseOtaURL reads the OtaUrl of a Status 1 answer and removes anything that can hold a secret:
// user:password, the query (tokens, signatures) and the fragment. ok is false when the answer has
// no OtaUrl key or the value cannot be cleaned safely: a value that does not parse, or one with an
// '@' that is not userinfo (a password containing '#', '?' or '/' mis-parses into host, query or
// path) is dropped, never stored raw.
func parseOtaURL(raw json.RawMessage) (clean string, ok bool) {
	var s struct {
		StatusPRM *struct{ OtaUrl *string }
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusPRM == nil || s.StatusPRM.OtaUrl == nil {
		return "", false
	}
	v := strings.TrimSpace(*s.StatusPRM.OtaUrl)
	if v == "" {
		return "", true
	}
	clean, _, ok = settings.SplitURLCredentials(v)
	return clean, ok
}

// parseGroupTopic reads the GroupTopic of a Status 1 answer (the same one that carries the OtaUrl,
// so it costs no read of its own). ok is false when the answer has no such key; an empty value is
// a read value ("not set"). The topic is a MQTT topic, not a secret.
func parseGroupTopic(raw json.RawMessage) (topic string, ok bool) {
	var s struct {
		StatusPRM *struct{ GroupTopic *string }
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusPRM == nil || s.StatusPRM.GroupTopic == nil {
		return "", false
	}
	topic = strings.TrimSpace(*s.StatusPRM.GroupTopic)
	if !validGroupTopic(topic) {
		return "", false
	}
	return topic, true
}

// validGroupTopic says whether a group topic read from a device or from the bus is shown: at most
// Tasmota's TOPSZ (151) bytes, no MQTT wildcard, NUL or control character. Any client on the broker
// can publish an INFO1, and the value is rendered on every device list load, so anything else is
// refused and the stored value stays.
func validGroupTopic(t string) bool {
	if len(t) > 151 {
		return false
	}
	for _, r := range t {
		if r == '+' || r == '#' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// readMQTT reads where the device itself is configured to report (Status 6, then SetOption3 when a
// host is set) and classifies it against our broker. No MqttHost, or SetOption3 off, means the device
// does not talk MQTT at all; otherwise it is "ours" when it is configured for the tasmofleet broker
// and "other" for any other target (or when tasmofleet has no broker). Shared by the probe and the
// background re-read (ReadBroker, #598), so both store the same facts.
//
// The probe has proved regular firmware first (Status 2), so there an answer that says nothing about
// MQTT means no MQTT, and a SetOption3 without an answer counts as "on" (a missing answer must not hide
// a configured host). strict (the re-read, which runs unattended and proves nothing first) never
// guesses: such a Status 6 answer is errNoMQTTAnswer (minimal or safeboot right now, a build without
// MQTT), and a SetOption3 without an answer is an error, so a half-done read stores nothing.
func (p *Prober) readMQTT(ctx context.Context, t tasmota.Target, strict bool) (mq model.MQTTCapability, host string, port int, err error) {
	mq = model.MQTTNone
	raw, err := p.cmd(ctx, t, "Status 6")
	var s struct {
		StatusMQT *struct {
			MqttHost string
			MqttPort int
		}
	}
	switch {
	case err != nil && !errors.Is(err, tasmota.ErrUnknownCommand):
		return mq, "", 0, fmt.Errorf("status 6: %w", err)
	case err != nil || json.Unmarshal(raw, &s) != nil || s.StatusMQT == nil:
		if strict {
			return mq, "", 0, errNoMQTTAnswer
		}
		return mq, "", 0, nil
	case s.StatusMQT.MqttHost == "":
		return mq, "", 0, nil
	}
	host, port = s.StatusMQT.MqttHost, s.StatusMQT.MqttPort
	mq = model.MQTTOtherBroker
	if p.Broker != nil && p.Broker.Same(host, port) {
		mq = model.MQTTOurs
	}
	off, err := p.mqttSwitchedOff(ctx, t)
	switch {
	case err != nil && strict:
		return mq, host, port, fmt.Errorf("setoption3: %w", err)
	case err == nil && off:
		mq = model.MQTTNone
	}
	return mq, host, port, nil
}

// errNoMQTTAnswer: the device answered Status 6 without anything about MQTT (Unknown, or no StatusMQT).
var errNoMQTTAnswer = errors.New("status 6 says nothing about MQTT")

// mqttSwitchedOff reads SetOption3 (MQTT on/off); err when there is no usable answer.
func (p *Prober) mqttSwitchedOff(ctx context.Context, t tasmota.Target) (bool, error) {
	raw, err := p.cmd(ctx, t, "SetOption3")
	if err != nil {
		return false, err
	}
	var s struct{ SetOption3 string }
	if json.Unmarshal(raw, &s) != nil || s.SetOption3 == "" {
		return false, tasmota.ErrBadResponse
	}
	return strings.EqualFold(s.SetOption3, "OFF"), nil
}

func (p *Prober) status5(ctx context.Context, t tasmota.Target, out *probed) (model.NetFacts, error) {
	raw, err := p.cmd(ctx, t, "Status 5")
	if err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", err)
	}
	return parseStatus5(raw, out)
}

// parseStatus5 reads MAC, Ethernet MAC and hostname into out and returns the network facts
// (address, gateway, subnet mask and DNS servers of the active interface).
func parseStatus5(raw json.RawMessage, out *probed) (model.NetFacts, error) {
	var s struct {
		StatusNET struct {
			Hostname                           string
			IPAddress                          string
			Gateway                            string
			Subnetmask, DNSServer1, DNSServer2 string
			Mac                                string
			Ethernet                           struct{ Hostname, IPAddress, Gateway, Subnetmask, DNSServer1, DNSServer2, Mac string }
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", tasmota.ErrBadResponse)
	}
	out.mac = strings.ToLower(s.StatusNET.Mac)
	if eth := strings.ToLower(s.StatusNET.Ethernet.Mac); eth != "" && eth != out.mac && eth != "00:00:00:00:00:00" {
		out.ethMAC = eth
	}
	// The hostname is the device's own text (#747): cleaned like discovery cleans it, since it ends
	// up in notifications and on the pages.
	out.hostname = inventory.CleanName(s.StatusNET.Hostname)
	n := model.NetFacts{Hostname: out.hostname, IP: s.StatusNET.IPAddress, Gateway: s.StatusNET.Gateway}
	n.Subnet, n.DNS = netMask(s.StatusNET.Subnetmask), dnsServers(s.StatusNET.DNSServer1, s.StatusNET.DNSServer2)
	// An Ethernet build whose Wi-Fi station has no address (0.0.0.0) is wired: the top-level
	// values are the idle Wi-Fi ones, the Ethernet object has the real ones.
	if eth := s.StatusNET.Ethernet; unsetIP(n.IP) && !unsetIP(eth.IPAddress) {
		n.Wired, n.IP, n.Gateway = true, eth.IPAddress, eth.Gateway
		n.Subnet, n.DNS = netMask(eth.Subnetmask), dnsServers(eth.DNSServer1, eth.DNSServer2)
		if h := inventory.CleanName(eth.Hostname); h != "" {
			n.Hostname = h
		}
	}
	return n, nil
}

// netMask is a subnet mask as reported, "" when unset.
func netMask(m string) string {
	if unsetIP(m) {
		return ""
	}
	return m
}

// dnsServers joins the DNS servers a device has set ("a, b"); an unset one (0.0.0.0) is left out.
func dnsServers(servers ...string) string {
	var set []string
	for _, s := range servers {
		if !unsetIP(s) {
			set = append(set, s)
		}
	}
	return strings.Join(set, ", ")
}

// withStoredWifi completes the facts of a Status 5 read with the Wi-Fi facts stored earlier
// (Status 11 is read only in the background); a wired board is on no access point.
func (p *Prober) withStoredWifi(stored, n model.NetFacts) model.NetFacts {
	if !n.Wired {
		n.SSID, n.BSSID, n.Channel = stored.SSID, stored.BSSID, stored.Channel
	}
	n.ReadAt = p.now()
	return n
}

// unsetIP is an address Tasmota reports for an interface that has none.
func unsetIP(ip string) bool { return ip == "" || ip == "0.0.0.0" }

// status11 reads the access point the device is connected to. It is best effort: a failed or
// empty reply (a wired board, a flaky answer) keeps the Wi-Fi facts already in net.
func (p *Prober) status11(ctx context.Context, t tasmota.Target, net *model.NetFacts) {
	raw, err := p.cmd(ctx, t, "Status 11")
	if err != nil {
		return
	}
	var s struct {
		StatusSTS struct {
			Wifi struct {
				SSId, BSSId string
				Channel     int
			}
		}
	}
	if json.Unmarshal(raw, &s) != nil || s.StatusSTS.Wifi.SSId == "" {
		return
	}
	w := s.StatusSTS.Wifi
	net.SSID, net.BSSID, net.Channel = w.SSId, w.BSSId, w.Channel
}

// ReadNet reads a stored device's network facts (Status 5, then best effort Status 11, one at a
// time, never Status 0) and stores them. It runs in the background (netFactsReader), never as
// part of first contact or a refresh. Minimal firmware and the safeboot image answer neither command: nothing is read.
func (p *Prober) ReadNet(ctx context.Context, id model.DeviceID) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	if d.Identity.Variant == "minimal" || d.Identity.Variant == "safeboot" { // #558
		return nil
	}
	t := tasmota.TargetFor(d)
	rp := p // the same prober, reading through the router
	if p.Reads != nil {
		c := *p
		c.Client = p.Reads
		rp = &c
	}
	var out probed
	tctx, check := transport.WithTrace(ctx)
	net, err := rp.status5(tctx, t, &out) // the check, and the read of the facts it vouches for
	if err != nil {
		return err
	}
	if d.MAC != "" && !d.HasMAC(out.mac) && !d.HasMAC(out.ethMAC) {
		// An answer over MQTT comes from whoever listens on the topic (and over HTTP from whoever holds
		// the address): facts of another device must not be stored on this one. Tasmota always reports
		// its MAC in Status 5, so an answer without one is refused too. The rule is a refresh's
		// (checkMAC, #65): a device stored without a MAC has nothing to compare. Answered on the topic
		// alone, it is the finding of every other check there: the router marks the device (#852).
		if slices.Equal(check.Answered(), []transport.Path{transport.MQTT}) {
			return p.noteTopic(d, errAnotherDevice)
		}
		return fmt.Errorf("status 5: the answer is from another device than the stored one")
	}
	net = p.withStoredWifi(d.Net, net) // kept when Status 11 gives none
	if !net.Wired {
		rp.status11(pinTo(ctx, check), t, &net) // where Status 5 was answered, never past it (#810)
	}
	if err := p.Devices.UpdateNet(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, net); err != nil {
		return err
	}
	p.announce(id)
	return nil
}

// errAnotherDevice: the device that answers at the stored address is not the stored one (its MAC is not in
// the stored set), or the answer has no MAC at all. Minimal firmware, which knows no Status 5, is
// settings.ErrNoMAC.
var errAnotherDevice = errors.New("another device answers at the stored address")

// errAnotherDeviceOnTopic is errAnotherDevice where the check was answered over MQTT only (verifyAndPin, VerifyRead,
// the identity probe): another device listens on the stored topic, not on the address. It is errors.Is
// errAnotherDevice for every caller; the broker reader and the data refresh log it louder, naming the topic (errors.As
// an anotherOnTopic), since the device's commands over MQTT reach that topic, and the router is told (noteTopic, #852).
var errAnotherDeviceOnTopic error = anotherOnTopic{}

type anotherOnTopic struct{ topic string }

func (e anotherOnTopic) Error() string {
	return "another device answers on the stored MQTT topic " + e.topic
}
func (anotherOnTopic) Is(target error) bool {
	return target == errAnotherDevice || target == errAnotherDeviceOnTopic
}

// noteTopic tells the router what a check answered over MQTT alone showed about d's topic (#852) and returns its
// result: another MAC marks the device (its commands, writes and transfers stop going to that topic, Choose) and is
// errAnotherDeviceOnTopic naming the topic. Any other result changes nothing, one of the device's own MACs included:
// a check that hears one reply hears whichever of two devices on the topic answers first, so it cannot show that the
// other device left (only recheckTopic, which hears every reply, ends a mark). Only for a check whose trace answered
// over MQTT alone, with d.Topic the topic the router asked (withStoredTopic).
func (p *Prober) noteTopic(d model.Device, err error) error {
	if !errors.Is(err, errAnotherDevice) {
		return err
	}
	if p.Topics != nil {
		p.Topics.Mark(d.ID, d.Topic)
	}
	return anotherOnTopic{topic: d.Topic}
}

// withStoredTopic returns d with the topic stored now. The router reads the device again before every send over MQTT
// (Router.read, pinnedMQTT, publish), so a check there is answered on the stored topic, which may differ from the
// caller's snapshot (a broker reader's list, an edit or a refresh since); a mark belongs to the topic the check asked
// (#852). Without the router or the marks nothing depends on it.
func (p *Prober) withStoredTopic(ctx context.Context, d model.Device) (model.Device, error) {
	if p.Reads == nil || p.Topics == nil || p.Devices == nil {
		return d, nil
	}
	now, err := p.Devices.GetDevice(ctx, d.ID)
	if err != nil {
		return d, err
	}
	d.Topic = now.Topic
	return d, nil
}

// topicMarked says whether the router holds d's topic as taken by another device (#852).
func (p *Prober) topicMarked(d model.Device) bool {
	return p.Topics != nil && d.Topic != "" && !p.Topics.Since(d.ID, d.Topic).IsZero()
}

// topicHeard is what the check that hears every reply on a device's topic learnt (hearTopic).
type topicHeard struct {
	ctx   context.Context // pinned to MQTT for the reads it vouches for (pinnedToMQTTCheck) when ok; the caller's otherwise
	asked model.Device    // the stored device the router asked, its topic the one asked
	reply probed          // the one reply's MAC, Ethernet MAC and hostname
	net   model.NetFacts  // the one reply's network facts
}

// hearTopic asks Status 5 on d's stored topic and hears every reply within the router's identity window
// (Router.TopicStatus5, the rule of a restore over MQTT, #852): exactly one reply, with one of the stored MACs, shows
// the device alone on its topic (ok, h.ctx pinned to MQTT) and ends a mark it had there; a second reply or another MAC
// marks it (errAnotherDeviceOnTopic). Nothing heard, a window cut short, MQTT not usable, or a single reply without a
// MAC (minimal firmware) is not ok: nothing is known, and a mark stays. It is the identity probe's check over MQTT
// (probeOverMQTT, #682) and the check that may end a mark (recheckTopic).
func (p *Prober) hearTopic(ctx context.Context, tc topicChecker, d model.Device) (h topicHeard, ok bool, err error) {
	h.ctx = ctx
	asked, replies, err := tc.TopicStatus5(ctx, d.ID)
	h.asked = asked
	if err != nil {
		slog.DebugContext(ctx, "topic check: nothing heard on the topic", "device", d.ID, "err", err)
		return h, false, nil
	}
	if len(replies) == 0 || asked.Topic == "" || asked.MAC == "" {
		return h, false, nil
	}
	if len(replies) == 1 {
		n, perr := parseStatus5(replies[0], &h.reply)
		if perr != nil || (h.reply.mac == "" && h.reply.ethMAC == "") {
			return h, false, nil //nolint:nilerr // a reply without a MAC (minimal firmware) says nothing about the topic
		}
		if asked.HasMAC(h.reply.mac) || asked.HasMAC(h.reply.ethMAC) {
			if p.Topics != nil {
				p.Topics.Clear(asked.ID, asked.Topic)
			}
			h.ctx, h.net = pinnedToMQTTCheck(ctx), n
			return h, true, nil
		}
	}
	if p.Topics != nil {
		p.Topics.Mark(asked.ID, asked.Topic)
	}
	return h, true, anotherOnTopic{topic: asked.Topic}
}

// recheckTopic is the check of a device the router holds off its topic (#852): hearTopic, which alone may end the mark.
// It returns ctx pinned to MQTT when the device answered alone there (ok), errAnotherDeviceOnTopic while another device
// still answers, and not ok when nothing was learnt (the mark stays). Only marked devices and the identity probe pay
// the window.
func (p *Prober) recheckTopic(ctx context.Context, d model.Device) (context.Context, bool, error) {
	tc, ok := p.Reads.(topicChecker)
	if !ok {
		return ctx, false, nil // nothing can hear every reply: the mark stays
	}
	h, ok, err := p.hearTopic(ctx, tc, d)
	return h.ctx, ok, err
}

// topicUnchecked is the refusal of a read for a device held off its topic (#852) when the check that hears every reply
// there learnt nothing (MQTT not usable, no reply, a window cut short, no MAC): a check that hears one reply hears
// whichever device answers first, so nothing is read from the topic.
func topicUnchecked(topic string) error {
	return fmt.Errorf("%w: the device's MQTT topic %s is held as taken by another device and could not be checked; nothing was read there", tasmota.ErrUnreachable, topic)
}

// recheckMarked is recheckTopic for the broker reader's own schedule of marked devices (#852, recheckTopics): it reads
// the stored topic first (withStoredTopic: the mark belongs to the topic the router asks) and asks it only while the
// device is marked there. nil: the mark ended, or nothing was learnt and it stays; anotherOnTopic: another device still
// answers on the topic.
func (p *Prober) recheckMarked(ctx context.Context, d model.Device) error {
	d, err := p.withStoredTopic(ctx, d)
	if err != nil {
		return err
	}
	if !p.topicMarked(d) {
		return nil
	}
	_, _, err = p.recheckTopic(ctx, d)
	return err
}

// notStoredDevice says err is VerifyAddress's refusal: another device answers (errAnotherDevice), or the
// answer shows no MAC at all, so it cannot be told (settings.ErrNoMAC, minimal firmware). Neither is a
// failure of the read: nothing was stored, and the device is asked again at the regular interval.
func notStoredDevice(err error) bool {
	return errors.Is(err, errAnotherDevice) || errors.Is(err, settings.ErrNoMAC)
}

// VerifyAddress says, with one Status 5, whether the device answering at the stored address is the stored
// one, by the rule of ReadNet and the refresh (a MAC of the stored set). The background reads (#600) call
// it before they read anything that is stored on, or shown for, the device: an address a DHCP lease handed
// to another device must not fill this device's broker, Settings values, policy values or template reading.
// A device stored without a MAC has nothing to compare (first contact). Silent: the refresh audits a refusal,
// this check runs often. Any other error (not reachable, a timeout) is the read's own failure.
func (p *Prober) VerifyAddress(ctx context.Context, d model.Device) error {
	if d.MAC == "" {
		return nil
	}
	var out probed
	if _, err := p.status5(ctx, tasmota.TargetFor(d), &out); err != nil {
		if errors.Is(err, tasmota.ErrUnknownCommand) {
			return settings.ErrNoMAC // minimal firmware reports no MAC: it cannot show it is the stored device
		}
		return err
	}
	if !d.HasMAC(out.mac) && !d.HasMAC(out.ethMAC) {
		return errAnotherDevice
	}
	return nil
}

// A check vouches for the transport its read used, and for no other (#810): over HTTP the answer comes from whoever
// holds the stored address, over MQTT from whoever listens on the stored topic, and a DHCP lease handed to another
// device makes the two different devices. The reads that go through the router may fall back from MQTT to HTTP (MQTT
// published nothing, or nobody answered), and a check routed on its own may fall back too, so a read and its check
// left to the router can go different ways. Every caller follows one of two forms, which are the only two ways in:
//   - the check after the reads (VerifyRead): the caller records its reads in a transport.WithTrace and hands that
//     trace over; the check is asked pinned to each transport that answered the reads;
//   - the check before the reads (verifyAndPin: the data refresh and the broker read; ReadNet, whose Status 5 is its
//     check): the check is routed, and the reads are pinned to the transport that answered it (pinTo).

// VerifyRead is VerifyAddress after reads through Reads (the router: MQTT first for a device on our broker, #456),
// for the values those reads returned: the Settings service and the policy poller read through the router, so
// their check must go the way their reads went, like ReadNet's. Over HTTP alone it would vouch for whoever holds the
// address while the values came from the device on the topic, and it refused every read of a device whose HTTP API
// is off (#662). read is the trace of the reads it vouches for: the check is asked pinned to each transport that
// answered them (no fallback), so it is answered where they were (#810); a read answered over both is checked over
// both. A transport that carried a read but answered none (MQTT published it, nobody answered, the read went on
// over HTTP) gave no value and is not asked. Through the router a trace in which nothing was answered refuses: its
// reads were made with another context, or brought nothing to vouch for. ReadBroker asks its check before its reads
// (verifyAndPin, #839).
//
// Inside a data refresh whose first check the stored device answered over MQTT (pinnedToMQTTCheck) the checks after
// reads answered over MQTT only are not asked again (#676): every read of that refresh is pinned to MQTT, so it
// reached the topic the first check vouched for, and the refresh asked the same question up to three times (before the
// reads, after the Settings read, after the policy read). Over HTTP every check stays: the answer comes from whoever
// holds the address, and a lease handed to another device between two reads is what the checks after the reads are
// for (#662).
func (p *Prober) VerifyRead(ctx context.Context, d model.Device, read *transport.Trace) error {
	if read == nil {
		return errors.New("identity check: no trace of the read it vouches for") // a caller's mistake: fail closed
	}
	if p.Reads == nil {
		// no router: the reads went to the address by this prober's client, which records nothing ("http")
		if read.Via() != "http" {
			return errors.New("identity check: the read went over MQTT and this prober has no router to ask there")
		}
		return p.VerifyAddress(ctx, d)
	}
	paths := read.Answered()
	if len(paths) == 0 {
		return errors.New("identity check: the trace holds no read it could vouch for") // a caller's mistake: fail closed
	}
	if checked, _ := ctx.Value(mqttCheckedKey{}).(bool); checked && slices.Equal(paths, []transport.Path{transport.MQTT}) {
		return nil
	}
	d, err := p.withStoredTopic(ctx, d)
	if err != nil {
		return err
	}
	c := *p
	c.Client = p.Reads
	for _, path := range paths {
		err := c.VerifyAddress(transport.WithPath(ctx, path), d)
		if path == transport.MQTT {
			err = p.noteTopic(d, err) // answered on the topic: another MAC there marks it (#852); one reply never clears
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// verifyAndPin asks the check before the reads, through Reads like VerifyRead, and returns ctx pinned to the
// transport the check went over, so every read made with it is one the check vouched for (#676, #810): after an
// answer over MQTT, MQTT for the topic (pinnedToMQTTCheck: no read falls back to HTTP, where another device may hold
// the address, and the checks after MQTT reads are not asked again); otherwise HTTP for the address, where the checks
// after the reads are asked again (#662). A device without a stored MAC sends nothing here: HTTP. Another MAC in an
// answer over MQTT only is errAnotherDeviceOnTopic, and the router marks the device (#852) on the stored topic, the
// one the router asks (withStoredTopic). A marked device's reads go over HTTP, so the routed check would never ask the
// topic again: for it the check asks the topic first and hears every reply there (recheckTopic), which
// alone can end the mark, and only when nothing is learnt there does the routed check follow.
func (p *Prober) verifyAndPin(ctx context.Context, d model.Device) (context.Context, error) {
	d, err := p.withStoredTopic(ctx, d)
	if err != nil {
		return ctx, err
	}
	marked := p.topicMarked(d)
	if marked {
		if pctx, ok, err := p.recheckTopic(ctx, d); ok {
			return pctx, err
		}
	}
	c := *p
	if p.Reads != nil {
		c.Client = p.Reads
	}
	tctx, trace := transport.WithTrace(ctx)
	err = c.VerifyAddress(tctx, d)
	if slices.Equal(trace.Answered(), []transport.Path{transport.MQTT}) {
		err = p.noteTopic(d, err)
		if err == nil && marked {
			// The check that hears every reply learnt nothing and HTTP failed, so the routed check went to the topic
			// and heard one reply: whichever device answered first. It vouches for nothing there (#852).
			return ctx, topicUnchecked(d.Topic)
		}
	}
	if err != nil {
		return ctx, err
	}
	pctx := pinTo(ctx, trace)
	if pin, _ := transport.PathFrom(pctx); pin == transport.MQTT {
		return pinnedToMQTTCheck(pctx), nil
	}
	return pctx, nil
}

// pinTo returns ctx pinned to the transport that answered the check recorded in check: MQTT when only MQTT
// answered it, otherwise HTTP (an answer at the address, or nothing recorded: a prober without a router).
func pinTo(ctx context.Context, check *transport.Trace) context.Context {
	if slices.Equal(check.Answered(), []transport.Path{transport.MQTT}) {
		return transport.WithPath(ctx, transport.MQTT)
	}
	return transport.WithPath(ctx, transport.HTTP)
}

type mqttCheckedKey struct{}

// pinnedToMQTTCheck pins every later call of a data refresh to MQTT (no fallback to HTTP, where another device may
// hold the address: a read MQTT leaves unanswered fails the refresh) and lets VerifyRead rely on the refresh's first
// check, which the stored device answered over MQTT. Both in one place: the check is skipped only where nothing can
// go over HTTP. It belongs to one refresh of one device: nothing outside it (a page open, another visit) sees it.
func pinnedToMQTTCheck(ctx context.Context) context.Context {
	return context.WithValue(transport.WithPath(ctx, transport.MQTT), mqttCheckedKey{}, true)
}

// ReadBroker re-reads the broker a stored device is configured for (Status 6, and SetOption3 when a
// host is set; never Status 0, one command at a time) and stores host, port and capability when they
// differ from what is stored (#598). Why: the host is otherwise only stored by a probe, so a device
// moved to another broker outside TasmoFleet kept the old one and the "Other broker" hint (#580)
// stayed away. Callers hold the device lock (brokerHostReader). Minimal firmware and the safeboot
// image answer neither command: nothing is read, and nothing is stored when the device answers like
// them while its stored identity still says regular firmware (an update outside a job). A read that
// fails half way stores nothing either (readMQTT strict).
//
// It reads through Reads, the router, like every other background read (#839): MQTT first for a device usable on
// TasmoFleet's broker, HTTP when MQTT is not usable or does not answer. The check comes first (verifyAndPin: routed,
// the reads pinned to the transport that answered it, #810), so the broker of another device is never stored on
// this one: over HTTP that is whoever holds the address, over MQTT whoever listens on the topic. A device that moved
// to another broker after a clean disconnect keeps a retained "Online" will on ours; its check goes unanswered over
// MQTT, the router asks HTTP once, and the reads follow the check there.
func (p *Prober) ReadBroker(ctx context.Context, id model.DeviceID) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	if d.Identity.Variant == "minimal" || d.Identity.Variant == "safeboot" {
		return nil
	}
	ctx, err = p.verifyAndPin(ctx, d) // the host of another device is not this device's
	if err != nil {
		return err
	}
	rp := *p // the same prober, reading through the router where the check was answered
	if p.Reads != nil {
		rp.Client = p.Reads
	}
	c, host, port, err := rp.readMQTT(ctx, tasmota.TargetFor(d), true)
	if errors.Is(err, errNoMQTTAnswer) {
		return nil // nothing about the broker in the answer: the stored one stays
	}
	if err != nil {
		return err
	}
	// A device stored as "ours" stays so when the read only says "other" (store.UpdateMQTTBroker).
	sameCap := c == d.MQTT || (d.MQTT == model.MQTTOurs && c == model.MQTTOtherBroker)
	if host == d.MQTTHost && port == d.MQTTPort && sameCap {
		return nil // nothing moved: no write, no event
	}
	if err := p.Devices.UpdateMQTTBroker(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, c, host, port); err != nil {
		return err
	}
	p.announce(id)
	return nil
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Describe fills MAC, topic, name, MQTT capability and identity of d from the
// device. Nothing is stored. Fields the user typed are kept.
func (p *Prober) Describe(ctx context.Context, d model.Device) (model.Device, error) {
	pr, err := p.probe(ctx, d, nil)
	if err != nil {
		return d, err
	}
	d.Identity = pr.identity
	if d.MAC == "" {
		d.MAC = pr.mac
		if pr.ethMAC != "" && pr.mac != "" && len(d.MACs) == 0 {
			d.MACs = []string{pr.mac, pr.ethMAC} // Wi-Fi plus Ethernet, set once at creation
		}
	}
	if d.Topic == "" {
		d.Topic = pr.topic
	}
	if d.MQTT == model.MQTTUnknown {
		d.MQTT = pr.mqtt
	}
	d.MQTTHost, d.MQTTPort = pr.mqttHost, pr.mqttPort
	if pr.readOTAURL {
		d.OTAURL, d.OTAURLReadAt = pr.otaURL, p.now()
	}
	if pr.readGroupTopic {
		d.GroupTopic, d.GroupTopicReadAt = pr.groupTopic, p.now()
	}
	if pr.relayNames != nil {
		d.RelayNames = pr.relayNames
	}
	if d.Name == "" {
		d.Name = inventory.CleanName(firstNonEmpty(pr.devName, pr.hostname, pr.topic, d.Address))
	}
	return d, nil
}

// Refresh re-probes a stored device and stores what it reports (#91): identity, MQTT facts, the
// network facts of Status 5, the DeviceName and the topic (the stored name and topic follow them
// unless the user chose their own, store.ProbeResult). The device must answer with a MAC of the
// stored set, else the refresh is refused (RefreshRefusedError, audited) before anything else
// is sent and nothing is written (#65). A device without a MAC gets the one it reported bound
// when an admin refreshes it (actor not empty); an automatic refresh (actor "", discovery) never
// binds (discovery D8). A device on our broker is read over MQTT first once the check that hears every reply on its
// topic heard it alone with a stored MAC (probe, #682), at its address otherwise. On minimal firmware the stored
// flash values are kept and no MAC can be read.
func (p *Prober) Refresh(ctx context.Context, id model.DeviceID, actor string) error {
	return p.refresh(ctx, id, actor, false)
}

// RefreshFacts is the refresh the background schedule runs (#600): the same probe, MQTT first (#682), and the same
// facts (identity, MQTT facts, network facts, relay names), but nothing the person sees or relies on is renamed on its
// own: the stored name and topic do not follow the DeviceName and topic the device reports (an explicit Refresh,
// discovery and the first probe still do), and no MAC is bound.
func (p *Prober) RefreshFacts(ctx context.Context, id model.DeviceID) error {
	return p.refresh(ctx, id, "", true)
}

func (p *Prober) refresh(ctx context.Context, id model.DeviceID, actor string, factsOnly bool) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	pr, err := p.probe(ctx, d, func(pr probed) error { return p.checkMAC(ctx, d, pr, actor, pr.via()) })
	if err != nil {
		return err
	}
	res := store.ProbeResult{Identity: pr.identity, SetMQTT: !pr.minimal, MQTT: pr.mqtt, MQTTHost: pr.mqttHost, MQTTPort: pr.mqttPort}
	if pr.minimal {
		res.Identity.FlashSizeKB, res.Identity.ProgramFlashSizeKB, res.Identity.FreeKB = d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB
	} else {
		res.DeviceName, res.DeviceTopic, res.NameFrom = pr.devName, pr.topic, fallbackName(d, pr)
		res.SetOTAURL, res.OTAURL, res.RelayNames = pr.readOTAURL, pr.otaURL, pr.relayNames
		res.SetGroupTopic, res.GroupTopic = pr.readGroupTopic, pr.groupTopic
		res.SetNet, res.Net = true, p.withStoredWifi(d.Net, pr.net)
		if d.MAC == "" && actor != "" && bindable(pr.mac) {
			res.BindMACs = []string{pr.mac}
			if bindable(pr.ethMAC) {
				res.BindMACs = append(res.BindMACs, pr.ethMAC)
			}
		}
	}
	if factsOnly {
		res.DeviceName, res.DeviceTopic, res.NameFrom = "", "", "" // "" keeps the recorded DeviceName and the stored name and topic
	}
	// Only probe-owned fields are written, and only if the device is still
	// the one that was probed (a slow probe must not undo an admin edit or
	// land on a device that was moved, rebound or deleted meanwhile). A probe over MQTT also needs the topic it asked:
	// its answers came from whoever listens there.
	err = p.Devices.UpdateProbe(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC, Topic: pr.checkedTopic}, res)
	var topicKept error
	if err == nil && res.DeviceTopic != "" {
		topicKept = p.auditTopic(ctx, d, res.DeviceTopic, actor)
	}
	switch {
	case errors.Is(err, store.ErrMACInUse):
		return p.refuse(ctx, d, actor, pr.via(), pr.mac, refusedTaken)
	case err != nil:
		return err
	}
	if len(res.BindMACs) > 0 {
		p.record(ctx, actor, "device.mac_bound", d.ID, "ok", map[string]any{"mac": pr.mac, "ethernet_mac": pr.ethMAC, "address": d.Address, "via": pr.via()})
	}
	if pr.checkedTopic != "" && !factsOnly {
		// A refresh a person or discovery started that read the device over MQTT, as each of its questions there was
		// audited before (identity.mqtt_ask); the daily background refresh is not.
		p.record(ctx, actor, "identity.mqtt_ask", d.ID, "ok", map[string]any{"topic": pr.checkedTopic, "via": "mqtt", "reason": "refresh identity"})
	}
	p.announce(id)
	if p.ReadNetLater != nil && !pr.minimal {
		p.ReadNetLater(id) // the Wi-Fi facts of Status 11 fill in when they arrive
	}
	return topicKept
}

// TopicKeptError is returned by a refresh that succeeded except for one thing: the device reports
// a topic another device in the inventory already has, so the stored topic stays and MQTT
// messages of this device cannot be matched (#111). Callers that only care about success ignore it.
type TopicKeptError struct {
	Reported string // the topic the device reports
	Stored   string // the topic that stays
}

func (e *TopicKeptError) Error() string {
	return "topic not changed: the device reports " + e.Reported + ", which another device already has"
}

// Notice is the sentence for the device page.
func (e *TopicKeptError) Notice() string {
	return "The device reports the MQTT topic " + e.Reported + ", which another device in the inventory already has, " +
		"so the stored topic " + e.Stored + " was kept and MQTT will not match this device until the topics differ."
}

// auditTopic compares the topic stored by UpdateProbe with the one before the refresh and audits
// a change (device.topic_changed) or a topic that could not follow (device.topic_kept, returned
// as TopicKeptError). A failed read of the device only skips the audit.
func (p *Prober) auditTopic(ctx context.Context, before model.Device, reported, actor string) error {
	after, err := p.Devices.GetDevice(ctx, before.ID)
	if err != nil {
		slog.WarnContext(ctx, "refresh: reload device for the topic audit", "device", before.ID, "err", err)
		return nil
	}
	switch {
	case after.Topic != reported:
		p.record(ctx, actor, "device.topic_kept", before.ID, "refused", map[string]any{"stored": after.Topic, "reported": reported})
		return &TopicKeptError{Reported: reported, Stored: after.Topic}
	case before.Topic != after.Topic:
		p.record(ctx, actor, "device.topic_changed", before.ID, "ok", map[string]any{"old": before.Topic, "new": after.Topic, "via": "http"})
	}
	return nil
}

// bindable reports whether mac is a real MAC (normal form, not all zero) that may be bound.
func bindable(mac string) bool {
	n, err := inventory.NormaliseMAC(mac)
	return err == nil && n != "" && n == mac && n != "00:00:00:00:00:00"
}

// tasmotaDefaultName is the DeviceName of a Tasmota whose name was never set (its FriendlyName1).
const tasmotaDefaultName = "Tasmota"

// fallbackName returns the stored name when it is one tasmofleet filled in itself before it
// recorded the DeviceName: the hostname (how devices were named before 0.12.3), the topic or the
// address (Describe's fallbacks). Such a name follows the DeviceName (#91, refining #52), unless
// the DeviceName is Tasmota's default, which would give many devices the same name. The default
// itself as a stored name is one tasmofleet filled in (Describe, #104) and follows too.
func fallbackName(d model.Device, pr probed) string {
	if pr.devName == "" || pr.devName == tasmotaDefaultName || d.Name == "" {
		return ""
	}
	if d.Name == tasmotaDefaultName {
		return d.Name // Describe named a device after its default DeviceName (#104)
	}
	for _, s := range []string{pr.hostname, d.Net.Hostname, pr.topic, d.Topic, d.Address} {
		if s != "" && d.Name == inventory.CleanName(s) {
			return d.Name
		}
	}
	return ""
}

// Reasons of a refused refresh.
const (
	refusedOther = "another MAC"
	refusedTaken = "the MAC of another device"
	// refusedUnverified: minimal firmware reports no MAC, and the device has one stored.
	refusedUnverified = "no MAC (minimal firmware)"
)

// RefreshRefusedError says that the device that answered is not the stored one (#65). Nothing was
// written. The web layer shows Refusal() on the device page.
type RefreshRefusedError struct {
	Reported string // the MAC the device reported, lower case ("" when it reported none)
	Reason   string // refusedOther or refusedTaken
}

func (e *RefreshRefusedError) Error() string {
	return "identity refresh refused: the device reports " + e.Reason + " (" + e.Reported + ")"
}

// Refusal is the sentence for the device page.
func (e *RefreshRefusedError) Refusal() string {
	mac := "MAC " + e.Reported
	if e.Reported == "" {
		mac = "no MAC"
	}
	if e.Reason == refusedUnverified {
		return "The device runs minimal firmware, which does not report its MAC, so TasmoFleet cannot check that it is this device. " +
			"The stored identity was kept. Refresh again once the update has finished."
	}
	if e.Reason == refusedTaken {
		return "The device at this address reports " + mac + ", which belongs to another device in the inventory. Nothing was changed."
	}
	return "The device at this address reports " + mac + ", not the MAC stored for this device, so it is another device. " +
		"Nothing was changed and nothing more was sent to it. If the device was replaced, use Replace device; " +
		"if the address now belongs to another device, correct it on the Edit page."
}

// checkMAC refuses a device that reports none of the stored MACs. Nothing to compare: a device
// stored without a MAC. Minimal firmware (no Status 5) cannot prove a stored MAC, so its answers
// are not taken as the stored device's either: refused, the stored identity stays (#96 review).
func (p *Prober) checkMAC(ctx context.Context, d model.Device, pr probed, actor, via string) error {
	switch {
	case d.MAC == "" || d.HasMAC(pr.mac) || d.HasMAC(pr.ethMAC):
		return nil
	case pr.minimal:
		return p.refuse(ctx, d, actor, via, "", refusedUnverified)
	}
	return p.refuse(ctx, d, actor, via, pr.mac, refusedOther)
}

// refuse audits a refused refresh and returns its error.
func (p *Prober) refuse(ctx context.Context, d model.Device, actor, via, reported, reason string) error {
	slog.WarnContext(ctx, "identity refresh refused", "device", d.ID, "address", d.Address, "reason", reason, "via", via)
	p.record(ctx, actor, "device.refresh_refused", d.ID, "refused", map[string]any{
		"address": d.Address, "stored_mac": d.MAC, "reported_mac": reported, "reason": reason, "via": via,
	})
	return &RefreshRefusedError{Reported: reported, Reason: reason}
}

// record writes one audit row. Details are addresses, MACs and reasons, never credentials.
func (p *Prober) record(ctx context.Context, actor, action string, id model.DeviceID, result string, detail map[string]any) {
	if p.Audit == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}") // strings only; cannot happen
	}
	p.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: action, Target: "device:" + strconv.FormatInt(int64(id), 10), Detail: string(b), Result: result})
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// macVerifier implements inventory.Verifier: Status 5 without credentials,
// IP only, so a rogue host never receives a stored password.
type macVerifier struct{ client tasmota.Client }

func (v macVerifier) VerifyMAC(ctx context.Context, address string, port int) (string, error) {
	t := tasmota.TargetFor(model.Device{Address: address, Port: port}) // no user, no password
	raw, err := v.client.Command(ctx, t, "Status 5")
	if err != nil {
		return "", err
	}
	var s struct {
		StatusNET struct{ Mac string }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
		return "", tasmota.ErrBadResponse
	}
	return strings.ToLower(s.StatusNET.Mac), nil
}
