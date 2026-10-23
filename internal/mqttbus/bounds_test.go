package mqttbus

import (
	"fmt"
	"strings"
	"testing"
)

type fakeMsg struct {
	topic    string
	payload  []byte
	retained bool
}

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 0 }
func (m fakeMsg) Retained() bool    { return m.retained }
func (m fakeMsg) Topic() string     { return m.topic }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}

type dropLog map[string]int

func newBoundsBus(t *testing.T, known func(Message) bool) (*bus, dropLog) {
	t.Helper()
	codec, ok := NewTopicCodec("%prefix%/%topic%/", [3]string{"cmnd", "stat", "tele"})
	if !ok {
		t.Fatal("codec")
	}
	drops := dropLog{}
	return &bus{codec: codec, subs: map[int]*sub{}, retained: newRetainedCache(), known: known,
		onDropped: func(r string) { drops[r]++ }}, drops
}

func retainedMsg(topic, payload string) fakeMsg {
	return fakeMsg{topic: topic, payload: []byte(payload), retained: true}
}

func lwt(dev, payload string) fakeMsg { return retainedMsg("tele/"+dev+"/LWT", payload) }

type knownSet map[string]bool

func (k knownSet) fn(m Message) bool { return k[m.Topic] }

func cached(b *bus, topic string) (*retEntry, bool) {
	e, ok := b.retained.entries[topic]
	return e, ok
}

// checkAccounting recomputes every counter from the entries (review: cap
// bypass through drifting accounting).
func checkAccounting(t *testing.T, c *retainedCache) {
	t.Helper()
	keys := map[string]*keyUsage{}
	unknown := 0
	for topic, e := range c.entries {
		if e.size != len(topic)+len(e.msg.Payload) {
			t.Fatalf("%s: recorded size %d", topic, e.size)
		}
		if e.key == "" {
			unknown += e.size
			continue
		}
		if keys[e.key] == nil {
			keys[e.key] = &keyUsage{}
		}
		keys[e.key].topics++
		keys[e.key].bytes += e.size
	}
	if unknown != c.unknownBytes || c.unknownOrder.Len()+knownEntries(c) != c.len() || len(keys) != len(c.keys) {
		t.Fatalf("accounting drift: unknown bytes %d/%d, keys %d/%d", unknown, c.unknownBytes, len(keys), len(c.keys))
	}
	for k, u := range keys {
		if *c.keys[k] != *u {
			t.Fatalf("key %s: %+v recorded, %+v actual", k, *c.keys[k], *u)
		}
		if u.topics > maxKeyTopics || u.bytes > maxKeyBytes {
			t.Fatalf("key %s over its reservation: %+v", k, *u)
		}
	}
	if c.unknownOrder.Len() > maxUnknownTopics || c.unknownBytes > maxUnknownBytes {
		t.Fatalf("unknown pool over budget: %d topics, %d bytes", c.unknownOrder.Len(), c.unknownBytes)
	}
}

func knownEntries(c *retainedCache) int {
	n := 0
	for _, e := range c.entries {
		if e.key != "" {
			n++
		}
	}
	return n
}

// Why (squatting): 1e5 junk retained topics must not push the LWT of managed
// devices out of the replay cache.
func TestJunkTopicsCannotEvictInventoryDevices(t *testing.T) {
	k := knownSet{"lamp": true, "plug": true}
	b, drops := newBoundsBus(t, k.fn)
	b.dispatch(nil, lwt("lamp", "Online"))
	for i := range 100_000 {
		b.dispatch(nil, lwt(fmt.Sprintf("junk%d", i), "Online"))
	}
	b.dispatch(nil, lwt("plug", "Online"))
	for _, d := range []string{"lamp", "plug"} {
		if e, ok := cached(b, "tele/"+d+"/LWT"); !ok || e.key == "" {
			t.Fatalf("%s not in its reservation after the flood", d)
		}
	}
	if drops[dropUnknownEvict] == 0 {
		t.Fatal("evictions not reported")
	}
	checkAccounting(t, b.retained)
}

// Why (review: shared known pool): spoofed messages on one managed device's
// topics may only use that device's reservation, never another device's.
func TestSpoofedKnownTopicsStayInOneReservation(t *testing.T) {
	k := knownSet{"lamp": true, "plug": true}
	b, drops := newBoundsBus(t, k.fn)
	b.dispatch(nil, lwt("plug", "Online"))
	for i := range 100 { // STATUS0..99 on the lamp's prefix
		b.dispatch(nil, retainedMsg(fmt.Sprintf("stat/lamp/STATUS%d", i), strings.Repeat("x", 20_000)))
	}
	if u := b.retained.keys["topic:lamp"]; u == nil || u.topics > maxKeyTopics || u.bytes > maxKeyBytes {
		t.Fatalf("lamp reservation exceeded: %+v", u)
	}
	if e, ok := cached(b, "tele/plug/LWT"); !ok || e.key != "topic:plug" {
		t.Fatal("another device's reservation was affected")
	}
	if drops[dropKeyOverReserved] == 0 {
		t.Fatal("reservation overflow not reported")
	}
	checkAccounting(t, b.retained)
}

// Why (review: cap bypass): every path (new, overwrite, clear, class change)
// keeps the accounting exact.
func TestAccountingSurvivesOverwriteChurn(t *testing.T) {
	k := knownSet{"lamp": true}
	b, _ := newBoundsBus(t, k.fn)
	for i := range 10_000 {
		payload := strings.Repeat("x", i%300+1)
		b.dispatch(nil, lwt("lamp", payload))
		b.dispatch(nil, retainedMsg(fmt.Sprintf("stat/lamp/POWER%d", i%40), payload))
		b.dispatch(nil, lwt(fmt.Sprintf("junk%d", i%50), payload))
		if i%7 == 0 {
			b.dispatch(nil, lwt(fmt.Sprintf("junk%d", i%50), ""))
			b.dispatch(nil, retainedMsg(fmt.Sprintf("stat/lamp/POWER%d", i%40), ""))
		}
		if i%1000 == 0 {
			k["lamp"] = !k["lamp"]
			b.Reclassify()
		}
	}
	checkAccounting(t, b.retained)
}

// Topic bytes count too: long topic names cannot bypass the byte budget.
func TestTopicLengthCounts(t *testing.T) {
	b, _ := newBoundsBus(t, nil)
	long := strings.Repeat("t", 60_000)
	for i := range 200 {
		b.dispatch(nil, lwt(fmt.Sprintf("%s%d", long, i), "x"))
	}
	checkAccounting(t, b.retained)
}

// Why (review: stale reservations): a device removed from the inventory loses
// its reservation on the next resync; a device added gains one.
func TestReclassifyFollowsInventory(t *testing.T) {
	k := knownSet{"lamp": true}
	b, _ := newBoundsBus(t, k.fn)
	b.dispatch(nil, lwt("lamp", "Online"))
	b.dispatch(nil, lwt("plug", "Online"))
	delete(k, "lamp")
	k["plug"] = true
	b.Reclassify()
	if e, _ := cached(b, "tele/lamp/LWT"); e.key != "" {
		t.Fatal("removed device kept its reservation")
	}
	if e, _ := cached(b, "tele/plug/LWT"); e.key != "topic:plug" {
		t.Fatal("added device did not gain a reservation")
	}
	checkAccounting(t, b.retained)
}

// Live dispatch never depends on the cache: handlers see every message.
func TestDispatchUnaffectedByCacheLimits(t *testing.T) {
	b, _ := newBoundsBus(t, nil)
	var seen int
	b.subs[1] = &sub{kinds: map[Kind]struct{}{KindLWT: {}}, h: func(Message) { seen++ }}
	for i := range 5000 {
		b.dispatch(nil, lwt(fmt.Sprintf("junk%d", i), "Online"))
	}
	if seen != 5000 {
		t.Fatalf("handlers saw %d of 5000", seen)
	}
}

// Oversized payloads never reach a parser and are reported.
func TestOversizedPayloadIsDropped(t *testing.T) {
	b, drops := newBoundsBus(t, nil)
	var seen int
	b.subs[1] = &sub{kinds: map[Kind]struct{}{KindState: {}}, h: func(Message) { seen++ }}
	b.dispatch(nil, retainedMsg("tele/a/STATE", strings.Repeat("x", 1<<20)))
	if seen != 0 || b.retained.len() != 0 || drops[dropPayloadTooBig] != 1 {
		t.Fatalf("1 MiB message delivered (%d), kept (%d) or not reported (%v)", seen, b.retained.len(), drops)
	}
	b.dispatch(nil, fakeMsg{topic: "tele/a/STATE", payload: []byte(`{"POWER":"ON"}`)})
	if seen != 1 {
		t.Fatal("normal message after a dropped one not delivered")
	}
}

// Suffixes with unbounded digits would let one prefix mint unlimited topics.
func TestSuffixDigitsAreBounded(t *testing.T) {
	codec, _ := NewTopicCodec("%prefix%/%topic%/", [3]string{"cmnd", "stat", "tele"})
	for _, topic := range []string{"stat/a/STATUS123", "stat/a/POWER100", "tele/a/INFO4", "tele/a/INFO"} {
		if _, _, _, ok := codec.Decode(topic); ok {
			t.Errorf("%s decoded", topic)
		}
	}
	for _, topic := range []string{"stat/a/STATUS", "stat/a/STATUS13", "stat/a/POWER", "stat/a/POWER32", "tele/a/INFO3"} {
		if _, _, _, ok := codec.Decode(topic); !ok {
			t.Errorf("%s not decoded", topic)
		}
	}
}
