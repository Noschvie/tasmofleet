package transport

import "strings"

// DefaultGroupTopic is Tasmota's default GroupTopic; a device may sit on it without anyone having
// set it, so it is never a topic of its own.
const DefaultGroupTopic = "tasmotas"

// TopicShapeOK reports whether t can be a single device's topic: not empty, no '/', '+', '#' and no
// whitespace. It mirrors mqttcmd's checkTopic, which stays the second guard.
func TopicShapeOK(t string) bool {
	return t != "" && !strings.ContainsAny(t, "/+#") && !strings.ContainsAny(t, " \t\r\n\v\f")
}

// CommandTopic expands a device's FullTopic into the stem it listens on for commands, the way
// Tasmota's GetTopic_P does: %prefix% becomes the command prefix (cmnd), %topic% the topic, a
// FullTopic without %prefix% gets "/%prefix%" appended, "//" collapses to "/", the trailing '/' is
// dropped. An empty FullTopic is Tasmota's default %prefix%/%topic%/. ok is false when the stem cannot
// be known: the FullTopic has no %topic% (every device with it would share one stem) or contains
// another placeholder (%hostname%, %id%, ...).
func CommandTopic(fullTopic, topic, prefix string) (stem string, ok bool) {
	if fullTopic == "" {
		fullTopic = defaultFullTopic
	}
	if !strings.Contains(fullTopic, "%topic%") {
		return "", false
	}
	if !strings.Contains(fullTopic, "%prefix%") {
		fullTopic += "/%prefix%"
	}
	tmpl := strings.ReplaceAll(strings.ReplaceAll(fullTopic, "%prefix%", prefix), "%topic%", "\x00")
	if strings.Contains(tmpl, "%") {
		return "", false
	}
	s := strings.ReplaceAll(tmpl, "\x00", topic)
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return strings.Trim(s, "/"), true
}

// StemsOverlap reports whether a device listening on stem a would also receive what is sent to stem
// b, or the other way round: Tasmota subscribes to stem + "/#", so one stem that is a level prefix of
// the other overlaps.
func StemsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
