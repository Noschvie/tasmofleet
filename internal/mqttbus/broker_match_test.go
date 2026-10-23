package mqttbus

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func table(m map[string]string) LookupFunc {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ip, ok := m[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
}

// Why: a device configured with the broker's IP (or another alias of it) does
// report to our broker. Calling it "another broker" misleads the operator.
func TestBrokerMatcher(t *testing.T) {
	dns := table(map[string]string{
		"mqtt.example.internal": "172.31.12.5",
		"broker-alias.lan":      "172.31.12.5",
		"other.lan":             "172.31.12.9",
	})
	cases := []struct {
		name   string
		url    string
		host   string
		port   int
		want   bool
		wantOK bool // matcher exists
	}{
		{"same name", "tcp://mqtt.example.internal:1883", "mqtt.example.internal", 1883, true, true},
		{"name case-insensitive", "tcp://MQTT.Example.Internal:1883", "mqtt.EXAMPLE.internal", 1883, true, true},
		{"trailing dot", "tcp://mqtt.example.internal:1883", "mqtt.example.internal.", 1883, true, true},
		{"device port 0 means 1883", "tcp://mqtt.example.internal", "mqtt.example.internal", 0, true, true},
		{"hostname vs IP of the same broker", "tcp://mqtt.example.internal:1883", "172.31.12.5", 1883, true, true},
		{"IP vs hostname of the same broker", "tcp://172.31.12.5:1883", "mqtt.example.internal", 1883, true, true},
		{"two aliases of one IP", "tcp://mqtt.example.internal:1883", "broker-alias.lan", 1883, true, true},
		{"different port", "tcp://mqtt.example.internal:1883", "mqtt.example.internal", 1884, false, true},
		{"different host", "tcp://mqtt.example.internal:1883", "other.lan", 1883, false, true},
		{"different IP", "tcp://mqtt.example.internal:1883", "172.31.12.9", 1883, false, true},
		{"unresolvable device host", "tcp://mqtt.example.internal:1883", "nowhere.invalid", 1883, false, true},
		{"empty device host", "tcp://mqtt.example.internal:1883", "", 1883, false, true},
		{"ssl default port is 8883", "ssl://mqtt.example.internal", "mqtt.example.internal", 8883, true, true},
		{"ssl vs plain default port", "ssl://mqtt.example.internal", "mqtt.example.internal", 0, false, true},
		{"credentials in URL are ignored", "tcp://u:p@mqtt.example.internal:1883", "mqtt.example.internal", 1883, true, true},
		{"no broker configured", "", "mqtt.example.internal", 1883, false, false},
		{"garbage URL", "::::", "mqtt.example.internal", 1883, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewBrokerMatcher(tc.url, dns)
			if m.Configured() != tc.wantOK {
				t.Fatalf("Configured = %v", m.Configured())
			}
			if got := m.Same(tc.host, tc.port); got != tc.want {
				t.Fatalf("Same(%q,%d) = %v, want %v", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestBrokerMatcherCachesLookups(t *testing.T) {
	n := 0
	m := NewBrokerMatcher("tcp://mqtt.example.internal:1883", func(_ context.Context, h string) ([]netip.Addr, error) {
		n++
		return []netip.Addr{netip.MustParseAddr("172.31.12.5")}, nil
	})
	for i := 0; i < 5; i++ {
		m.Same("alias.lan", 1883)
	}
	if n != 2 { // alias.lan and the broker host, once each
		t.Fatalf("%d lookups, want 2 (a page render must not hit DNS each time)", n)
	}
}
