package config

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Broker is MQTT_URL as the settings page shows it: host, port and TLS. The
// URL stays the stored value; these parts are a view of it.
type Broker struct {
	Host string // empty: MQTT is off
	Port int    // 0: the default port (1883, with TLS 8883)
	TLS  bool
}

// Default broker ports.
const (
	MQTTPort    = 1883
	MQTTTLSPort = 8883
)

// SplitMQTTURL parses a broker URL into its parts. An empty URL is MQTT off
// (the zero Broker). tcp:// and mqtt:// are plain, ssl://, tls:// and mqtts://
// are TLS; a missing port is the scheme's default and is returned explicitly.
// A URL with user information, a path or a query cannot be shown as the three
// parts and is an error (the page then shows the URL itself).
func SplitMQTTURL(raw string) (Broker, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Broker{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Broker{}, errors.New("not a URL")
	}
	var b Broker
	switch u.Scheme {
	case "tcp", "mqtt":
	case "ssl", "tls", "mqtts":
		b.TLS = true
	default:
		return Broker{}, errors.New("unknown scheme " + strconv.Quote(u.Scheme))
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return Broker{}, errors.New("has more than scheme, host and port")
	}
	b.Host = u.Hostname()
	if b.Host == "" {
		return Broker{}, errors.New("no host")
	}
	b.Port = defaultMQTTPort(b.TLS)
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Broker{}, errors.New("port must be 1-65535")
		}
		b.Port = n
	}
	return b, nil
}

// JoinMQTTURL composes the broker URL: tcp://host:port, or ssl://host:port
// with TLS. An empty host is MQTT off ("", whatever the port says). Port 0 is
// the default port. The host is a name or an IP address (IPv6 with or without
// brackets); anything that would change the URL's structure is refused.
func JoinMQTTURL(b Broker) (string, error) {
	host := strings.TrimSpace(b.Host)
	if host == "" {
		return "", nil
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return "", errors.New("host: an IPv6 zone is not supported")
		}
	} else if strings.ContainsAny(host, ":/@?#[]%\\ \t") || strings.ContainsFunc(host, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("host: enter a host name or an IP address only")
	}
	port := b.Port
	if port == 0 {
		port = defaultMQTTPort(b.TLS)
	}
	if port < 1 || port > 65535 {
		return "", errors.New("port must be 1-65535")
	}
	scheme := "tcp"
	if b.TLS {
		scheme = "ssl"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func defaultMQTTPort(tls bool) int {
	if tls {
		return MQTTTLSPort
	}
	return MQTTPort
}
