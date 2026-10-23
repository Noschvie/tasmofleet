package mqttbus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ProbeTimeout bounds one connection test.
const ProbeTimeout = 8 * time.Second

// Probe connects once (no retries, no subscriptions), reports whether the
// broker accepted the credentials and disconnects. It backs the "test
// connection" button of the system settings page. The error text names the
// broker host and the failure, never the user name or password.
//
// The host name is resolved once and the connection goes to that address, so
// a changing DNS answer cannot redirect the credentials after the check. Link
// local (cloud metadata), multicast and unspecified addresses are refused.
//
// Which addresses may be tried is decided by o.AllowAddr (the caller passes
// the device client's rule: no loopback, link-local, NAT64, v4-compatible,
// metadata, multicast or unspecified addresses). Without it nothing is tried.
// The errors are deliberately coarse so the test cannot be used to map the
// network; the detail goes to the log.
func Probe(ctx context.Context, o Options) error {
	if o.AllowAddr == nil {
		return errors.New("connection tests are not available")
	}
	u, err := url.Parse(o.BrokerURL)
	if err != nil || u.Hostname() == "" {
		return errors.New("invalid broker URL")
	}
	if err := refuseWebsocket(u); err != nil {
		return errors.New("websocket brokers are not supported; use tcp:// or ssl://")
	}
	host, port := u.Hostname(), u.Port()
	ip, err := probeAddr(ctx, host, o.AllowAddr)
	if err != nil {
		return err
	}
	pinned := *u
	if port != "" {
		pinned.Host = net.JoinHostPort(ip.String(), port)
	} else if ip.Is6() {
		pinned.Host = "[" + ip.String() + "]"
	} else {
		pinned.Host = ip.String()
	}
	pinned.User = nil
	po := paho.NewClientOptions().
		AddBroker(pinned.String()).
		SetTLSConfig(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}).
		SetClientID("tasmofleet-probe").
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetConnectTimeout(ProbeTimeout)
	if o.Username != "" {
		po.SetUsername(o.Username).SetPassword(o.Password)
	}
	c := paho.NewClient(po)
	tok := c.Connect()
	done := make(chan struct{})
	go func() { tok.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		c.Disconnect(0)
		return ctx.Err()
	case <-time.After(ProbeTimeout + time.Second):
		c.Disconnect(0)
		return unreachable(u.Host)
	}
	if err := tok.Error(); err != nil {
		slog.Warn("mqtt connection test failed", "host", u.Host, "err", err)
		return classifyProbeError(u.Host, err)
	}
	c.Disconnect(100)
	return nil
}

// lookupIP resolves a host name; tests replace it.
var lookupIP = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// probeAddr resolves host once and returns the first usable address.
func probeAddr(ctx context.Context, host string, allow func(netip.Addr) bool) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return checkProbeAddr(ip.Unmap(), allow)
	}
	addrs, err := lookupIP(ctx, host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, errors.New("cannot resolve the host name")
	}
	var last error
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		got, err := checkProbeAddr(ip.Unmap(), allow)
		if err == nil {
			return got, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no usable address")
	}
	return netip.Addr{}, last
}

func checkProbeAddr(ip netip.Addr, allow func(netip.Addr) bool) (netip.Addr, error) {
	if !ip.IsValid() || !allow(ip) {
		return netip.Addr{}, errNotAllowed
	}
	return ip, nil
}

var errNotAllowed = errors.New("this address is not allowed as a broker")

func unreachable(host string) error {
	return fmt.Errorf("no MQTT broker reachable at %s", host)
}

// classifyProbeError reduces paho's detailed errors to two answers: the broker
// refused the credentials, or nothing usable answered.
func classifyProbeError(host string, err error) error {
	low := strings.ToLower(err.Error())
	for _, m := range []string{"not authorized", "bad user name", "identifier rejected", "unacceptable protocol", "credentials"} {
		if strings.Contains(low, m) {
			return errors.New("the broker answered but refused the credentials")
		}
	}
	return unreachable(host)
}
