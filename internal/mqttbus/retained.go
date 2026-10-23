package mqttbus

import (
	"container/list"
	"strings"
)

// Retained replay cache. Threat model and budgets: docs/security/mqtt-ingest.md.
//
// Sizes count topic + payload bytes. A message is "known" when Options.Known
// says it belongs to an inventory device. Known messages are accounted per
// device key (the %topic%, or the MAC for discovery messages), each key with
// its own small reservation, so spoofed messages on one device's topics can
// only use that device's reservation, never another device's. Known entries
// are never evicted. A known message beyond its device's reservation is
// treated as unknown. Unknown messages share one small pool, oldest evicted
// first. Live dispatch to handlers never depends on the cache.
const (
	maxPayload          = 256 << 10 // longest message tasmofleet handles (STATUS dumps are a few KiB)
	maxKeyTopics        = 32        // retained topics per known device key
	maxKeyBytes         = 256 << 10 // retained bytes per known device key
	maxKnownKeys        = 4096      // known device keys (sanity cap; the fleet has tens)
	maxUnknownTopics    = 1024
	maxUnknownBytes     = 4 << 20
	dropPayloadTooBig   = "payload_too_large"
	dropKnownKeysFull   = "known_keys_full"
	dropUnknownEvict    = "unknown_evicted"
	dropUnknownTooBig   = "unknown_too_large"
	dropKeyOverReserved = "device_reservation_full"
)

type retEntry struct {
	msg  Message
	key  string // device key for known entries, "" for unknown
	size int
	elem *list.Element // position in the unknown FIFO; nil for known entries
}

type keyUsage struct{ topics, bytes int }

// retainedCache is not safe for concurrent use; the bus holds its mutex.
type retainedCache struct {
	entries      map[string]*retEntry
	keys         map[string]*keyUsage
	unknownOrder *list.List // of topic strings, oldest first
	unknownBytes int
}

func newRetainedCache() *retainedCache {
	return &retainedCache{entries: map[string]*retEntry{}, keys: map[string]*keyUsage{}, unknownOrder: list.New()}
}

// DeviceKey is the one key function for a decoded message: the retained
// cache accounts reservations under it, and the caller's Options.Known must
// classify with it too (TopicKey/MACKey build the same keys from inventory
// values), so classification and storage cannot disagree. Discovery
// messages are keyed by MAC, everything else by the device %topic%; both
// are exact bytes of the decoded (canonical) topic.
func DeviceKey(m Message) string {
	if m.Kind == KindDiscovery {
		return "mac:" + m.Topic
	}
	return TopicKey(m.Topic)
}

// TopicKey is DeviceKey for an inventory device's MQTT %topic%.
func TopicKey(topic string) string { return "topic:" + topic }

// MACKey is DeviceKey for an inventory MAC in any common spelling
// (aa:bb:..., AABB...); "" if it is not a MAC.
func MACKey(mac string) string {
	s := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
	if !discoveryMAC.MatchString(s) {
		return ""
	}
	return "mac:" + s
}

// put stores, replaces or (empty payload) clears the entry for topic and
// returns the reasons of any drops, for metrics.
func (c *retainedCache) put(topic string, msg Message, known bool) []string {
	c.remove(topic) // replacing = remove + add: one accounting path
	if len(msg.Payload) == 0 {
		return nil
	}
	size := len(topic) + len(msg.Payload)
	var drops []string
	if known {
		key := DeviceKey(msg)
		u := c.keys[key]
		switch {
		case u == nil && len(c.keys) >= maxKnownKeys:
			drops = append(drops, dropKnownKeysFull)
		case u != nil && (u.topics+1 > maxKeyTopics || u.bytes+size > maxKeyBytes):
			drops = append(drops, dropKeyOverReserved)
		case size > maxKeyBytes:
			drops = append(drops, dropKeyOverReserved)
		default:
			if u == nil {
				u = &keyUsage{}
				c.keys[key] = u
			}
			u.topics++
			u.bytes += size
			c.entries[topic] = &retEntry{msg: msg, key: key, size: size}
			return nil
		}
		// Over the device's reservation: compete in the unknown pool instead.
	}
	if size > maxUnknownBytes {
		return append(drops, dropUnknownTooBig)
	}
	for c.unknownOrder.Len() >= maxUnknownTopics || c.unknownBytes+size > maxUnknownBytes {
		c.remove(c.unknownOrder.Front().Value.(string)) //nolint:forcetypeassert // the list only holds topic strings
		drops = append(drops, dropUnknownEvict)
	}
	e := &retEntry{msg: msg, size: size}
	e.elem = c.unknownOrder.PushBack(topic)
	c.entries[topic] = e
	c.unknownBytes += size
	return drops
}

func (c *retainedCache) remove(topic string) {
	e, ok := c.entries[topic]
	if !ok {
		return
	}
	delete(c.entries, topic)
	if e.key != "" {
		u := c.keys[e.key]
		u.topics--
		u.bytes -= e.size
		if u.topics == 0 {
			delete(c.keys, e.key)
		}
		return
	}
	c.unknownOrder.Remove(e.elem)
	c.unknownBytes -= e.size
}

// messages returns all cached messages (for Subscribe replay).
func (c *retainedCache) messages() []Message {
	out := make([]Message, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.msg)
	}
	return out
}

func (c *retainedCache) len() int { return len(c.entries) }
