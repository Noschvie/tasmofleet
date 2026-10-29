package discovery

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: #130 asks for the device's real hostname in the New devices list; each of the three ways
// discovery learns a device must keep it, and a device that reports none must leave it empty
// (the list shows a dash) instead of showing the MQTT topic as a hostname.
func TestDiscoveryConfigFillsTheHostname(t *testing.T) {
	c, err := parseDiscovery("02005ED22C14", discoveryMsg("02005ED22C14", "172.31.13.31", "lamp").Payload)
	if err != nil || c.host != "lamp" {
		t.Fatalf("host = %q, err %v", c.host, err)
	}
	b, _ := json.Marshal(map[string]any{"ip": "172.31.13.31", "dn": "Lamp", "mac": "02005ED22C14", "t": "lamp_topic"})
	if c, err = parseDiscovery("02005ED22C14", b); err != nil || c.host != "" {
		t.Fatalf("no hn: host = %q, err %v (the topic is no hostname)", c.host, err)
	}
}

func TestStatus5FillsTheHostname(t *testing.T) {
	c, err := parseStatus5("lamp_topic", []byte(`{"StatusNET":{"Hostname":"tasmota-lamp-1234","IPAddress":"172.31.13.31","Mac":"02:00:5E:D2:2C:14"}}`))
	if err != nil || c.host != "tasmota-lamp-1234" {
		t.Fatalf("host = %q, err %v", c.host, err)
	}
	if c, err = parseStatus5("lamp_topic", []byte(`{"StatusNET":{"IPAddress":"172.31.13.31","Mac":"02:00:5E:D2:2C:14"}}`)); err != nil || c.host != "" {
		t.Fatalf("no hostname: host = %q, err %v", c.host, err)
	}
}

// Why: a hostname comes from the network; control characters must not reach the page.
func TestHostnameIsSanitised(t *testing.T) {
	if got := cleanHostname("  evil\x00\u202e-host\n"); got != "evil-host" {
		t.Fatalf("got %q", got)
	}
	if cleanHostname("   ") != "" {
		t.Fatal("blank hostname must stay empty")
	}
}

func TestScanFillsTheHostname(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5e:aa:bb:01", 0))
	t.Cleanup(srv.Close)
	e := scanEnv(t, srv, "127.0.0.0/8")
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if _, ok := e.mgr.probe(bg, probeTarget{ip: netip.MustParseAddr(host), port: mustPort(t, srv)}); !ok {
		t.Fatal("the test device was not found")
	}
	props := mustProposals(t, e)
	if len(props) != 1 || props[0].Device.Net.Hostname != "scanned-lamp" {
		t.Fatalf("proposals = %+v", props)
	}
}

// Why: the hostname is display only. If it were part of the version, a device that merely
// reports it later would turn every confirmation an admin was looking at stale.
func TestHostnameIsNotPartOfTheVersion(t *testing.T) {
	r := store.ProposalRow{Kind: store.ProposalNew}
	r.Device.MAC, r.Device.Address, r.Device.Name = "02:00:5e:d2:2c:14", "172.31.13.31", "lamp"
	before := versionOf(r)
	r.Device.Net.Hostname = "tasmota-1234"
	if versionOf(r) != before {
		t.Fatal("a hostname changed the proposal's version")
	}
}

// Why: a later sighting without a hostname (a scan hit that could not read Status 5) must not
// erase the one MQTT discovery reported.
func TestLaterSightingKeepsTheKnownHostname(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02BBBBBBBBBB", "172.31.13.77", "x"))
	if _, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: "02:bb:bb:bb:bb:bb", ip: netip.MustParseAddr("172.31.13.77"), port: 80, name: "x"}); err != nil {
		t.Fatal(err)
	}
	if p := mustProposals(t, e); len(p) != 1 || p[0].Device.Net.Hostname != "x" {
		t.Fatalf("proposals = %+v", p)
	}
}
