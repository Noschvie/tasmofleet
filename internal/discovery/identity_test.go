package discovery

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// fakeResolver answers LookupNetIP from a table; no real DNS in tests.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]netip.Addr
	calls int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if a, ok := r.hosts[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func resolverEnv(t *testing.T, hosts map[string]string) (*env, *fakeResolver) {
	t.Helper()
	r := &fakeResolver{hosts: map[string][]netip.Addr{}}
	for h, ip := range hosts {
		r.hosts[h] = []netip.Addr{netip.MustParseAddr(ip)}
	}
	return newEnv(t, "172.31.0.0/16", func(o *Options) { o.Resolver = r }), r
}

func auditCount(e *env, action string) int {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	n := 0
	for _, ev := range e.audit.ev {
		if ev.Action == action {
			n++
		}
	}
	return n
}

const lampMAC = "02:00:5e:d2:2c:14"

// Why: the live bug. A device stored by hostname is seen at the IP the
// hostname resolves to; that is the same device at the same place, so
// nothing may happen: no update, no proposal, and above all no audit row
// (the scheduled scan wrote 255 refused rows).
func TestHostnameResolvingToDiscoveredIPIsANoOp(t *testing.T) {
	e, r := resolverEnv(t, map[string]string{"lamp.example.internal": "172.31.13.31"})
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "lamp.example.internal", MAC: lampMAC})
	before := len(e.audit.ev)
	for i := 0; i < 3; i++ {
		out, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.31"), port: 80, name: "lamp"})
		if err != nil || out != outKnown {
			t.Fatalf("outcome %q err %v", out, err)
		}
	}
	if r.calls == 0 {
		t.Fatal("the hostname was never resolved")
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "lamp.example.internal" {
		t.Fatalf("address = %s", d.Address)
	}
	if len(e.audit.ev) != before {
		t.Fatalf("no-op wrote audit rows: %+v", e.audit.ev[before:])
	}
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: a hostname is the admin's choice. Even when the IP is proven by the
// MAC, discovery must not swap the name for the IP; it offers the change.
func TestIPNeverReplacesHostnameAutomatically(t *testing.T) {
	e, _ := resolverEnv(t, map[string]string{"lamp.example.internal": "172.31.13.31"})
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "lamp.example.internal", MAC: lampMAC})
	e.ver.macs["172.31.13.77"] = lampMAC // proven, and still not applied
	out, err := e.mgr.consider(bg, candidate{via: ViaMQTT, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.77"), name: "lamp"})
	if err != nil || out != outAddressChange {
		t.Fatalf("outcome %q err %v", out, err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "lamp.example.internal" {
		t.Fatalf("hostname replaced: %s", d.Address)
	}
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindAddressChange || p[0].ExistingID != id {
		t.Fatalf("proposals = %+v", p)
	}
	if n := auditCount(e, "device.upsert"); n != 0 {
		t.Fatalf("a suggestion wrote %d refused audit rows", n)
	}
}

// Why: a stored hostname that does not resolve (DNS down) must not make the
// device look new or trigger an update either.
func TestUnresolvableHostnameIsASuggestionNotAnUpdate(t *testing.T) {
	e, _ := resolverEnv(t, nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "lamp.example.internal", MAC: lampMAC})
	out, _ := e.mgr.consider(bg, candidate{via: ViaScan, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.31"), port: 80, name: "lamp"})
	if out != outAddressChange {
		t.Fatalf("outcome %q", out)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "lamp.example.internal" {
		t.Fatalf("address = %s", d.Address)
	}
}

// Why: a real address change of a device with a password is never applied
// by discovery (its password would move to whatever answers). It is a
// suggestion, and repeated sightings leave neither more rows nor audit noise.
func TestCredentialedAddressChangeIsASuggestionWithoutAuditNoise(t *testing.T) {
	e, _ := resolverEnv(t, nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: lampMAC, HTTPUser: "admin", HTTPPassword: "secret"})
	before := len(e.audit.ev)
	for i := 0; i < 5; i++ {
		out, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.99"), port: 80, name: "lamp"})
		if err != nil || out != outAddressChange {
			t.Fatalf("outcome %q err %v", out, err)
		}
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" {
		t.Fatalf("moved to %s", d.Address)
	}
	if p := mustProposals(t, e); len(p) != 1 {
		t.Fatalf("proposals = %+v", p)
	}
	if len(e.audit.ev) != before {
		t.Fatalf("suggestions wrote audit rows: %+v", e.audit.ev[before:])
	}
}

// Why: an unverifiable move of a device without password is refused by the
// inventory; the refusal is worth one audit row per day, not one per scan.
func TestRefusedMoveAttemptsAreAuditedOncePerDay(t *testing.T) {
	e, _ := resolverEnv(t, nil)
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: lampMAC}) //nolint:errcheck // setup
	// nothing answers at .99: the move cannot be verified
	for i := 0; i < 5; i++ {
		out, _ := e.mgr.consider(bg, candidate{via: ViaScan, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.99"), port: 80, name: "lamp"})
		if out != outAddressChange {
			t.Fatalf("outcome %q", out)
		}
	}
	if n := auditCount(e, "device.upsert"); n != 1 {
		t.Fatalf("%d refused audit rows, want 1", n)
	}
}

// Why: a web user without a password is no credential (spec 11.1); otherwise
// every device imported from TasmoAdmin would be pinned to its address.
func TestUserWithoutPasswordDoesNotBlockProvenMove(t *testing.T) {
	e, _ := resolverEnv(t, nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: lampMAC, HTTPUser: "admin"})
	e.ver.macs["172.31.13.99"] = lampMAC
	out, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: lampMAC, ip: netip.MustParseAddr("172.31.13.99"), port: 80, name: "lamp"})
	if err != nil || out != outMoved {
		t.Fatalf("outcome %q err %v", out, err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.99" {
		t.Fatalf("address = %s", d.Address)
	}
	if err := MoveRefusal(model.Device{MAC: lampMAC, HTTPUser: "admin"}); err != nil {
		t.Fatalf("MoveRefusal for a user without password: %v", err)
	}
}

// Why: identity is the MAC set. A known device found by IP, by its Ethernet
// MAC, or listed as "new" from an older sighting must never show up as new.
func TestKnownDeviceIsNeverListedAsNew(t *testing.T) {
	const eth = "02:00:5e:43:3e:52"
	e, _ := resolverEnv(t, nil)
	// An old "new" proposal from before the device was added.
	if err := e.mgr.propose(bg, candidate{via: ViaMQTT, mac: eth, ip: netip.MustParseAddr("172.31.13.31"), name: "lamp"}, "new", 0, "not in the inventory"); err != nil {
		t.Fatal(err)
	}
	if p := mustProposals(t, e); len(p) != 1 {
		t.Fatalf("setup: %+v", p)
	}
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: lampMAC, MACs: []string{eth}}) //nolint:errcheck // setup
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("known device (by Ethernet MAC) still listed: %+v", p)
	}
	for _, mac := range []string{lampMAC, eth} {
		out, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: mac, ip: netip.MustParseAddr("172.31.13.31"), port: 80, name: "lamp"})
		if err != nil || out != outKnown {
			t.Fatalf("MAC %s: outcome %q err %v", mac, out, err)
		}
	}
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: a scan reports both interface MACs of an Ethernet-capable ESP32; if
// only the Ethernet one is in the inventory the device is still known.
func TestScanCandidateMatchesOnAnyReportedMAC(t *testing.T) {
	const wifi, eth = "02:00:5e:d2:2c:14", "02:00:5e:43:3e:52"
	e, _ := resolverEnv(t, nil)
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: eth}) //nolint:errcheck // setup
	out, err := e.mgr.consider(bg, candidate{via: ViaScan, mac: wifi, macs: []string{eth}, ip: netip.MustParseAddr("172.31.13.31"), port: 80, name: "lamp"})
	if err != nil || out != outKnown {
		t.Fatalf("outcome %q err %v", out, err)
	}
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: "Ignore" must not be a trap. An ignored device can be listed and
// brought back, so an admin who ignored the wrong one can fix it.
func TestIgnoredProposalCanBeShownAndUnignored(t *testing.T) {
	e, _ := resolverEnv(t, nil)
	if err := e.mgr.propose(bg, candidate{via: ViaMQTT, mac: "02:00:5e:40:b4:34", ip: netip.MustParseAddr("172.31.13.77"), name: "new"}, "new", 0, "not in the inventory"); err != nil {
		t.Fatal(err)
	}
	p := mustProposals(t, e)
	if err := e.mgr.Dismiss(bg, "admin", p[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := mustProposals(t, e); len(got) != 0 {
		t.Fatalf("still visible: %+v", got)
	}
	ign, err := e.mgr.Ignored(bg)
	if err != nil || len(ign) != 1 || ign[0].ID != p[0].ID {
		t.Fatalf("ignored = %+v, %v", ign, err)
	}
	if err := e.mgr.Undismiss(bg, "admin", p[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := mustProposals(t, e); len(got) != 1 {
		t.Fatalf("not visible after un-ignore: %+v", got)
	}
	if ign, _ := e.mgr.Ignored(bg); len(ign) != 0 {
		t.Fatalf("still ignored: %+v", ign)
	}
	if !e.audit.has("discovery.undismiss") {
		t.Error("un-ignore not audited")
	}
	if err := e.mgr.Undismiss(bg, "admin", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// auditDetail counts the audit rows of action whose detail contains needle.
func auditDetail(e *env, action, needle string) int {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	n := 0
	for _, ev := range e.audit.ev {
		if ev.Action == action && strings.Contains(ev.Detail, needle) {
			n++
		}
	}
	return n
}
