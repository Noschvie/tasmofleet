package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

var bg = context.Background()

type auditLog struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *auditLog) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	a.ev = append(a.ev, e)
	a.mu.Unlock()
}

func (a *auditLog) has(action string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.ev {
		if e.Action == action {
			return true
		}
	}
	return false
}

// macVerifier answers like a network of unauthenticated devices: ip -> MAC.
type macVerifier struct {
	macs  map[string]string
	calls atomic.Int32
}

func (v *macVerifier) VerifyMAC(_ context.Context, address string, _ int) (string, error) {
	v.calls.Add(1)
	if m, ok := v.macs[address]; ok {
		return m, nil
	}
	return "", errors.New("no answer")
}

type env struct {
	st    *store.SQLite
	inv   *inventory.Manager
	mgr   *Manager
	ver   *macVerifier
	audit *auditLog
}

func newEnv(t *testing.T, nets string, mut func(*Options)) *env {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // temp DB
	e := &env{st: st, ver: &macVerifier{macs: map[string]string{}}, audit: &auditLog{}}
	prefix := netip.MustParsePrefix(nets)
	e.inv, err = inventory.New(inventory.Options{Store: st, Audit: e.audit, Networks: []netip.Prefix{prefix}, Verifier: e.ver})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Inventory: e.inv, Store: st, Audit: e.audit, Networks: []netip.Prefix{prefix}, Verifier: e.ver}
	if mut != nil {
		mut(&o)
	}
	if e.mgr, err = New(o); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { _ = e.mgr.Run(ctx); close(done) }() // returns ctx.Err() on shutdown
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 500; i++ { // Run publishes its context first
		e.mgr.smu.Lock()
		ok := e.mgr.base != nil
		e.mgr.smu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("manager did not start")
}

func discoveryMsg(mac, ip, topic string) mqttbus.Message {
	b, _ := json.Marshal(map[string]any{"ip": ip, "dn": "Lamp " + topic, "hn": topic, "mac": mac, "t": topic, "ft": "%prefix%/%topic%/"})
	return mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: mac, Payload: b, Retained: true}
}

func mustProposals(t *testing.T, e *env) []Proposal {
	t.Helper()
	p, err := e.mgr.Proposals(bg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Why: the captured retained discovery messages of the real fleet must turn
// into one "new" proposal per device, with the topic and name the device
// announced, and nothing else may be created.
func TestFixtureReplayYieldsOneNewProposalPerDevice(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	f, err := os.Open("../../testdata/fixtures/mqtt/discovery-retained.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only fixture
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	n := 0
	for sc.Scan() {
		var h struct{ MAC string }
		if err := json.Unmarshal([]byte(sc.Text()), &h); err != nil {
			t.Fatal(err)
		}
		e.mgr.handleMessage(bg, mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: h.MAC, Payload: []byte(sc.Text()), Retained: true})
		n++
	}
	props := mustProposals(t, e)
	if n != 20 || len(props) != 20 {
		t.Fatalf("%d messages gave %d proposals, want 20", n, len(props))
	}
	for _, p := range props {
		if p.Kind != KindNew || p.Via != ViaMQTT || p.Device.Topic == "" || p.Device.MAC == "" || p.Device.HTTPPassword != "" {
			t.Errorf("unexpected proposal %+v", p)
		}
	}
	if devs, _ := e.inv.List(bg); len(devs) != 0 {
		t.Errorf("discovery created %d devices on its own; adding is an admin action", len(devs))
	}
}

// Why: a retained message replays at every reconnect; a device that is
// already in the inventory must not come back as a proposal.
func TestKnownDeviceProducesNoProposal(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: DHCP drift. A device without credentials whose MAC is proven at the
// new address is moved by inventory.Upsert; no admin work needed.
func TestProvenMoveOfUncredentialedDeviceIsApplied(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	e.ver.macs["172.31.13.77"] = "02:00:5e:d2:2c:14"
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.77", "lamp"))
	d, _ := e.inv.Get(bg, id)
	if d.Address != "172.31.13.77" {
		t.Fatalf("address = %s", d.Address)
	}
	if p := mustProposals(t, e); len(p) != 0 {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: a spoofed discovery message that claims the MAC of a credentialed
// device at an attacker's address must not move it (the poller would send
// the stored password there). It becomes a proposal for the admin.
func TestSpoofedMoveOfCredentialedDeviceBecomesAddressChange(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp", HTTPUser: "admin", HTTPPassword: "secret"})
	e.ver.macs["172.31.13.99"] = "02:00:5e:d2:2c:14" // the rogue host happily claims it
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.99", "lamp"))
	d, _ := e.inv.Get(bg, id)
	if d.Address != "172.31.13.31" || d.HTTPPassword != "secret" {
		t.Fatalf("device moved or changed: %+v", d)
	}
	if e.ver.calls.Load() != 0 {
		t.Error("a credentialed device must not even be probed")
	}
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindAddressChange || p[0].ExistingID != id || p[0].Device.Address != "172.31.13.99" {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why: another MAC claiming the topic of a known device is a conflict; the
// record must stay as it is and the proposal cannot be accepted blindly.
func TestForeignMACClaimingKnownTopicIsConflict(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	e.mgr.handleMessage(bg, discoveryMsg("02005E320B94", "172.31.13.50", "lamp"))
	d, _ := e.inv.Get(bg, id)
	if d.MAC != "02:00:5e:d2:2c:14" || d.Address != "172.31.13.31" {
		t.Fatalf("record changed: %+v", d)
	}
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindConflict {
		t.Fatalf("proposals = %+v", p)
	}
	if _, err := e.mgr.Accept(bg, "admin", p[0].ID); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("accept conflict: %v", err)
	}
}

// Why: discovery data is untrusted; what cannot be a device of ours, or is
// internally inconsistent, is dropped without a trace in the proposal list.
func TestUnusableMessagesAreDropped(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	good := discoveryMsg("02005ED22C14", "172.31.13.31", "lamp")
	cases := map[string]mqttbus.Message{
		"outside device networks": discoveryMsg("02005E433E52", "10.1.1.1", "x"),
		"metadata address":        discoveryMsg("02005E8B3EDF", "169.254.169.254", "x"),
		"topic MAC differs":       {Kind: mqttbus.KindDiscovery, Topic: "02005E61F031", Payload: good.Payload},
		"not JSON":                {Kind: mqttbus.KindDiscovery, Topic: "02005E461CEE", Payload: []byte("nope")},
		"oversized":               {Kind: mqttbus.KindDiscovery, Topic: "02005E33FFD9", Payload: []byte(`{"mac":"02005E33FFD9","ip":"172.31.1.1","dn":"` + strings.Repeat("a", maxDiscoveryPayload) + `"}`)},
		"bad MAC":                 {Kind: mqttbus.KindDiscovery, Topic: "zz", Payload: []byte(`{"mac":"zz","ip":"172.31.1.1"}`)},
	}
	for name, m := range cases {
		e.mgr.handleMessage(bg, m)
		if p := mustProposals(t, e); len(p) != 0 {
			t.Fatalf("%s produced %+v", name, p)
		}
	}
}

// Why: names and topics come from the network. Control characters and
// slashes must not reach the inventory form.
func TestClaimFieldsAreSanitised(t *testing.T) {
	m := discoveryMsg("02005ED22C14", "172.31.13.31", "a/b#c")
	var raw map[string]any
	_ = json.Unmarshal(m.Payload, &raw) // test fixture built above
	raw["dn"] = "  Evil\x00\n" + strings.Repeat("x", 300)
	m.Payload, _ = json.Marshal(raw)
	c, err := parseDiscovery(m.Topic, m.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if c.topic != "" || strings.ContainsAny(c.name, "\x00\n") || len([]rune(c.name)) > maxNameRunes {
		t.Fatalf("candidate not sanitised: %+v", c)
	}
}

// Why: one publisher repeating a message must not turn into a probe stream.
func TestRepeatedMessageOfOneMACIsRateLimited(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14"}) //nolint:errcheck // setup
	for i := 0; i < 5; i++ {
		e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.77", "lamp"))
	}
	if n := e.ver.calls.Load(); n != 1 {
		t.Fatalf("verifier called %d times, want 1", n)
	}
}

// Why: the bus handler must not block and must still end in proposals.
func TestQueuedMessagesAreProcessedByRun(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.run(t)
	e.mgr.HandleMQTT(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.mgr.HandleMQTT(discoveryMsg("02005ED22C14", "172.31.13.31", "lamp")) // coalesced
	waitFor(t, func() bool { return len(mustProposals(t, e)) == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// Why: accepting must run the inventory validation and audit, and the
// result must be a device the admin could have added by hand.
func TestAcceptNewCreatesDeviceAndClearsProposal(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	p := mustProposals(t, e)
	id, err := e.mgr.Accept(bg, "admin", p[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.inv.Get(bg, id)
	if err != nil || d.MAC != "02:00:5e:d2:2c:14" || d.Topic != "lamp" || d.Address != "172.31.13.31" {
		t.Fatalf("device = %+v, %v", d, err)
	}
	if len(mustProposals(t, e)) != 0 || !e.audit.has("discovery.accept") || !e.audit.has("device.create") {
		t.Error("proposal not cleared or accept not audited")
	}
	if _, err := e.mgr.Accept(bg, "admin", p[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second accept: %v", err)
	}
}

// Why: between proposal and accept the address may have been reassigned; a
// different MAC there means the proposal describes the wrong hardware.
func TestAcceptNewRefusesWhenAddressNowHostsOtherMAC(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	e.ver.macs["172.31.13.31"] = "02:00:5e:79:43:7d"
	p := mustProposals(t, e)
	if _, err := e.mgr.Accept(bg, "admin", p[0].ID); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v", err)
	}
	if devs, _ := e.inv.List(bg); len(devs) != 0 {
		t.Fatal("device created from a stale proposal")
	}
}

// Why: "confirm new address": once the new address proves the MAC without
// credentials, the admin's confirmation moves the device and keeps its record.
func TestAcceptAddressChangeMovesDeviceAfterMACProof(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Notes: "keep"})
	// a credential-less device is moved by Upsert itself, so force a proposal
	// by letting the verifier fail at first
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Kind != KindAddressChange || p[0].Version == "" {
		t.Fatalf("proposals = %+v", p)
	}
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	got, err := e.mgr.AcceptVersion(bg, "admin", p[0].ID, p[0].Version)
	if err != nil || got != id {
		t.Fatalf("accept = %d, %v", got, err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.40" || d.Notes != "keep" {
		t.Fatalf("device = %+v", d)
	}
}

// credentialedProposal sets up the spoof scenario: a credentialed device and
// a discovery message that claims its MAC at another address.
func credentialedProposal(t *testing.T) (*env, model.DeviceID, Proposal) {
	t.Helper()
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", HTTPUser: "admin", HTTPPassword: "secret"})
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	p := mustProposals(t, e)
	if len(p) != 1 {
		t.Fatalf("proposals = %+v", p)
	}
	return e, id, p[0]
}

// Why: TOCTOU. The admin confirms what was shown. A newer sighting that
// claims another address is a new version of the candidate; the old
// confirmation must not apply it.
func TestConfirmOfReplacedCandidateIsStale(t *testing.T) {
	e, id, shown := credentialedProposal(t)
	e.mgr.last = map[string]time.Time{} // rate limit is not under test
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.66", "lamp"))
	e.ver.macs["172.31.13.66"] = "02:00:5e:d2:2c:14"
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	if _, err := e.mgr.AcceptVersion(bg, "admin", shown.ID, shown.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" {
		t.Fatalf("device moved to %s", d.Address)
	}
	cur := mustProposals(t, e)
	if len(cur) != 1 || cur[0].Version == shown.Version || cur[0].Device.Address != "172.31.13.66" {
		t.Fatalf("proposal after replacement = %+v", cur)
	}
}

// Why: a sighting with unchanged content (retained replay) must not
// invalidate what the admin is looking at.
func TestResightingWithSameContentKeepsVersion(t *testing.T) {
	e, _, shown := credentialedProposal(t)
	e.mgr.last = map[string]time.Time{}
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	if cur := mustProposals(t, e); len(cur) != 1 || cur[0].Version != shown.Version {
		t.Fatalf("version changed: %+v", cur)
	}
}

// plainProposal is an address-change proposal for a bound device without credentials.
func plainProposal(t *testing.T) (*env, model.DeviceID, Proposal) {
	t.Helper()
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14"})
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	p := mustProposals(t, e)
	if len(p) != 1 {
		t.Fatalf("proposals = %+v", p)
	}
	return e, id, p[0]
}

// Why: the confirmed device must still be the one that was shown: if it was
// rebound or deleted while the proposal waited, nothing is applied.
func TestAcceptMoveRefusedWhenDeviceChangedOrGone(t *testing.T) {
	e, id, p := plainProposal(t)
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	d, _ := e.inv.Get(bg, id)
	d.MAC = "02:00:5e:40:b4:34"
	if err := e.inv.Update(bg, "t", d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.AcceptVersion(bg, "admin", p.ID, p.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("rebound: err = %v", err)
	}
	if err := e.inv.Delete(bg, "t", id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.AcceptVersion(bg, "admin", p.ID, p.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("deleted: err = %v", err)
	}
}

// Why: with credentials stored, the probe must not carry them: the verifier
// is the credential-less path, and a missing verifier means no discovery move.
func TestAcceptMoveNeedsVerifier(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", func(o *Options) { o.Verifier = nil })
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14"}) //nolint:errcheck // setup
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	p := mustProposals(t, e)
	if len(p) != 1 {
		t.Fatalf("proposals = %+v", p)
	}
	if _, err := e.mgr.Accept(bg, "admin", p[0].ID); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("err = %v", err)
	}
}

func TestDismissHidesProposalAndSurvivesReplay(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	p := mustProposals(t, e)
	if err := e.mgr.Dismiss(bg, "admin", p[0].ID); err != nil {
		t.Fatal(err)
	}
	e.mgr.last = map[string]time.Time{} // let the replay through the rate limit
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	if len(mustProposals(t, e)) != 0 {
		t.Fatal("dismissed device came back after a retained replay")
	}
	if err := e.mgr.Dismiss(bg, "admin", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// --- scan ---

func TestPlanScanRules(t *testing.T) {
	nets := []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}
	pp := func(ss ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, s := range ss {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}
	// Why each: outside networks must be refused before a packet; the size
	// cap bounds a scan; wide ranges beyond the networks cannot sneak in.
	for name, c := range map[string][]netip.Prefix{
		"outside":            pp("10.0.0.0/24"),
		"straddles network":  pp("172.0.0.0/8"),
		"1025 addresses":     pp("172.31.0.0/22", "172.31.4.0/32"),
		"too wide":           pp("172.31.0.0/21"),
		"none":               nil,
		"one outside of two": pp("172.31.1.0/24", "192.168.0.0/30"),
		"ipv6 outside":       pp("fd00::/120"),
	} {
		if _, err := planScan(c, nets); !errors.Is(err, ErrScanRange) {
			t.Errorf("%s: err = %v, want ErrScanRange", name, err)
		}
	}
	got, err := planScan(pp("172.31.1.0/24"), nets)
	if err != nil || len(got) != 254 || got[0].String() != "172.31.1.1" || got[253].String() != "172.31.1.254" {
		t.Fatalf("/24: %d addresses, %v", len(got), err)
	}
	if got, _ := planScan(pp("172.31.0.0/22"), nets); len(got) != 1022 {
		t.Errorf("/22 = %d addresses, want 1022 (the maximum minus network and broadcast)", len(got))
	}
	if got, _ := planScan(pp("172.31.1.0/30", "172.31.1.0/30"), nets); len(got) != 2 {
		t.Errorf("overlap: %v", got)
	}
	if got, err := planScan(pp("172.31.1.5/32"), nets); err != nil || len(got) != 1 {
		t.Errorf("/32: %v %v", got, err)
	}
	// loopback is skipped unless a device network names it explicitly
	if _, err := planScan(pp("127.0.0.0/30"), []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}); err != nil {
		t.Errorf("explicit loopback network: %v", err)
	}
}

func tasmotaHandler(hits *atomic.Int32, mac string, delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Query().Get("cmnd") {
		case "Status 2":
			_, _ = w.Write([]byte(`{"StatusFWR":{"Version":"16.0.0(release-tasmota)","Hardware":"ESP8266EX"}}`))
		case "Status 5":
			_, _ = w.Write([]byte(`{"StatusNET":{"Hostname":"scanned-lamp","Mac":"` + mac + `"}}`))
		default:
			_, _ = w.Write([]byte(`{"Command":"Unknown"}`))
		}
	})
}

func scanEnv(t *testing.T, srv *httptest.Server, nets string) *env {
	t.Helper()
	port := mustPort(t, srv)
	return newEnv(t, nets, func(o *Options) {
		o.Client = tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: o.Networks, MaxConcurrent: 16})
		o.Ports = []int{port}
		o.Verifier = nil
		o.ProbeTimeout = 500 * time.Millisecond
	})
}

func mustPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://")
	p := u[strings.LastIndex(u, ":")+1:]
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return n
}

// Why: the range check is the SSRF boundary of the scan: a CIDR outside
// DEVICE_NETWORKS must be refused before any packet leaves, which the
// listener (inside the allowed loopback range, but not in the scanned range)
// proves by staying untouched.
func TestScanOutsideNetworksSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5e:3a:ce:55", 0))
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.1/32")
	e.run(t)
	for _, c := range []string{"127.0.0.0/24", "127.0.0.0/8", "0.0.0.0/0"} {
		if _, err := e.mgr.StartScan(bg, "admin", []netip.Prefix{netip.MustParsePrefix(c)}); !errors.Is(err, ErrScanRange) {
			t.Errorf("%s: err = %v", c, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("listener got %d requests from a refused scan", hits.Load())
	}
}

// Why: a scan finds the device, records it as a proposal with the address
// that answered (not an address the device claims), and reports progress.
func TestScanFindsDeviceAndProposesIt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5E:3A:CE:55", 0))
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.0/8")
	e.run(t)
	id, err := e.mgr.StartScan(bg, "admin", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { st, _ := e.mgr.Scan(id); return st.State == ScanDone })
	st, _ := e.mgr.Scan(id)
	if st.Done != st.Total || len(st.Hits) != 1 || st.Hits[0].Outcome != outNew || st.Hits[0].MAC != "02:00:5e:3a:ce:55" {
		t.Fatalf("status = %+v", st)
	}
	p := mustProposals(t, e)
	if len(p) != 1 || p[0].Via != ViaScan || p[0].Device.Address != "127.0.0.1" || p[0].Device.Port != mustPort(t, srv) || p[0].Device.Name != "scanned-lamp" {
		t.Fatalf("proposals = %+v", p)
	}
	did, err := e.mgr.Accept(bg, "admin", p[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := e.inv.Get(bg, did); d.Port != mustPort(t, srv) {
		t.Errorf("accepted device = %+v", d)
	}
	for _, a := range []string{"discovery.scan_start", "discovery.scan_end"} {
		if !e.audit.has(a) {
			t.Errorf("no audit event %s", a)
		}
	}
}

// Why: concurrency and the retry are bounded: 16 probes at most in flight,
// and a timeout is retried exactly once.
func TestScanBoundsConcurrencyAndRetriesOnce(t *testing.T) {
	l, err := net.Listen("tcp", "0.0.0.0:0") // answers on every 127.x address
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never answers within the probe timeout
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.0/8")
	// Count on the client side: the server notices an abandoned request a moment late.
	cc := &countingClient{Client: e.mgr.o.Client}
	e.mgr.o.Client = cc
	e.mgr.o.ProbeTimeout = 50 * time.Millisecond
	e.run(t)
	id, err := e.mgr.StartScan(bg, "admin", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/26")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { st, _ := e.mgr.Scan(id); return st.State == ScanDone })
	if cc.peak.Load() > 16 {
		t.Errorf("peak concurrency %d > 16", cc.peak.Load())
	}
	if cc.peak.Load() < 2 {
		t.Errorf("peak concurrency %d: the scan is not concurrent", cc.peak.Load())
	}
	if cc.total.Load() != 62*2 {
		t.Errorf("requests = %d, want %d (one retry per timeout)", cc.total.Load(), 62*2)
	}
}

type countingClient struct {
	tasmota.Client
	cur, peak, total atomic.Int32
}

func (c *countingClient) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	c.total.Add(1)
	n := c.cur.Add(1)
	defer c.cur.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return c.Client.Command(ctx, t, cmnd)
}

// Why: an admin who cancels a scan of a dead range must not wait for every
// probe timeout.
func TestCancelStopsScanQuickly(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5e:3a:ce:55", 30*time.Second))
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.0/8")
	e.mgr.o.ProbeTimeout = 30 * time.Second
	e.run(t)
	id, err := e.mgr.StartScan(bg, "admin", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hits.Load() > 0 })
	start := time.Now()
	if err := e.mgr.CancelScan(bg, "admin", id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { st, _ := e.mgr.Scan(id); return st.State == ScanCancelled })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

func TestOnlyOneScanAtATime(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5e:3a:ce:55", 5*time.Second))
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.0/8")
	e.run(t)
	c := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	id, err := e.mgr.StartScan(bg, "admin", c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.StartScan(bg, "admin", c); !errors.Is(err, ErrScanRunning) {
		t.Fatalf("err = %v", err)
	}
	_ = e.mgr.CancelScan(bg, "admin", id) // test cleanup
}

func TestStartScanBeforeRunFails(t *testing.T) {
	e := newEnv(t, "127.0.0.0/8", func(o *Options) { o.Client = tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: o.Networks}) })
	if _, err := e.mgr.StartScan(bg, "a", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

// Why: a scan hit whose MAC belongs to a credentialed device at another
// address goes through the same identity rules as MQTT: no silent move.
func TestScanHitOfKnownCredentialedDeviceIsAddressChange(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(tasmotaHandler(&hits, "02:00:5e:3a:ce:55", 0))
	defer srv.Close()
	e := scanEnv(t, srv, "127.0.0.0/8")
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "127.0.0.9", MAC: "02:00:5e:3a:ce:55", HTTPPassword: "pw"})
	e.run(t)
	sid, _ := e.mgr.StartScan(bg, "admin", []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	waitFor(t, func() bool { st, _ := e.mgr.Scan(sid); return st.State == ScanDone })
	if d, _ := e.inv.Get(bg, id); d.Address != "127.0.0.9" {
		t.Fatalf("device moved to %s", d.Address)
	}
	if p := mustProposals(t, e); len(p) != 1 || p[0].Kind != KindAddressChange {
		t.Fatalf("proposals = %+v", p)
	}
}

// Why (N20): the fleet default password chosen at accept goes into the new
// device (user empty = Tasmota's admin). An address change never takes it: the
// device keeps its own stored password, and the move must not hand a
// credential to a host it was not entered for.
func TestAcceptWithDefaultPassword(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.ver.macs["172.31.13.31"] = "02:00:5e:d2:2c:14"
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
	p := mustProposals(t, e)
	id, err := e.mgr.AcceptVersion(WithDevicePassword(bg, "fleet-fake-pw"), "admin", p[0].ID, p[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := e.inv.Get(bg, id); d.HTTPPassword != "fleet-fake-pw" || d.HTTPUser != "" {
		t.Fatalf("new device credentials: user %q, password set %v", d.HTTPUser, d.HTTPPassword != "")
	}

	e = newEnv(t, "172.31.0.0/16", nil)
	id, _ = e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14"})
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	p = mustProposals(t, e)
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	if _, err := e.mgr.AcceptVersion(WithDevicePassword(bg, "fleet-fake-pw"), "admin", p[0].ID, p[0].Version); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.inv.Get(bg, id); d.HTTPPassword != "" || d.Address != "172.31.13.40" {
		t.Fatalf("a move must not take the default password: %+v", d.Address)
	}
}

// Why (N20, security review 2026-10-22): the address of a proposal comes from
// a sighting anyone on the network can fake. The fleet-wide default password
// is used by every device, so it may only be stored for a host that proved
// the proposal's MAC; otherwise it would go via basic auth to an impostor at
// the next poll. Without the default the accept stays possible (nothing
// secret is sent).
func TestAcceptWithDefaultPasswordNeedsAProvenMAC(t *testing.T) {
	for name, opt := range map[string]func(*Options){
		"no answer":   nil,
		"no verifier": func(o *Options) { o.Verifier = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "172.31.0.0/16", opt)
			e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.31", "lamp"))
			p := mustProposals(t, e)
			if _, err := e.mgr.AcceptVersion(WithDevicePassword(bg, "fleet-fake-pw"), "admin", p[0].ID, p[0].Version); !errors.Is(err, ErrNotAcceptable) {
				t.Fatalf("accept with the default for an unproven host: %v, want ErrNotAcceptable", err)
			}
			if devs, _ := e.inv.List(bg); len(devs) != 0 {
				t.Fatalf("a device was created: %+v", devs)
			}
			id, err := e.mgr.AcceptVersion(bg, "admin", p[0].ID, p[0].Version)
			if err != nil {
				t.Fatalf("accept without the default: %v", err)
			}
			if d, _ := e.inv.Get(bg, id); d.HTTPPassword != "" {
				t.Error("the device got a password")
			}
		})
	}
}

// Why: the Add devices page tells an admin since when a device has been
// announcing itself ("First seen"); a later sighting must not reset it, or a
// device that keeps announcing would always look brand new.
func TestProposalKeepsItsFirstSighting(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.40", "lamp"))
	first := mustProposals(t, e)
	if len(first) != 1 || first[0].FirstSeen.IsZero() {
		t.Fatalf("proposal has no first sighting: %+v", first)
	}
	time.Sleep(5 * time.Millisecond) // timestamps are milliseconds
	e.mgr.last = map[string]time.Time{}
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.41", "lamp"))
	again := mustProposals(t, e)
	if len(again) != 1 || !again[0].FirstSeen.Equal(first[0].FirstSeen) || !again[0].SeenAt.After(first[0].SeenAt) {
		t.Fatalf("re-sighting changed the first sighting or not the last one: before %+v, after %+v", first[0], again[0])
	}
}
