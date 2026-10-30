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

// CommandTopic expands a device's FullTopic for a command to topic: %prefix% becomes cmnd, %topic%
// the topic; an empty FullTopic is Tasmota's default %prefix%/%topic%/. A trailing '/' is dropped.
// ok is false when the FullTopic has no %topic% (every device would share one topic) or any other
// placeholder (%hostname%, %id%, ...) that cannot be expanded from what tasmofleet knows.
func CommandTopic(fullTopic, topic string) (string, bool) {
	if fullTopic == "" {
		fullTopic = defaultFullTopic
	}
	if !strings.Contains(fullTopic, "%topic%") {
		return "", false
	}
	tmpl := strings.ReplaceAll(strings.ReplaceAll(fullTopic, "%prefix%", "cmnd"), "%topic%", "\x00")
	if strings.Contains(tmpl, "%") {
		return "", false
	}
	return strings.TrimRight(strings.ReplaceAll(tmpl, "\x00", topic), "/"), true
}
