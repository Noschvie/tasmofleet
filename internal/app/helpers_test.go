package app

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
)

func splitAddr(t *testing.T, hostport string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, n
}

func loopbackNets() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }
