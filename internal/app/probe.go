package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// identityStore is the part of the store the prober needs.
type identityStore interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	UpdateProbe(ctx context.Context, id model.DeviceID, g store.ProbeGuard, r store.ProbeResult) error
	UpdateNet(ctx context.Context, id model.DeviceID, g store.ProbeGuard, n model.NetFacts) error
}

// brokerMatcher tells whether a device's MQTT settings point at the broker
// tasmofleet uses (mqttbus.BrokerMatcher).
type brokerMatcher interface {
	Same(host string, port int) bool
}

// Prober learns a device's identity from individual Status queries (never
// Status 0, spec section 5), one at a time with a pause in between.
type Prober struct {
	Client  tasmota.Client
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

// probe queries the device. A device on minimal firmware only answers a few
// commands, so MAC, topic and MQTT capability stay empty there.
func (p *Prober) probe(ctx context.Context, d model.Device) (probed, error) {
	t := tasmota.TargetFor(d)
	var out probed
	in := fwselect.Inputs{}

	raw2, err := p.Client.Command(ctx, t, "Status 2")
	switch {
	case err == nil:
		in.Status2 = raw2
	case errors.Is(err, tasmota.ErrUnknownCommand):
		in.Status2, out.minimal = []byte(unknownReply), true
	default:
		return out, err
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
		if probe, err := tasmota.ParseUpgradeProbe(up); err == nil {
			in.Upgrade = &fwselect.UpgradeProbe{Version: probe.Version}
		}
	}
	if err := p.pause(ctx); err != nil {
		return out, err
	}
	root, err := p.Client.RootPage(ctx, t)
	if err != nil {
		if out.minimal {
			return out, fmt.Errorf("main page: %w", err)
		}
		// The language only comes from the page; the identity is usable without it.
	} else {
		in.Root = &fwselect.RootInfo{Version: root.Version, Image: root.Image, Lang: root.Lang, Minimal: root.Minimal}
	}
	if out.identity, err = fwselect.Detect(in); err != nil {
		return out, fmt.Errorf("identify: %w", err)
	}
	out.identity.DetectedAt = p.now()
	if out.minimal {
		return out, nil
	}

	// Status 5 gives the MAC the device is bound to. Its network facts and Status 11 are read
	// later, in the background (ReadNet): they must never hold up first contact or a refresh.
	if _, err := p.status5(ctx, t, &out); err != nil {
		return out, err
	}
	// Status 6: no MqttHost, or SetOption3 off, means the device does not
	// talk MQTT at all. Otherwise it is "ours" when it is configured for the
	// tasmofleet broker and "other" for any other target (or when tasmofleet
	// has no broker).
	out.mqtt = model.MQTTNone
	if raw, err := p.cmd(ctx, t, "Status 6"); err == nil {
		var s struct {
			StatusMQT struct {
				MqttHost string
				MqttPort int
			}
		}
		if json.Unmarshal(raw, &s) == nil && s.StatusMQT.MqttHost != "" {
			out.mqttHost, out.mqttPort = s.StatusMQT.MqttHost, s.StatusMQT.MqttPort
			out.mqtt = model.MQTTOtherBroker
			if p.Broker != nil && p.Broker.Same(out.mqttHost, out.mqttPort) {
				out.mqtt = model.MQTTOurs
			}
		}
	} else if !errors.Is(err, tasmota.ErrUnknownCommand) {
		return out, fmt.Errorf("status 6: %w", err)
	}
	if out.mqtt != model.MQTTNone && p.mqttSwitchedOff(ctx, t) {
		out.mqtt = model.MQTTNone
	}
	// Plain "Status" (not "Status 0") is the Status object only: DeviceName and the topic the
	// device actually uses. The "Topic" command would return the setting with its placeholders
	// ("tasmota_%06X") unresolved, and nothing on the bus ever matches that.
	if raw, err := p.cmd(ctx, t, "Status"); err == nil {
		var s struct {
			Status struct{ DeviceName, Topic string }
		}
		if json.Unmarshal(raw, &s) == nil {
			// Both come from the device: only a usable topic is taken, a name is cleaned.
			if inventory.ValidTopic(s.Status.Topic) {
				out.topic = s.Status.Topic
			}
			out.devName = inventory.CleanName(s.Status.DeviceName)
		}
	}
	return out, nil
}

// mqttSwitchedOff reads SetOption3 (MQTT on/off). Any failure counts as "on":
// the host is configured, and a missing answer must not hide it.
func (p *Prober) mqttSwitchedOff(ctx context.Context, t tasmota.Target) bool {
	raw, err := p.cmd(ctx, t, "SetOption3")
	if err != nil {
		return false
	}
	var s struct{ SetOption3 string }
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(s.SetOption3, "OFF")
}

func (p *Prober) status5(ctx context.Context, t tasmota.Target, out *probed) (model.NetFacts, error) {
	raw, err := p.cmd(ctx, t, "Status 5")
	if err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", err)
	}
	var s struct {
		StatusNET struct {
			Hostname  string
			IPAddress string
			Gateway   string
			Mac       string
			Ethernet  struct{ Hostname, IPAddress, Gateway, Mac string }
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return model.NetFacts{}, fmt.Errorf("status 5: %w", tasmota.ErrBadResponse)
	}
	out.mac = strings.ToLower(s.StatusNET.Mac)
	if eth := strings.ToLower(s.StatusNET.Ethernet.Mac); eth != "" && eth != out.mac && eth != "00:00:00:00:00:00" {
		out.ethMAC = eth
	}
	out.hostname = s.StatusNET.Hostname
	n := model.NetFacts{Hostname: s.StatusNET.Hostname, IP: s.StatusNET.IPAddress, Gateway: s.StatusNET.Gateway}
	// An Ethernet build whose Wi-Fi station has no address (0.0.0.0) is wired: the top-level
	// values are the idle Wi-Fi ones, the Ethernet object has the real ones.
	if eth := s.StatusNET.Ethernet; unsetIP(n.IP) && !unsetIP(eth.IPAddress) {
		n.Wired, n.IP, n.Gateway = true, eth.IPAddress, eth.Gateway
		if eth.Hostname != "" {
			n.Hostname = eth.Hostname
		}
	}
	return n, nil
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
// part of first contact or a refresh. Minimal firmware answers neither command: nothing is read.
func (p *Prober) ReadNet(ctx context.Context, id model.DeviceID) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	if d.Identity.Variant == "minimal" {
		return nil
	}
	t := tasmota.TargetFor(d)
	var out probed
	net, err := p.status5(ctx, t, &out)
	if err != nil {
		return err
	}
	net.SSID, net.BSSID, net.Channel = d.Net.SSID, d.Net.BSSID, d.Net.Channel // kept when Status 11 gives none
	if net.Wired {
		net.SSID, net.BSSID, net.Channel = "", "", 0 // a wired board is on no access point
	} else {
		p.status11(ctx, t, &net)
	}
	net.ReadAt = p.now()
	if err := p.Devices.UpdateNet(ctx, id, store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}, net); err != nil {
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
	pr, err := p.probe(ctx, d)
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
	if d.Name == "" {
		d.Name = inventory.CleanName(firstNonEmpty(pr.devName, pr.hostname, pr.topic, d.Address))
	}
	return d, nil
}

// Refresh re-probes a stored device and stores identity and MQTT facts. On minimal
// firmware the stored flash values are kept (Status 4 is not available).
func (p *Prober) Refresh(ctx context.Context, id model.DeviceID) error {
	d, err := p.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	pr, err := p.probe(ctx, d)
	if err != nil {
		return err
	}
	ident := pr.identity
	if pr.minimal {
		ident.FlashSizeKB, ident.ProgramFlashSizeKB, ident.FreeKB = d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB
	}
	// Only probe-owned fields are written, and only if the device is still
	// the one that was probed (a slow probe must not undo an admin edit or
	// land on a device that was moved, rebound or deleted meanwhile).
	if err := p.Devices.UpdateProbe(ctx, id,
		store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC},
		store.ProbeResult{Identity: ident, SetMQTT: !pr.minimal, MQTT: pr.mqtt, MQTTHost: pr.mqttHost, MQTTPort: pr.mqttPort,
			Topic: repairedTopic(d.Topic, pr.topic)}); err != nil {
		return err
	}
	p.announce(id)
	if p.ReadNetLater != nil && !pr.minimal {
		p.ReadNetLater(id) // the network facts fill in when they arrive
	}
	return nil
}

// repairedTopic is the topic to store over an unresolved one. Earlier versions stored the raw
// Topic setting ("tasmota_%06X"), which the health tracker could never match to a message: the
// device showed "MQTT: Not connected" while it was online. A topic the user typed is left alone.
func repairedTopic(stored, probed string) string {
	if strings.Contains(stored, "%") && probed != "" && !strings.Contains(probed, "%") {
		return probed
	}
	return ""
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
