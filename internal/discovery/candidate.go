package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
)

// candidate is a device claim from the network, already sanitised. Nothing in
// it is trusted: the MAC is self-asserted, and name and topic are free text.
type candidate struct {
	via       string
	mac       string
	macs      []string // further MACs of the same device (Ethernet), from a scan or a Status 5 reply
	ip        netip.Addr
	port      int // 0: unknown (MQTT), the stored port is kept
	name      string
	host      string // the hostname the device reported ("" when it did not), display only
	topic     string
	fullTopic string
}

const (
	maxDiscoveryPayload = 16 << 10 // real messages are ~1.5 KiB
	maxNameRunes        = 100
	maxTopicLen         = 100
)

var errBadClaim = errors.New("discovery: unusable device claim")

func badClaim(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errBadClaim, fmt.Sprintf(format, a...))
}

// parseDiscovery decodes a tasmota/discovery/<MAC>/config payload. topicMAC is
// the MAC level of the MQTT topic; a payload that names another MAC is refused.
func parseDiscovery(topicMAC string, payload []byte) (candidate, error) {
	if len(payload) > maxDiscoveryPayload {
		return candidate{}, badClaim("payload of %d bytes", len(payload))
	}
	var p struct {
		IP  string `json:"ip"`
		DN  string `json:"dn"`
		HN  string `json:"hn"`
		MAC string `json:"mac"`
		T   string `json:"t"`
		FT  string `json:"ft"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return candidate{}, badClaim("not JSON")
	}
	mac, err := inventory.NormaliseMAC(p.MAC)
	if err != nil || mac == "" {
		return candidate{}, badClaim("no valid MAC")
	}
	if tm, err := inventory.NormaliseMAC(topicMAC); err != nil || tm != mac {
		return candidate{}, badClaim("topic and payload name different MACs")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(p.IP))
	if err != nil {
		return candidate{}, badClaim("no valid IP address")
	}
	c := candidate{via: ViaMQTT, mac: mac, ip: ip.Unmap(), topic: cleanTopic(p.T), fullTopic: cleanFullTopic(p.FT)}
	c.name = cleanName(firstNonEmpty(p.DN, p.HN, p.T), mac)
	c.host = cleanHostname(p.HN)
	return c, nil
}

// allMACs returns the claimed MACs, the main one first.
func (c candidate) allMACs() []string { return append([]string{c.mac}, c.macs...) }

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// cleanName drops control characters, trims and shortens a device-supplied
// name. An empty result becomes "tasmota-<last 6 hex digits of the MAC>".
func cleanName(s, mac string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) { // Cf: bidi overrides, zero-width characters
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxNameRunes {
		s = strings.TrimSpace(string(r[:maxNameRunes]))
	}
	if s == "" {
		s = "tasmota-" + strings.ReplaceAll(mac, ":", "")[6:]
	}
	return s
}

// cleanHostname is cleanName for the hostname column: an unreported hostname stays empty.
func cleanHostname(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return cleanName(s, "000000000000")
}

// cleanTopic returns t if the inventory would accept it as a topic, else "".
func cleanTopic(t string) string {
	t = strings.TrimSpace(t)
	if t == "" || len(t) > maxTopicLen || strings.ContainsAny(t, "#+/") || strings.IndexFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return ""
	}
	return t
}

// cleanFullTopic keeps only plausible FullTopic patterns.
func cleanFullTopic(ft string) string {
	ft = strings.TrimSpace(ft)
	if ft == "" || len(ft) > maxTopicLen || !strings.Contains(ft, "%topic%") || strings.IndexFunc(ft, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return ""
	}
	return ft
}
