package mqttbus

import (
	"regexp"
	"strings"
)

// Default prefixes and FullTopic of Tasmota.
const (
	DefaultFullTopic = "%prefix%/%topic%/"
	discoveryFilter  = "tasmota/discovery/+/config"
)

var (
	defaultPrefixes = [3]string{"cmnd", "stat", "tele"}
	statusSuffix    = regexp.MustCompile(`^STATUS[0-9]{0,2}$`) // Status 0..13
	powerSuffix     = regexp.MustCompile(`^POWER[0-9]{0,2}$`)  // POWER, POWER1..32
	infoSuffix      = regexp.MustCompile(`^INFO[1-3]$`)
	// Tasmota writes the MAC in discovery topics as 12 upper-case hex digits.
	// Only that canonical form is decoded, so case variants of one MAC cannot
	// become several cache keys or reservations.
	discoveryMAC = regexp.MustCompile(`^[0-9A-F]{12}$`)
)

// TopicCodec translates between Tasmota topics and (device topic, kind)
// for one FullTopic pattern. Tasmota allows any pattern that contains
// %topic% and %prefix%, for example "%prefix%/%topic%/" (default),
// "%topic%/%prefix%/" or "site/%topic%/%prefix%/".
type TopicCodec struct {
	levels   []string // pattern levels, trailing "/" removed
	prefixes [3]string
}

// NewTopicCodec parses a FullTopic pattern. Empty arguments select the
// Tasmota defaults. A pattern without %topic% or %prefix% is rejected by
// returning ok=false.
func NewTopicCodec(fullTopic string, prefixes [3]string) (c TopicCodec, ok bool) {
	if fullTopic == "" {
		fullTopic = DefaultFullTopic
	}
	for i := range prefixes {
		if prefixes[i] == "" {
			prefixes[i] = defaultPrefixes[i]
		}
	}
	if strings.Count(fullTopic, "%topic%") != 1 || strings.Count(fullTopic, "%prefix%") != 1 {
		return TopicCodec{}, false
	}
	levels := strings.Split(strings.TrimSuffix(fullTopic, "/"), "/")
	for _, l := range levels {
		// Tasmota replaces the placeholders inside a level as well, but a
		// level that mixes a placeholder with text cannot be matched as a
		// single MQTT wildcard level.
		if (strings.Contains(l, "%topic%") && l != "%topic%") || (strings.Contains(l, "%prefix%") && l != "%prefix%") {
			return TopicCodec{}, false
		}
	}
	return TopicCodec{levels: levels, prefixes: prefixes}, true
}

// build returns the topic levels for prefix and device topic ("+" gives a wildcard).
func (c TopicCodec) build(prefix, topic string) []string {
	out := make([]string, len(c.levels))
	for i, l := range c.levels {
		switch l {
		case "%prefix%":
			l = prefix
		case "%topic%":
			l = topic
		}
		out[i] = l
	}
	return out
}

// Filters returns the MQTT subscription filters: everything below the tele
// and stat patterns, plus the retained discovery configs.
func (c TopicCodec) Filters() []string {
	return []string{
		strings.Join(c.build(c.prefixes[2], "+"), "/") + "/#",
		strings.Join(c.build(c.prefixes[1], "+"), "/") + "/#",
		discoveryFilter,
	}
}

// Command returns the topic to publish a command to.
func (c TopicCodec) Command(topic, command string) string {
	return strings.Join(c.build(c.prefixes[0], topic), "/") + "/" + command
}

// Decode classifies an incoming topic. ok is false for topics tasmofleet
// does not use (other devices' cmnd, unknown suffixes, foreign topics).
func (c TopicCodec) Decode(t string) (kind Kind, topic, suffix string, ok bool) {
	parts := strings.Split(t, "/")
	if len(parts) == 4 && parts[0] == "tasmota" && parts[1] == "discovery" && parts[3] == "config" && discoveryMAC.MatchString(parts[2]) {
		return KindDiscovery, parts[2], "config", true
	}
	if len(parts) != len(c.levels)+1 {
		return "", "", "", false
	}
	suffix = parts[len(parts)-1]
	var prefix string
	for i, l := range c.levels {
		switch l {
		case "%prefix%":
			prefix = parts[i]
		case "%topic%":
			topic = parts[i]
		default:
			if parts[i] != l {
				return "", "", "", false
			}
		}
	}
	if topic == "" {
		return "", "", "", false
	}
	switch prefix {
	case c.prefixes[2]:
		switch {
		case suffix == "LWT":
			return KindLWT, topic, suffix, true
		case suffix == "STATE":
			return KindState, topic, suffix, true
		case suffix == "SENSOR":
			return KindSensor, topic, suffix, true
		case infoSuffix.MatchString(suffix):
			return KindInfo, topic, suffix, true
		}
	case c.prefixes[1]:
		switch {
		case suffix == "RESULT":
			return KindResult, topic, suffix, true
		case suffix == "UPGRADE":
			return KindUpgrade, topic, suffix, true
		case suffix == "FILEDOWNLOAD" || suffix == "FILEUPLOAD":
			return KindFile, topic, suffix, true
		case statusSuffix.MatchString(suffix):
			return KindStatus, topic, suffix, true
		case powerSuffix.MatchString(suffix):
			return KindPower, topic, suffix, true
		}
	}
	return "", "", "", false
}
