package mqttbus

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LookupFunc resolves a host name to addresses. The production one is
// NetLookup; tests pass a table.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// NetLookup resolves through the system resolver.
func NetLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

const (
	lookupTimeout = 2 * time.Second
	lookupTTL     = 5 * time.Minute
	defaultPort   = 1883
	defaultTLS    = 8883
)

// BrokerMatcher answers "is this device configured for the broker tasmofleet
// uses?". The question is only about configuration; whether the device really
// publishes there is learned from its messages. A nil matcher means no
// broker is configured and matches nothing.
type BrokerMatcher struct {
	host   string
	port   int
	lookup LookupFunc
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]lookupEntry
}

type lookupEntry struct {
	addrs []netip.Addr
	at    time.Time
}

// NewBrokerMatcher parses the broker URL (tcp://, mqtt://, ssl://, mqtts://).
// An empty or unparsable URL yields nil: no broker, nothing matches.
func NewBrokerMatcher(brokerURL string, lookup LookupFunc) *BrokerMatcher {
	u, err := url.Parse(brokerURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	if lookup == nil {
		lookup = NetLookup
	}
	port := defaultPort
	switch u.Scheme {
	case "ssl", "tls", "mqtts", "tcps":
		port = defaultTLS
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		port = n
	}
	return &BrokerMatcher{host: normHost(u.Hostname()), port: port, lookup: lookup, now: time.Now, cache: map[string]lookupEntry{}}
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".") }

// Configured reports whether a broker is configured.
func (m *BrokerMatcher) Configured() bool { return m != nil }

// Same reports whether a device configured for host:port (port 0 = Tasmota
// default 1883) talks to our broker: same port and either the same host name
// (case-insensitive) or at least one common resolved address.
func (m *BrokerMatcher) Same(host string, port int) bool {
	if m == nil {
		return false
	}
	host = normHost(host)
	if host == "" {
		return false
	}
	if port == 0 {
		port = defaultPort
	}
	if port != m.port {
		return false
	}
	if host == m.host {
		return true
	}
	theirs := m.resolve(host)
	if len(theirs) == 0 {
		return false
	}
	for _, a := range m.resolve(m.host) {
		for _, b := range theirs {
			if a == b {
				return true
			}
		}
	}
	return false
}

func (m *BrokerMatcher) resolve(host string) []netip.Addr {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}
	}
	m.mu.Lock()
	c, ok := m.cache[host]
	m.mu.Unlock()
	if ok && m.now().Sub(c.at) < lookupTTL {
		return c.addrs
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	addrs, err := m.lookup(ctx, host)
	if err != nil {
		addrs = nil // unresolved: not provably the same broker
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	m.mu.Lock()
	m.cache[host] = lookupEntry{addrs: addrs, at: m.now()}
	m.mu.Unlock()
	return addrs
}
