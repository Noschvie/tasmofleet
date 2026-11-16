package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// whyEnv is the lookup of #713 on the real inventory, the real store and the real discovery manager:
// the answer is computed from what those three hold, so a fake that returns a fixed answer could not
// show that the page names the right record.
type whyEnv struct {
	*env
	st  *store.SQLite
	inv *inventory.Manager
	mgr *discovery.Manager
	net *whyNet
}

// whyNet counts everything that would reach a device or the broker: a MAC proof (an HTTP probe), a
// Status 5 question and every publish. The lookup must leave all three at zero (#713).
type whyNet struct {
	mu     sync.Mutex
	macs   map[string]string // address -> MAC the "device" there proves
	probes atomic.Int32
	asks   atomic.Int32
	pubs   atomic.Int32
}

func (n *whyNet) VerifyMAC(_ context.Context, address string, _ int) (string, error) {
	n.probes.Add(1)
	n.mu.Lock()
	defer n.mu.Unlock()
	if m, ok := n.macs[address]; ok {
		return m, nil
	}
	return "", errors.New("no answer")
}

func (n *whyNet) Do(context.Context, model.Device, string) (json.RawMessage, error) {
	n.asks.Add(1)
	return nil, errors.New("the lookup must not ask a device")
}

func (n *whyNet) Subscribe([]mqttbus.Kind, func(mqttbus.Message)) func() { return func() {} }
func (n *whyNet) Command(context.Context, string, string, string) error {
	n.pubs.Add(1)
	return nil
}
func (n *whyNet) CommandTopic(topic, command string) string { return "cmnd/" + topic + "/" + command }
func (n *whyNet) Connected() bool                           { return true }

func (n *whyNet) sent() int32 { return n.probes.Load() + n.asks.Load() + n.pubs.Load() }

func newWhyEnv(t *testing.T) *whyEnv {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &fakeAudit{}
	n := &whyNet{macs: map[string]string{}}
	nets := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}
	inv, err := inventory.New(inventory.Options{Store: st, Audit: rec, Networks: nets, Verifier: n})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := discovery.New(discovery.Options{Inventory: inv, Store: st, Audit: rec, Networks: nets, Verifier: n, Asker: n})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Attach(n)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = mgr.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	e := setupWith(t, func(d *Deps) { d.Inventory, d.Discovery = inv, mgr })
	return &whyEnv{env: e, st: st, inv: inv, mgr: mgr, net: n}
}

// waitFor polls cond for at most 5 s (the manager works on its own goroutine).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (w *whyEnv) discoveryConfig(mac, ip, topic, name string) mqttbus.Message {
	return mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: strings.ReplaceAll(mac, ":", ""), Retained: true,
		Payload: []byte(`{"ip":"` + ip + `","dn":"` + name + `","hn":"` + topic + `","mac":"` + mac + `","t":"` + topic + `","ft":"%prefix%/%topic%/"}`)}
}

// lookup is the Add page for ?why=text, as an admin sees it, and what it cost the network.
func (w *whyEnv) lookup(t *testing.T, text string) (status int, body string) {
	t.Helper()
	before := w.net.sent()
	res, body := w.do(t, "GET", "/devices/new?why="+url.QueryEscape(text), "admin", nil)
	if after := w.net.sent(); after != before {
		t.Errorf("the lookup of %q sent %d messages or probes to devices or the broker, want none", text, after-before)
	}
	return res.StatusCode, body
}

func hasAll(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("the page lacks %q", s)
		}
	}
}

// Why (#713): discovery binds a record that has no MAC when a claim at its address proves one. The
// device then keeps its old name and old topic, so it never shows up under New devices and the admin
// sees nothing that says why. The lookup must name that record, whether the admin types the topic the
// device announces now, its address or its MAC.
func TestLookupNamesTheStoredRecordADeviceWasBoundTo(t *testing.T) {
	w := newWhyEnv(t)
	id, err := w.inv.Create(t.Context(), "t", model.Device{Name: "workshop-old", Address: "10.0.0.60", Topic: "oldtopic"})
	if err != nil {
		t.Fatal(err)
	}
	w.net.macs["10.0.0.60"] = "02:00:5e:00:00:07"
	w.mgr.HandleMQTT(w.discoveryConfig("02:00:5e:00:00:07", "10.0.0.60", "newtopic", "Lamp newtopic"))
	waitFor(t, "the record to be bound", func() bool {
		d, err := w.inv.Get(t.Context(), id)
		return err == nil && d.MAC != ""
	})
	if d, _ := w.inv.Get(t.Context(), id); d.Name != "workshop-old" {
		t.Fatalf("the premise of #713 no longer holds, the record is now %q", d.Name)
	}
	if got, _ := w.mgr.Proposals(t.Context()); len(got) != 0 {
		t.Fatalf("the device is offered as new: %+v", got)
	}
	for _, q := range []string{"newtopic", "10.0.0.60", "02:00:5E:00:00:07"} {
		status, body := w.lookup(t, q)
		if status != 200 {
			t.Fatalf("%q: status %d", q, status)
		}
		hasAll(t, body, "workshop-old", "already in your device list", `href="/tf/devices/`+strconv.FormatInt(int64(id), 10)+`"`)
	}
}

// Why (#713): an ignored device is hidden behind a switch, so the admin who looks for it must be told
// that it is ignored and be able to bring it back from the answer, with the version of the row that
// was shown (#745): the number of a proposal can name another device by the time of the click.
func TestLookupOfAnIgnoredDeviceOffersRestoreWithItsVersion(t *testing.T) {
	w := newWhyEnv(t)
	row := store.ProposalRow{Kind: store.ProposalNew, Via: discovery.ViaMQTT, SeenAt: time.Now(),
		Device: model.Device{Name: "porch", Address: "10.0.0.50", MAC: "aa:bb:cc:dd:ee:01", Topic: "porch"}}
	if _, err := w.st.UpsertProposal(t.Context(), row, 0); err != nil {
		t.Fatal(err)
	}
	props, err := w.mgr.Proposals(t.Context())
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals = %+v, %v", props, err)
	}
	if err := w.mgr.Dismiss(t.Context(), "admin", props[0].ID, props[0].Version); err != nil {
		t.Fatal(err)
	}
	ign, _ := w.mgr.Ignored(t.Context())
	id, ver := strconv.FormatInt(ign[0].ID, 10), ign[0].Version
	for _, q := range []string{"10.0.0.50", "AA:BB:CC:DD:EE:01", "porch"} {
		status, body := w.lookup(t, q)
		if status != 200 {
			t.Fatalf("%q: status %d", q, status)
		}
		hasAll(t, body, "porch", "is ignored", `action="/tf/discovery/proposals/`+id+`/undismiss"`, `name="version" value="`+ver+`"`)
	}
	res, _ := w.do(t, "POST", "/discovery/proposals/"+id+"/undismiss", "admin", url.Values{"version": {ver}})
	if res.StatusCode != 303 {
		t.Fatalf("restore: status %d", res.StatusCode)
	}
	if got, _ := w.mgr.Proposals(t.Context()); len(got) != 1 {
		t.Fatalf("the restored device is not under New devices: %+v", got)
	}
	_, body := w.lookup(t, "porch")
	hasAll(t, body, "New devices")
	if strings.Contains(body, "is ignored") {
		t.Error("the answer still says ignored after Restore")
	}
}

// Why (#713): a topic that is on the broker, but whose MAC is unknown, cannot be added. The answer must
// give the reason discovery keeps for it, so the admin knows what to wait for or to fix.
func TestLookupOfATopicSeenOnlyOnMQTTGivesItsReason(t *testing.T) {
	w := newWhyEnv(t)
	w.mgr.HandleMQTT(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "ghost", Payload: []byte("Offline"), Retained: true})
	waitFor(t, "the topic to be seen", func() bool { return len(w.mgr.Unidentified()) == 1 })
	status, body := w.lookup(t, "ghost")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	hasAll(t, body, `id="why"`, "ghost", "is on the broker, but TasmoFleet does not know its MAC yet", "cannot be asked for its MAC until it comes online")
}

// Why (#713): "nothing found" must say so and say how far the answer reaches, or an admin takes a
// device TasmoFleet has just not heard yet for a device that does not exist. A text that cannot be
// looked up gets a sentence, not an error page, and costs nothing.
func TestLookupOfSomethingNeverSeenSaysSoAndRefusesUnusableText(t *testing.T) {
	w := newWhyEnv(t)
	status, body := w.lookup(t, "10.0.0.99")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	hasAll(t, body, "has not seen", "since it started")
	for _, q := range []string{strings.Repeat("a", 129), "bad\ntext"} {
		status, body := w.lookup(t, q)
		if status != 200 || !strings.Contains(body, "Enter an IP address") {
			t.Errorf("%.20q: status %d, the sentence is missing", q, status)
		}
	}
}

// Why (#713): the box of the lookup shows up only for a lookup. An empty field is no question, and the
// normal page must not run the lookup (it reads the store three more times).
func TestAddPageWithoutALookupShowsNoAnswerAndAsksNothing(t *testing.T) {
	fd := &fakeDiscovery{}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	for _, path := range []string{"/devices/new", "/devices/new?why=", "/devices/new?why=%20%20"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		if len(fd.explained) != 0 || strings.Contains(body, `id="why"`) {
			t.Fatalf("%s: lookups = %v, answer shown = %v", path, fd.explained, strings.Contains(body, `id="why"`))
		}
		if !strings.Contains(body, "Look up by IP address, MAC or topic") {
			t.Errorf("%s: the lookup form is missing", path)
		}
	}
}

// Why (#713): a failing store must not turn the Add page into a 500 or leak the lookup text into the
// log; the admin gets a sentence in the box.
func TestLookupErrorGivesASentenceNotAnErrorPage(t *testing.T) {
	fd := &fakeDiscovery{explainErr: errors.New("disk on fire")}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	res, body := e.do(t, "GET", "/devices/new?why=lamp", "admin", nil)
	if res.StatusCode != 200 || !strings.Contains(body, "could not be looked up") || strings.Contains(body, "disk on fire") {
		t.Fatalf("status %d, sentence shown: %v, error text shown: %v", res.StatusCode, strings.Contains(body, "could not be looked up"), strings.Contains(body, "disk on fire"))
	}
}

// Why (#713): an admin who sees "Show ignored (1)" must learn there when an ignored device comes back,
// and the number of days is the prune's (discovery.ProposalTTL), not a copy of it.
func TestShowIgnoredSaysWhenAnIgnoredDeviceComesBack(t *testing.T) {
	now := time.Now()
	fd := &fakeDiscovery{ignored: []discovery.Proposal{{ID: 5, Version: "v5", Kind: discovery.KindNew, Via: "mqtt", SeenAt: now, Device: model.Device{Name: "old", Address: "10.0.0.51", MAC: "aa:bb:cc:dd:ee:05"}}}}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	hasAll(t, body, "stays hidden while", "30 days without a sighting")
	if want := int(discovery.ProposalTTL / (24 * time.Hour)); want != 30 {
		t.Fatalf("the prune keeps proposals %d days: update the acceptance text of #713", want)
	}
	fd.ignored = nil
	_, body = e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "stays hidden while") {
		t.Error("the help line shows with nothing ignored")
	}
}

// Why (#713): the lookup shows addresses and topics of the fleet; like the rest of the Add page it is
// for admins only. The admin gets the answer from the same URL, so the refusal is the route's, not a
// missing feature.
func TestLookupIsAdminOnly(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Discovery = &fakeDiscovery{} })
	if res, body := e.do(t, "GET", "/devices/new?why=lamp", "admin", nil); res.StatusCode != 200 || !strings.Contains(body, `id="why"`) {
		t.Fatalf("an admin got status %d without the answer", res.StatusCode)
	}
	res, body := e.do(t, "GET", "/devices/new?why=lamp", "operator", nil)
	if res.StatusCode == 200 || strings.Contains(body, `id="why"`) {
		t.Fatalf("an operator got status %d", res.StatusCode)
	}
}
