package mqttbus

import (
	"strings"
	"testing"
)

func prefixFor(c TopicCodec, k Kind) string {
	switch k {
	case KindLWT, KindState, KindSensor, KindInfo:
		return c.prefixes[2]
	case KindResult, KindStatus, KindPower, KindUpgrade, KindDiscovery:
		return c.prefixes[1]
	}
	return c.prefixes[1]
}

// Why (review: parser differential): the decoded message is the only input to
// both classification (Options.Known) and storage (the reservation key). That
// is only safe if decoding is injective on canonical topics: every decoded
// topic re-encodes to exactly the raw topic, and the device key built from
// inventory values (TopicKey/MACKey) equals DeviceKey of the decoded message.
// A non-canonical spelling (case variant of a MAC, extra level, empty level)
// must not decode at all.
func FuzzDecodeIsCanonical(f *testing.F) {
	for _, s := range []string{
		"tele/lamp/LWT", "stat/lamp/STATUS13", "stat/lamp/POWER2", "tele/lamp/INFO1",
		"tasmota/discovery/02005ED22C14/config", "tasmota/discovery/02005ed22c14/config",
		"tele//LWT", "tele/lamp//LWT", "tele/lamp/LWT/", "site/lamp/tele/LWT", "tele/la+mp/LWT", "tele/lamp/lwt",
	} {
		f.Add(s, 0)
		f.Add(s, 1)
	}
	patterns := []string{"%prefix%/%topic%/", "%topic%/%prefix%/", "site/%topic%/%prefix%/"}
	f.Fuzz(func(t *testing.T, raw string, p int) {
		pattern := patterns[(p%len(patterns)+len(patterns))%len(patterns)]
		c, _ := NewTopicCodec(pattern, [3]string{})
		kind, topic, suffix, ok := c.Decode(raw)
		if !ok {
			return
		}
		m := Message{Kind: kind, Topic: topic, Suffix: suffix}
		if kind == KindDiscovery {
			if raw != "tasmota/discovery/"+topic+"/config" || MACKey(topic) != DeviceKey(m) {
				t.Fatalf("non-canonical discovery topic %q decoded (key %q vs %q)", raw, DeviceKey(m), MACKey(topic))
			}
			return
		}
		re := strings.Join(c.build(prefixFor(c, kind), topic), "/") + "/" + suffix
		if re != raw {
			t.Fatalf("%q decodes to (%s, %q, %q) which re-encodes to %q", raw, kind, topic, suffix, re)
		}
		if strings.Contains(topic, "/") || topic == "" {
			t.Fatalf("device topic %q is not a single non-empty level", topic)
		}
		if TopicKey(topic) != DeviceKey(m) {
			t.Fatalf("classification key %q != storage key %q", TopicKey(topic), DeviceKey(m))
		}
	})
}

// Case variants of one MAC used to give separate reservations.
func TestDiscoveryMACMustBeCanonical(t *testing.T) {
	c, _ := NewTopicCodec("", [3]string{})
	if _, _, _, ok := c.Decode("tasmota/discovery/02005ed22c14/config"); ok {
		t.Fatal("lower-case MAC decoded")
	}
	if _, _, _, ok := c.Decode("tasmota/discovery/02005ED22C14/config"); !ok {
		t.Fatal("canonical MAC not decoded")
	}
	if MACKey("02:00:5e:d2:2c:14") != MACKey("02005ED22C14") || MACKey("nope") != "" {
		t.Fatal("MACKey does not normalise inventory spellings")
	}
}
