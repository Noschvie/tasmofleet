package config

import (
	"strings"
	"testing"
)

// Why: the settings page shows MQTT_URL as Host / Port / TLS and writes back
// what JoinMQTTURL composes. If splitting and joining disagree, merely opening
// and saving the MQTT tab would silently move the fleet to another broker,
// port or transport (a TLS broker reached in clear text).
func TestMQTTURLRoundTrip(t *testing.T) {
	urls := []string{
		"tcp://mqtt.example.internal:1883",
		"tcp://mqtt.example.internal",
		"mqtt://10.0.0.5:1884",
		"ssl://broker.example:8883",
		"ssl://broker.example",
		"tls://broker.example:443",
		"mqtts://broker.example",
		"tcp://[fd00::1]:1883",
		"ssl://[fd00::1]",
		"tcp://Broker.Example:1/",
	}
	for _, raw := range urls {
		b, err := SplitMQTTURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		joined, err := JoinMQTTURL(b)
		if err != nil {
			t.Fatalf("%s: join %+v: %v", raw, b, err)
		}
		again, err := SplitMQTTURL(joined)
		if err != nil || again != b {
			t.Errorf("%s -> %+v -> %s -> %+v (%v): the parts changed on save", raw, b, joined, again, err)
		}
		if j2, _ := JoinMQTTURL(again); j2 != joined {
			t.Errorf("%s: composing is not stable: %s then %s", raw, joined, j2)
		}
		// The composed URL must still pass the validation the server applies.
		c := Config{MQTTURL: joined, LogLevel: "info"}
		bad := map[string]string{}
		c.validateOperational(func(k, m string) { bad[k] = m })
		if m, ok := bad["MQTT_URL"]; ok {
			t.Errorf("%s: composed %s is refused: %s", raw, joined, m)
		}
	}
}

// Why: each part maps to exactly one property of the connection; TLS decides
// the transport and the default port, never the host.
func TestSplitMQTTURLParts(t *testing.T) {
	cases := []struct {
		raw  string
		want Broker
	}{
		{"", Broker{}},
		{"tcp://h:1883", Broker{Host: "h", Port: 1883}},
		{"tcp://h", Broker{Host: "h", Port: 1883}},
		{"mqtt://h:2000", Broker{Host: "h", Port: 2000}},
		{"ssl://h", Broker{Host: "h", Port: 8883, TLS: true}},
		{"tls://h:9000", Broker{Host: "h", Port: 9000, TLS: true}},
		{"mqtts://h", Broker{Host: "h", Port: 8883, TLS: true}},
		{"tcp://[fd00::1]:1", Broker{Host: "fd00::1", Port: 1}},
	}
	for _, c := range cases {
		got, err := SplitMQTTURL(c.raw)
		if err != nil || got != c.want {
			t.Errorf("SplitMQTTURL(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
	}
}

// Why: a URL the three fields cannot represent must not be shown as if they
// did (saving would drop the credentials, path or the unknown scheme).
func TestSplitMQTTURLRefusesWhatThePartsCannotHold(t *testing.T) {
	for _, raw := range []string{
		"tcp://user:pw@h:1883", "ws://h:80", "http://h", "tcp://h:1883/x", "tcp://h:1883?a=b",
		"tcp://:1883", "tcp://h:0", "tcp://h:70000", "h:1883", "tcp://h#f", "%zz",
	} {
		if b, err := SplitMQTTURL(raw); err == nil {
			t.Errorf("SplitMQTTURL(%q) = %+v, want an error", raw, b)
		}
	}
}

// Why: Join is the only way the form writes MQTT_URL; it must produce the
// scheme TLS asks for, the default port for each transport, MQTT off for an
// empty host, and never let a host smuggle a path, user or second port in.
func TestJoinMQTTURL(t *testing.T) {
	ok := []struct {
		b    Broker
		want string
	}{
		{Broker{}, ""},
		{Broker{Port: 1883, TLS: true}, ""}, // no host: MQTT off, the port does not matter
		{Broker{Host: "h"}, "tcp://h:1883"},
		{Broker{Host: "h", TLS: true}, "ssl://h:8883"},
		{Broker{Host: "h", Port: 1883, TLS: true}, "ssl://h:1883"},
		{Broker{Host: " h ", Port: 65535}, "tcp://h:65535"},
		{Broker{Host: "fd00::1", Port: 1883}, "tcp://[fd00::1]:1883"},
		{Broker{Host: "[fd00::1]", Port: 1883}, "tcp://[fd00::1]:1883"},
		{Broker{Host: "10.0.0.5", Port: 1}, "tcp://10.0.0.5:1"},
	}
	for _, c := range ok {
		got, err := JoinMQTTURL(c.b)
		if err != nil || got != c.want {
			t.Errorf("JoinMQTTURL(%+v) = %q, %v; want %q", c.b, got, err, c.want)
		}
	}
	for _, b := range []Broker{
		{Host: "h", Port: -1}, {Host: "h", Port: 65536},
		{Host: "u:p@h"}, {Host: "h/x"}, {Host: "h:1884"}, {Host: "h?x"}, {Host: "h#x"},
		{Host: "a b"}, {Host: "h\x01"}, {Host: "fe80::1%eth0"}, {Host: "h\\x"},
	} {
		if got, err := JoinMQTTURL(b); err == nil {
			t.Errorf("JoinMQTTURL(%+v) = %q, want an error", b, got)
		} else if !strings.Contains(err.Error(), "host") && !strings.Contains(err.Error(), "port") {
			t.Errorf("JoinMQTTURL(%+v): error %q does not name the field", b, err)
		}
	}
}

// Why (#1055): ParseBrokerURL is the one reader of a broker URL's scheme, host
// and port; the broker matcher, the stored password's binding and the
// validation all read through it. Each of them must see the same broker for
// the same URL: tcp and mqtt are one transport, ssl, tls and mqtts the other.
func TestParseBrokerURL(t *testing.T) {
	cases := []struct {
		raw  string
		want Broker
	}{
		{"tcp://h:1883", Broker{Host: "h", Port: 1883}},
		{"mqtt://h", Broker{Host: "h", Port: 1883}},
		{"ssl://h", Broker{Host: "h", Port: 8883, TLS: true}},
		{"tls://h:9000", Broker{Host: "h", Port: 9000, TLS: true}},
		{"mqtts://h", Broker{Host: "h", Port: 8883, TLS: true}},
		{"tcp://[fd00::1]", Broker{Host: "fd00::1", Port: 1883}},
		// The environment may carry user information; it is not part of the broker (#597).
		{"tcp://user:pw@h:1883", Broker{Host: "h", Port: 1883}},
		{"mqtts://user:pw@h", Broker{Host: "h", Port: 8883, TLS: true}},
		{"tcp://h:1883/path?a=b", Broker{Host: "h", Port: 1883}},
		{"tcp://h:0", Broker{Host: "h", Port: 0}}, // not range-checked: the form's view does that
	}
	for _, c := range cases {
		got, err := ParseBrokerURL(c.raw)
		if err != nil || got != c.want {
			t.Errorf("ParseBrokerURL(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
	}
	// Schemes the MQTT client cannot use (its address filter does not apply to
	// websockets), no host, no URL, and a port that is no number.
	for _, raw := range []string{
		"", "tcps://h", "ws://h:80", "wss://h:443", "ftp://h", "http://h", "h:1883", "tcp://", "tcp://:1883",
		"tcp://user:pw@:1883", "%zz", "tcp://h:99999999999999999999",
	} {
		if b, err := ParseBrokerURL(raw); err == nil {
			t.Errorf("ParseBrokerURL(%q) = %+v, want an error", raw, b)
		}
	}
}

// Why (#1055): MQTT_URL from the environment and from the form go through one
// validation. This pins which URLs it accepts, so that reading them through
// ParseBrokerURL cannot silently widen or narrow what an operator may set.
func TestValidateOperationalMQTTURL(t *testing.T) {
	ok := []string{
		"", "tcp://h", "tcp://h:1883", "mqtt://h:1883", "ssl://h:8883", "tls://h", "mqtts://h",
		"tcp://user:pw@h:1883", "tcp://[fd00::1]:1883", "tcp://h:1883/path", "tcp://h:0",
	}
	refused := []string{
		"tcps://h", "ws://h:80", "wss://h:443", "ftp://h", "tcp://", "tcp://:1883", "h:1883", "tcp://h:abc",
		"tcp://h:99999999999999999999", // a port no connection can use (the matcher already refused it)
	}
	check := func(raw string) bool {
		c := Config{MQTTURL: raw, LogLevel: "info"}
		refusedURL := false
		c.validateOperational(func(k, _ string) { refusedURL = refusedURL || k == "MQTT_URL" })
		return !refusedURL
	}
	for _, raw := range ok {
		if !check(raw) {
			t.Errorf("MQTT_URL %q is refused, want accepted", raw)
		}
	}
	for _, raw := range refused {
		if check(raw) {
			t.Errorf("MQTT_URL %q is accepted, want refused", raw)
		}
	}
}
