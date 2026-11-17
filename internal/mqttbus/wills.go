package mqttbus

import "strings"

// Wills is what the current connection heard of the devices' last wills and live messages (#972). The transport facts
// take silence under a device's topic as evidence only when the connection hears devices at all, and only while no
// device outside the inventory is up on the broker.
type Wills struct {
	// Up are the topics, in no order, that this connection heard a device under and whose latest will is not Offline:
	// an Online will (or any other text), or a live (not retained) telemetry or reply message. A cleared will (empty
	// payload) keeps the topic's last state: it deletes the retained message, the device stays connected.
	Up []string
	// Heard is whether any will (Online, Offline or a cleared one) arrived on this connection. A broker that denies
	// TasmoFleet reading the devices' wills lets none of them through. A live telemetry or reply message does not count:
	// a reply proves stat is readable, not that the wills are (an ACL may allow stat/# but not tele/#).
	Heard bool
	// Complete is false once more topics were up than the ledger keeps (maxWillTopics): Up may then miss some, until
	// the next connection.
	Complete bool
}

// WillLedger is a Bus that keeps the last wills heard on its current connection.
type WillLedger interface {
	// WillsHeard is the ledger of the current connection; the zero Wills (nothing heard) while not Connected.
	WillsHeard() Wills
}

// maxWillTopics bounds the topics the ledger keeps up; far above a home fleet, and on a larger shared broker the
// ledger says it is incomplete instead of dropping a topic silently.
const maxWillTopics = 4096

// willLedger is the bus's record of the wills of the current connection. The bus guards it with its mutex.
type willLedger struct {
	limit int
	up    map[string]struct{}
	heard bool
	full  bool // a topic was not kept since the last reset
}

func newWillLedger(limit int) *willLedger {
	return &willLedger{limit: limit, up: make(map[string]struct{})}
}

// reset forgets everything: a new connection starts with nothing heard, and the broker's replay of the retained wills
// fills it again.
func (l *willLedger) reset() {
	clear(l.up)
	l.heard, l.full = false, false
}

// record takes in the will of a topic. Every will counts as heard, an empty payload (a cleared retained will) too: it
// still came through the broker to us, so reading works. Only Offline, compared as the health tracker does, takes the
// topic down. An empty payload leaves its state as it was: it means someone deleted the retained will (MQTT Explorer,
// a cleanup script), not that the device left; the health tracker does not take it as Offline either.
func (l *willLedger) record(topic string, payload []byte) {
	l.heard = true
	switch p := strings.TrimSpace(string(payload)); {
	case p == "": // keep the topic's last state
	case strings.EqualFold(p, "offline"):
		delete(l.up, topic)
	default:
		l.markUp(topic)
	}
}

// live takes in a live (not retained) telemetry or reply message of a topic: a device is connected under it now, also
// when its will was never heard (deleted before this connection subscribed, or a broker that keeps no retained ones).
// It does not set heard: only a will shows that the devices' wills can be read on this connection.
func (l *willLedger) live(topic string) {
	l.markUp(topic)
}

func (l *willLedger) markUp(topic string) {
	if _, ok := l.up[topic]; !ok && len(l.up) >= l.limit {
		l.full = true
		return
	}
	l.up[topic] = struct{}{}
}

// wills is a copy the caller may keep.
func (l *willLedger) wills() Wills {
	w := Wills{Heard: l.heard, Complete: !l.full, Up: make([]string, 0, len(l.up))}
	for t := range l.up {
		w.Up = append(w.Up, t)
	}
	return w
}
