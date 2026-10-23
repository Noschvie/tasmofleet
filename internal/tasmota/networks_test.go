package tasmota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the device networks can be changed on the settings page while the server
// runs. The client must follow at once, in both directions, and an empty set
// must contact nothing (the state of a fresh install).
func TestClientFollowsTheNetworkSetLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Status":{"Topic":"x"}}`))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := strings.Cut(addr, ":")
	set := NewNetworkSet(nil)
	c := NewHTTPClient(Options{Networks: set})
	target := TargetFor(model.Device{ID: 1, Address: host, Port: atoiOrZero(port)})
	call := func() error {
		_, err := c.Command(context.Background(), target, "Status")
		return err
	}
	if err := call(); err == nil {
		t.Fatal("an empty set must contact nothing")
	}
	set.Set([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if err := call(); err != nil {
		t.Fatalf("after adding the network: %v", err)
	}
	set.Set([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if err := call(); err == nil {
		t.Fatal("after removing the network the device must be refused again")
	}
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// Why: an allowlist entry that covers everything must never unlock the ranges that
// are blocked for good (loopback and link-local only through a narrow entry; cloud
// metadata, NAT64, multicast, unspecified never).
func TestWideNetworkSetStillBlocksTheAlwaysBlockedRanges(t *testing.T) {
	set := NewNetworkSet([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")})
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "169.254.1.1", "fe80::1", "64:ff9b::a9fe:a9fe", "0.0.0.0", "224.0.0.1", "::10.0.0.1"} {
		if IPAllowed(netip.MustParseAddr(ip), set.Get()) {
			t.Errorf("%s must stay blocked", ip)
		}
	}
	if !IPAllowed(netip.MustParseAddr("172.31.13.5"), set.Get()) {
		t.Error("an ordinary address is allowed by a covering entry")
	}
	var unset *NetworkSet
	if IPAllowed(netip.MustParseAddr("172.31.13.5"), unset.Get()) {
		t.Error("an unset set means no device access")
	}
}
