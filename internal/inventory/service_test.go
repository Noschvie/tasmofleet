package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// memStore mirrors the semantics of store.SQLite that the service relies on:
// unique MAC (if set), unique topic (if set), unique (address, port);
// UpdateDevice leaves the identity alone, CreateDevice stores it.
type memStore struct {
	mu     sync.Mutex
	next   model.DeviceID
	devs   map[model.DeviceID]model.Device
	idents int // UpdateIdentity calls
}

func newMemStore() *memStore { return &memStore{devs: map[model.DeviceID]model.Device{}} }

func (s *memStore) conflict(d model.Device) bool {
	for id, o := range s.devs {
		if id == d.ID {
			continue
		}
		if (d.MAC != "" && o.MAC == d.MAC) || (d.Topic != "" && o.Topic == d.Topic) ||
			(o.Address == d.Address && o.Port == d.Port) {
			return true
		}
	}
	return false
}
func (s *memStore) ListDevices(context.Context) ([]model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Device
	for i := model.DeviceID(1); i <= s.next; i++ {
		if d, ok := s.devs[i]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}
func (s *memStore) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devs[id]
	if !ok {
		return d, store.ErrNotFound
	}
	return d, nil
}
func (s *memStore) find(f func(model.Device) bool) (model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if f(d) {
			return d, nil
		}
	}
	return model.Device{}, store.ErrNotFound
}
func (s *memStore) GetDeviceByMAC(_ context.Context, mac string) (model.Device, error) {
	return s.find(func(d model.Device) bool { return d.HasMAC(mac) })
}
func (s *memStore) GetDeviceByTopic(_ context.Context, t string) (model.Device, error) {
	return s.find(func(d model.Device) bool { return d.Topic == t })
}
func (s *memStore) CreateDevice(_ context.Context, d model.Device) (model.DeviceID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflict(d) {
		return 0, store.ErrConflict
	}
	s.next++
	d.ID = s.next
	if d.MAC != "" && !d.HasMAC(d.MAC) || d.MAC != "" && len(d.MACs) == 0 {
		d.MACs = append([]string{d.MAC}, d.MACs...) // the real store keeps the primary MAC in the set
	}
	s.devs[d.ID] = d
	return d.ID, nil
}
func (s *memStore) UpdateDevice(_ context.Context, d model.Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.devs[d.ID]
	if !ok {
		return store.ErrNotFound
	}
	if s.conflict(d) {
		return store.ErrConflict
	}
	d.Identity = o.Identity
	s.devs[d.ID] = d
	return nil
}
func (s *memStore) UpdateIdentity(_ context.Context, id model.DeviceID, i model.Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devs[id]
	if !ok {
		return store.ErrNotFound
	}
	d.Identity = i
	s.devs[id] = d
	s.idents++
	return nil
}
func (s *memStore) DeleteDevice(_ context.Context, id model.DeviceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devs[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.devs, id)
	return nil
}
func (s *memStore) SetLastSeen(context.Context, model.DeviceID, model.HealthState, time.Time) error {
	return nil
}

type auditLog struct{ events []model.AuditEvent }

func (a *auditLog) Record(_ context.Context, e model.AuditEvent) { a.events = append(a.events, e) }
func (a *auditLog) last(t *testing.T) model.AuditEvent {
	t.Helper()
	if len(a.events) == 0 {
		t.Fatal("no audit event recorded")
	}
	return a.events[len(a.events)-1]
}

type hub struct{ got []events.Event }

func (h *hub) Publish(e events.Event) { h.got = append(h.got, e) }

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time                       { return c.t }
func (c fixedClock) After(time.Duration) <-chan time.Time { return nil }

// fakeVerifier answers VerifyMAC from a table keyed by "address:port"; a
// missing entry behaves like a device that demands authentication.
type fakeVerifier struct {
	macs  map[string]string
	calls []string
	// during runs inside VerifyMAC, to simulate writes racing the probe.
	during func()
}

func (v *fakeVerifier) VerifyMAC(_ context.Context, a string, p int) (string, error) {
	k := fmt.Sprintf("%s:%d", a, p)
	v.calls = append(v.calls, k)
	if v.during != nil {
		v.during()
	}
	if m, ok := v.macs[k]; ok {
		return m, nil
	}
	return "", errors.New("authentication needed")
}

func newSvc(t *testing.T) (*Manager, *memStore, *auditLog, *hub) {
	t.Helper()
	m, st, al, h, _ := newSvcV(t)
	return m, st, al, h
}

func newSvcV(t *testing.T) (*Manager, *memStore, *auditLog, *hub, *fakeVerifier) {
	t.Helper()
	v := &fakeVerifier{macs: map[string]string{}}
	st, al, h := newMemStore(), &auditLog{}, &hub{}
	m, err := New(Options{
		Store: st, Audit: al, Events: h,
		Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24"), netip.MustParsePrefix("172.31.23.0/24")},
		Clock:    fixedClock{time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
		Verifier: v,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, al, h, v
}

func dev(name, addr string) model.Device { return model.Device{Name: name, Address: addr} }

// SSRF rule: nothing outside DEVICE_NETWORKS ever becomes a target, and
// special ranges are refused even if an operator configured a broad network.
func TestCreateRejectsAddressesOutsideNetworks(t *testing.T) {
	m, _, _, _ := newSvc(t)
	bad := []string{"8.8.8.8", "172.31.14.1", "127.0.0.1", "169.254.169.254", "224.0.0.1", "0.0.0.0", "::1",
		"http://172.31.13.5", "172.31.13.5:80", "172.31.13.5/x", "", "bad host", "-x.example"}
	for _, a := range bad {
		if _, err := m.Create(context.Background(), "t", dev("x", a)); !errors.Is(err, ErrInvalid) {
			t.Errorf("address %q: want ErrInvalid, got %v", a, err)
		}
	}
	if _, err := m.Create(context.Background(), "t", dev("x", "8.8.8.8")); !errors.Is(err, ErrOutsideNetwork) {
		t.Errorf("outside network must be matchable: %v", err)
	}
	for _, a := range []string{"172.31.13.5", "plug.example.internal", "OTHER.Example.Internal."} {
		if _, err := m.Create(context.Background(), "t", dev("x"+a, a)); err != nil {
			t.Errorf("address %q should be fine: %v", a, err)
		}
	}
}

func TestBroadNetworkStillRefusesLoopbackAndLinkLocal(t *testing.T) {
	m, err := New(Options{Store: newMemStore(), Audit: &auditLog{}, Networks: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"127.0.0.1", "169.254.1.1", "224.1.1.1"} {
		if _, err := m.Create(context.Background(), "t", dev("x", a)); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
}

// The simulator runs on loopback: an explicit narrow entry such as
// 127.0.0.0/8 unlocks it (same rule as the dialer), while the metadata
// address stays refused even when its range is listed.
func TestExplicitNarrowNetworkUnlocksLoopback(t *testing.T) {
	m, err := New(Options{Store: newMemStore(), Audit: &auditLog{}, Networks: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "t", dev("sim", "127.0.0.1")); err != nil {
		t.Errorf("explicit 127.0.0.0/8 must allow 127.0.0.1: %v", err)
	}
	if _, err := m.Create(context.Background(), "t", dev("meta", "169.254.169.254")); !errors.Is(err, ErrOutsideNetwork) {
		t.Errorf("metadata address accepted: %v", err)
	}
}

func TestNoNetworksMeansNoIPAddresses(t *testing.T) {
	m, _ := New(Options{Store: newMemStore(), Audit: &auditLog{}})
	if _, err := m.Create(context.Background(), "t", dev("x", "172.31.13.5")); !errors.Is(err, ErrOutsideNetwork) {
		t.Fatalf("got %v", err)
	}
}

// Identity is the MAC: every spelling must collapse to one stored form or the
// unique index and the MQTT discovery matching would see two devices.
func TestNormaliseMAC(t *testing.T) {
	for in, want := range map[string]string{
		"AA:BB:CC:DD:EE:FF":   "aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff":   "aa:bb:cc:dd:ee:ff",
		"AABBCCDDEEFF":        "aa:bb:cc:dd:ee:ff",
		"aabb.ccdd.eeff":      "aa:bb:cc:dd:ee:ff",
		" 00:11:22:33:44:55 ": "00:11:22:33:44:55",
		"":                    "",
	} {
		got, err := NormaliseMAC(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, in := range []string{"aa:bb:cc", "zz:bb:cc:dd:ee:ff", "aa:bb:cc:dd:ee:ff:00"} {
		if _, err := NormaliseMAC(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	m, _, _, _ := newSvc(t)
	cases := map[string]model.Device{
		"empty name":    {Address: "172.31.13.5"},
		"control chars": {Name: "a\x00b", Address: "172.31.13.5"},
		"bad port":      {Name: "x", Address: "172.31.13.5", Port: 70000},
		"bad topic":     {Name: "x", Address: "172.31.13.5", Topic: "a/b"},
		"topic space":   {Name: "x", Address: "172.31.13.5", Topic: "a b"},
		"bad mac":       {Name: "x", Address: "172.31.13.5", MAC: "nope"},
		"long tag":      {Name: "x", Address: "172.31.13.5", Tags: []string{strings.Repeat("a", 51)}},
	}
	for name, d := range cases {
		if _, err := m.Create(context.Background(), "t", d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestCreateNormalisesAndAudits(t *testing.T) {
	m, st, al, h := newSvc(t)
	id, err := m.Create(context.Background(), "alice", model.Device{
		Name: " Plug ", Address: "172.31.13.5", MAC: "AA-BB-CC-DD-EE-FF", Tags: []string{"b", " a ", "b", ""},
		HTTPUser: "admin", HTTPPassword: "s3cret-pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := st.devs[id]
	if got.Name != "Plug" || got.MAC != "aa:bb:cc:dd:ee:ff" || got.Port != 80 || strings.Join(got.Tags, ",") != "b,a" {
		t.Errorf("not normalised: %+v", got)
	}
	if !got.CreatedAt.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt %v", got.CreatedAt)
	}
	e := al.last(t)
	if e.Action != "device.create" || e.Actor != "alice" || e.Target != "device:1" || e.Result != "ok" {
		t.Errorf("audit: %+v", e)
	}
	// Safety rule 4: credentials never reach audit details or browser events.
	if strings.Contains(e.Detail, "s3cret-pw") {
		t.Errorf("audit detail leaks password: %s", e.Detail)
	}
	p := h.got[len(h.got)-1].Payload.(model.Device)
	if p.HTTPPassword != "" || p.ID != id {
		t.Errorf("event payload must carry the device without its password: %+v", p)
	}
}

func TestCreateDuplicateReportsConflictAndAuditsFailure(t *testing.T) {
	m, _, al, h := newSvc(t)
	if _, err := m.Create(context.Background(), "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:ff"}); err != nil {
		t.Fatal(err)
	}
	n := len(h.got)
	_, err := m.Create(context.Background(), "t", model.Device{Name: "b", Address: "172.31.13.6", MAC: "AA:BB:CC:DD:EE:FF"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("got %v", err)
	}
	if al.last(t).Result == "ok" {
		t.Error("failed create must be audited as failed")
	}
	if len(h.got) != n {
		t.Error("failed create must not publish")
	}
}

func TestUpdateAuditsChangedFieldsWithoutValues(t *testing.T) {
	m, st, al, _ := newSvc(t)
	id, _ := m.Create(context.Background(), "t", model.Device{Name: "a", Address: "172.31.13.5", HTTPPassword: "old-pw"})
	d := st.devs[id]
	d.Address, d.HTTPPassword = "172.31.13.9", "new-pw"
	if err := m.Update(context.Background(), "bob", d); err != nil {
		t.Fatal(err)
	}
	e := al.last(t)
	var det map[string]any
	if err := json.Unmarshal([]byte(e.Detail), &det); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Detail, "pw") {
		t.Errorf("leak: %s", e.Detail)
	}
	if e.Action != "device.update" || !strings.Contains(e.Detail, `"credentials"`) || det["address_to"] != "172.31.13.9" {
		t.Errorf("audit: %+v", e)
	}
	if err := m.Update(context.Background(), "bob", model.Device{ID: 99, Name: "x", Address: "172.31.13.9"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
}

func TestDeleteAudits(t *testing.T) {
	m, st, al, _ := newSvc(t)
	id, _ := m.Create(context.Background(), "t", dev("gone", "172.31.13.5"))
	if err := m.Delete(context.Background(), "bob", id); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.devs[id]; ok {
		t.Error("still stored")
	}
	if e := al.last(t); e.Action != "device.delete" || !strings.Contains(e.Detail, "gone") {
		t.Errorf("audit: %+v", e)
	}
	if err := m.Delete(context.Background(), "bob", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestUpsertMatchOrder(t *testing.T) {
	ctx := context.Background()
	m, st, _, _, v := newSvcV(t)
	v.macs["172.31.13.50:80"] = "aa:bb:cc:dd:ee:01"
	a, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:01", Topic: "ta"})
	b, _ := m.Create(ctx, "t", model.Device{Name: "b", Address: "172.31.13.6", Topic: "tb"})
	c, _ := m.Create(ctx, "t", model.Device{Name: "c", Address: "172.31.13.7"})

	// MAC wins over everything, even when topic/address point elsewhere.
	id, created, err := m.Upsert(ctx, "t", model.Device{Name: "z", Address: "172.31.13.50", MAC: "AA:BB:CC:DD:EE:01"})
	if err != nil || created || id != a {
		t.Errorf("by MAC: id=%d created=%v err=%v", id, created, err)
	}
	if st.devs[a].Address != "172.31.13.50" {
		t.Errorf("DHCP drift not applied: %+v", st.devs[a])
	}
	// Topic next.
	id, created, err = m.Upsert(ctx, "t", model.Device{Name: "z", Address: "172.31.13.6", Topic: "tb"})
	if err != nil || created || id != b {
		t.Errorf("by topic: id=%d created=%v err=%v", id, created, err)
	}
	// Address last (port 0 == 80).
	id, created, err = m.Upsert(ctx, "t", model.Device{Name: "z", Address: "172.31.13.7", Port: 80, Topic: "tc"})
	if err != nil || created || id != c {
		t.Errorf("by address: id=%d created=%v err=%v", id, created, err)
	}
	// Nothing matches -> new device.
	id, created, err = m.Upsert(ctx, "t", dev("new", "172.31.13.99"))
	if err != nil || !created || id == 0 {
		t.Errorf("create: id=%d created=%v err=%v", id, created, err)
	}
	// Same address on another port is another device (unique key is the pair).
	_, created, err = m.Upsert(ctx, "t", model.Device{Name: "other-port", Address: "172.31.13.99", Port: 8080})
	if err != nil || !created {
		t.Errorf("port: created=%v err=%v", created, err)
	}
}

func TestUpsertMergeKeepsOperatorChoices(t *testing.T) {
	ctx := context.Background()
	m, st, al, _ := newSvc(t)
	id, _ := m.Create(ctx, "t", model.Device{
		Name: "Renamed by operator", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:01", Notes: "my note",
		Tags: []string{"kitchen"}, HTTPUser: "admin", HTTPPassword: "pw1",
	})
	_, _, err := m.Upsert(ctx, "import", model.Device{
		Name: "Imported Name", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:01", Notes: "imported",
		Tags: []string{"imported"}, OTAExcluded: true, Topic: "plug1",
		Identity: model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := st.devs[id]
	if got.Name != "Renamed by operator" || got.Notes != "my note" {
		t.Errorf("re-import must not undo renames/notes: %+v", got)
	}
	if got.HTTPUser != "admin" || got.HTTPPassword != "pw1" {
		t.Errorf("credentials must survive an import row without credentials: %+v", got)
	}
	if strings.Join(got.Tags, ",") != "kitchen,imported" || !got.OTAExcluded || got.Topic != "plug1" {
		t.Errorf("merge: %+v", got)
	}
	if got.Identity.Version != "16.0.0" || st.idents != 1 {
		t.Errorf("identity: %+v (writes %d)", got.Identity, st.idents)
	}
	if e := al.last(t); e.Action != "device.update" {
		t.Errorf("action %s", e.Action)
	}
}

func TestUpsertAuditsAddressChange(t *testing.T) {
	ctx := context.Background()
	m, _, al, _, v := newSvcV(t)
	v.macs["172.31.13.8:80"] = "aa:bb:cc:dd:ee:01"
	m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:01"}) //nolint:errcheck // test setup, asserted below via Upsert
	if _, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.8", MAC: "aa:bb:cc:dd:ee:01"}); err != nil {
		t.Fatal(err)
	}
	e := al.last(t)
	if e.Action != "device.address_changed" || !strings.Contains(e.Detail, `"address_from":"172.31.13.5"`) || !strings.Contains(e.Detail, `"address_to":"172.31.13.8"`) {
		t.Errorf("audit: %+v", e)
	}
}

// A new IP lease can land on an address that used to belong to other hardware.
// Merging would make later OTA jobs talk to the wrong device.
func TestUpsertRefusesDifferentMACOnAddressOrTopicMatch(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := newSvc(t)
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:01", Topic: "ta"})
	for _, in := range []model.Device{
		{Name: "x", Address: "172.31.13.5", MAC: "aa:bb:cc:dd:ee:02"},
		{Name: "x", Address: "172.31.13.6", MAC: "aa:bb:cc:dd:ee:02", Topic: "ta"},
	} {
		if _, _, err := m.Upsert(ctx, "t", in); !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%+v: got %v", in, err)
		}
	}
	if st.devs[id].MAC != "aa:bb:cc:dd:ee:01" {
		t.Error("existing record was modified")
	}
}

// Importing the same TasmoAdmin export twice must be idempotent.
func TestUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m, st, _, _ := newSvc(t)
	in := model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", HTTPUser: "admin", HTTPPassword: "pw"}
	id1, c1, _ := m.Upsert(ctx, "t", in)
	id2, c2, err := m.Upsert(ctx, "t", in)
	if err != nil || id1 != id2 || !c1 || c2 || len(st.devs) != 1 {
		t.Errorf("ids %d %d created %v %v err %v n=%d", id1, id2, c1, c2, err, len(st.devs))
	}
}

func TestUpsertInvalidIsRejectedAndAudited(t *testing.T) {
	m, _, al, _ := newSvc(t)
	if _, _, err := m.Upsert(context.Background(), "t", dev("x", "8.8.8.8")); !errors.Is(err, ErrOutsideNetwork) {
		t.Fatalf("got %v", err)
	}
	if al.last(t).Result == "ok" {
		t.Error("rejected import row must be audited as failed")
	}
}

func TestNewRequiresStoreAndAudit(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("want error")
	}
}

// Attack paths for credential redirection. Whatever arrives through MQTT
// discovery or a probe is untrusted: it must never move a record (and with it
// the stored HTTP password) to a host whose identity is unproven.
func TestUpsertNeverRedirectsCredentials(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	setup := func(t *testing.T, withMAC bool) (*Manager, *memStore, *fakeVerifier, model.DeviceID) {
		m, st, _, _, v := newSvcV(t)
		d := model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", HTTPUser: "admin", HTTPPassword: "pw-secret"}
		if withMAC {
			d.MAC = mac
		}
		id, err := m.Create(ctx, "t", d)
		if err != nil {
			t.Fatal(err)
		}
		return m, st, v, id
	}
	unchanged := func(t *testing.T, st *memStore, id model.DeviceID) {
		t.Helper()
		if d := st.devs[id]; d.Address != "172.31.13.5" || d.HTTPPassword != "pw-secret" {
			t.Errorf("record was modified: %+v", d)
		}
	}

	t.Run("rogue claims known topic with other MAC", func(t *testing.T) {
		m, st, _, id := setup(t, true)
		_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "evil", Address: "172.31.13.66", Topic: "ta", MAC: "de:ad:be:ef:00:01"})
		if !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("got %v", err)
		}
		unchanged(t, st, id)
	})
	t.Run("rogue claims known topic without MAC", func(t *testing.T) {
		m, st, v, id := setup(t, true)
		_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "evil", Address: "172.31.13.66", Topic: "ta"})
		if !errors.Is(err, ErrNeedsVerification) {
			t.Fatalf("got %v", err)
		}
		unchanged(t, st, id)
		_ = v
	})
	t.Run("DHCP reuse: other hardware at the old address", func(t *testing.T) {
		m, st, _, id := setup(t, true)
		_, _, err := m.Upsert(ctx, "probe", model.Device{Name: "x", Address: "172.31.13.5", MAC: "de:ad:be:ef:00:02"})
		if !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("got %v", err)
		}
		unchanged(t, st, id)
	})
	// The MAC in Status 5 is self-asserted. An impostor that answers with
	// the known MAC must not receive the record of a credentialed device:
	// a genuine password-protected Tasmota cannot even answer the
	// unauthenticated probe, so "verified" would prove only the impostor.
	t.Run("impostor reports the known MAC at a new IP", func(t *testing.T) {
		m, st, v, id := setup(t, true)
		v.macs["172.31.13.66:80"] = mac
		_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.66", MAC: mac})
		if !errors.Is(err, ErrNeedsVerification) {
			t.Fatalf("got %v", err)
		}
		if len(v.calls) != 0 {
			t.Errorf("a credentialed record must not even trigger a probe: %v", v.calls)
		}
		unchanged(t, st, id)
	})
	t.Run("admin Update may move a credentialed device after the explicit confirmation (row 15)", func(t *testing.T) {
		m, st, _, id := setup(t, true)
		d := st.devs[id]
		d.Address = "172.31.13.9"
		if err := m.Update(ctx, "admin", d); !errors.Is(err, ErrMoveUnverified) {
			t.Fatalf("an unproven move needs the confirmation: %v", err)
		}
		if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.9", 0)), "admin", d); err != nil {
			t.Fatalf("an admin edit is the explicit choice: %v", err)
		}
		if got := st.devs[id]; got.Address != "172.31.13.9" || got.HTTPPassword != "pw-secret" {
			t.Errorf("%+v", got)
		}
	})
	t.Run("no verifier configured", func(t *testing.T) {
		st := newMemStore()
		m, _ := New(Options{Store: st, Audit: &auditLog{}, Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}})
		id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, HTTPPassword: "pw-secret"})
		_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.9", MAC: mac})
		if !errors.Is(err, ErrNeedsVerification) {
			t.Fatalf("got %v", err)
		}
		unchanged(t, st, id)
	})
	t.Run("record without MAC is never moved, even if the probe agrees", func(t *testing.T) {
		m, st, v, id := setup(t, false)
		v.macs["172.31.13.66:80"] = mac
		_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.66", Topic: "ta", MAC: mac})
		if !errors.Is(err, ErrNeedsVerification) {
			t.Fatalf("got %v", err)
		}
		unchanged(t, st, id)
	})
	t.Run("refusals are audited as failures", func(t *testing.T) {
		m, _, _, _ := newSvc(t)
		al := m.audit.(*auditLog)
		m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac})     //nolint:errcheck // setup
		m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.66", MAC: mac}) //nolint:errcheck // asserted via audit
		if e := al.last(t); e.Result == "ok" || e.Action == "device.address_changed" {
			t.Errorf("audit: %+v", e)
		}
	})
}

// openDevice creates a credential-less device bound to mac at 172.31.13.5.
func openDevice(t *testing.T, mac string) (*Manager, *memStore, *fakeVerifier, model.DeviceID) {
	t.Helper()
	m, st, _, _, v := newSvcV(t)
	id, err := m.Create(context.Background(), "t", model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, v, id
}

// DHCP drift of devices without a password is still followed automatically
// (spec 11.3): with nothing secret in the record, a lying host gains nothing.
func TestUpsertMovesCredentialLessDeviceAfterProbe(t *testing.T) {
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, v, id := openDevice(t, mac)
	v.macs["172.31.13.9:80"] = "AA-BB-CC-DD-EE-01"
	if _, _, err := m.Upsert(context.Background(), "mqtt", model.Device{Name: "a", Address: "172.31.13.9", MAC: mac}); err != nil {
		t.Fatal(err)
	}
	if d := st.devs[id]; d.Address != "172.31.13.9" {
		t.Errorf("%+v", d)
	}
	v.macs["172.31.13.10:80"] = "de:ad:be:ef:00:03"
	if _, _, err := m.Upsert(context.Background(), "mqtt", model.Device{Name: "a", Address: "172.31.13.10", MAC: mac}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("other MAC at the new address: got %v", err)
	}
}

// An incoming device that carries credentials (an import row) must not be
// able to attach them to a record and move it in one step: the credentials
// would then go to whatever host claimed the MAC.
func TestUpsertIncomingCredentialsMakeMoveAdminOnly(t *testing.T) {
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, v, id := openDevice(t, mac)
	v.macs["172.31.13.66:80"] = mac
	_, _, err := m.Upsert(context.Background(), "import", model.Device{Name: "a", Address: "172.31.13.66", MAC: mac, HTTPUser: "admin", HTTPPassword: "pw"})
	if !errors.Is(err, ErrNeedsVerification) {
		t.Fatalf("got %v", err)
	}
	if d := st.devs[id]; d.Address != "172.31.13.5" || d.HTTPPassword != "" {
		t.Errorf("record was modified: %+v", d)
	}
}

// DNS rebinding: a name verified against one IP can resolve to another at
// the next connect. Automatic moves therefore only accept IP literals, which
// pins the verified address; DNS names are an admin choice through Update.
func TestUpsertRefusesMoveToDNSName(t *testing.T) {
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, v, id := openDevice(t, mac)
	v.macs["plug.example:80"] = mac
	_, _, err := m.Upsert(context.Background(), "mqtt", model.Device{Name: "a", Address: "plug.example", MAC: mac})
	if !errors.Is(err, ErrNeedsVerification) {
		t.Fatalf("got %v", err)
	}
	if len(v.calls) != 0 {
		t.Errorf("DNS name must not be probed: %v", v.calls)
	}
	if st.devs[id].Address != "172.31.13.5" {
		t.Errorf("moved: %+v", st.devs[id])
	}
}

// A record without MAC must not be bound to whatever MAC a message claims:
// the claimant could then pass every later MAC check (and lock out the real
// device). The binding is accepted only from the host the record points to.
func TestUpsertBindsMACOnlyWhenTheRecordedHostReportsIt(t *testing.T) {
	ctx := context.Background()
	const attacker, real = "de:ad:be:ef:00:01", "aa:bb:cc:dd:ee:01"
	m, st, _, _, v := newSvcV(t)
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", HTTPPassword: "pw-secret"})

	_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", MAC: attacker})
	if !errors.Is(err, ErrNeedsVerification) {
		t.Fatalf("unverifiable claim: got %v", err)
	}
	v.macs["172.31.13.5:80"] = real
	_, _, err = m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", MAC: attacker})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("claim contradicted by the recorded host: got %v", err)
	}
	if st.devs[id].MAC != "" {
		t.Fatalf("record bound to a claimed MAC: %+v", st.devs[id])
	}
	if _, _, err = m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.5", Topic: "ta", MAC: real}); err != nil {
		t.Fatal(err)
	}
	if d := st.devs[id]; d.MAC != real || d.HTTPPassword != "pw-secret" {
		t.Errorf("confirmed MAC not bound: %+v", d)
	}
}

// TOCTOU: the probe runs without the lock, so an admin may change the record
// meanwhile. Upsert must not write its stale merge over that change.
func TestUpsertDetectsChangeDuringVerification(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, v, id := openDevice(t, mac)
	v.macs["172.31.13.9:80"] = mac
	v.during = func() {
		d := st.devs[id]
		d.HTTPUser, d.HTTPPassword = "admin", "new-secret"
		if err := m.Update(ctx, "admin", d); err != nil {
			t.Error(err)
		}
	}
	_, _, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.9", MAC: mac})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
	if d := st.devs[id]; d.Address != "172.31.13.5" || d.HTTPPassword != "new-secret" {
		t.Errorf("stale merge was written: %+v", d)
	}
}

// Why (audit row 15, 2026-10-21; replaces security review F2, tightened by the
// second review): an answer to the credential-less probe is no proof for a
// device that has a web password (a genuine one would refuse it). Even with
// the expected MAC the admin must confirm the target explicitly, and the
// password stays with the record.
func TestUpdateOfCredentialedDeviceNeedsConfirmationEvenWithTheExpectedMAC(t *testing.T) {
	m, st, _, _, v := newSvcV(t)
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", Port: 80, MAC: mac, HTTPPassword: "stored-pw"})
	for _, k := range []string{"172.31.13.9:80", "172.31.13.9:8080"} {
		v.macs[k] = mac
	}
	d := st.devs[id]
	d.Address = "172.31.13.9"
	if err := m.Update(ctx, "bob", d); !errors.Is(err, ErrMoveUnverified) {
		t.Fatalf("a matching MAC is not enough: %v", err)
	}
	if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.9", 0)), "bob", d); err != nil {
		t.Fatal(err)
	}
	d = st.devs[id]
	d.Port = 8080
	if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.9", 80)), "bob", d); !errors.Is(err, ErrMoveUnverified) {
		t.Fatalf("a confirmation for 172.31.13.9:80 must not cover :8080: %v", err)
	}
	if got := st.devs[id]; got.HTTPPassword != "stored-pw" || got.Port == 8080 {
		t.Errorf("%+v", got)
	}
}

// Why (second security review, point 3): no path but the explicit admin
// actions moves a credentialed device. Scan, MQTT announce and import all go
// through Upsert and are refused even when the new address answers with the
// right MAC; UpdateIf (discovery's "Update address") has the same gate as the
// edit form, and a refused attempt writes nothing and probes nobody with the
// password (the verifier receives no credentials by construction).
func TestOnlyConfirmedAdminActionsMoveACredentialedDevice(t *testing.T) {
	m, st, _, _, v := newSvcV(t)
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, HTTPPassword: "stored-pw"})
	v.macs["172.31.13.66:80"] = mac
	for _, actor := range []string{"scan", "mqtt", "import"} {
		if _, _, err := m.Upsert(ctx, actor, model.Device{Name: "a", Address: "172.31.13.66", MAC: mac}); err == nil {
			t.Errorf("%s moved a credentialed device", actor)
		}
	}
	cur := st.devs[id]
	next := cur
	next.Address = "172.31.13.66"
	if err := m.UpdateIf(ctx, "discovery", cur, next); !errors.Is(err, ErrMoveUnverified) {
		t.Fatalf("UpdateIf without the confirmation: %v", err)
	}
	if got := st.devs[id]; got.Address != "172.31.13.5" || got.HTTPPassword != "stored-pw" {
		t.Fatalf("a refused attempt changed the record: %+v", got)
	}
	if err := m.UpdateIf(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.66", 0)), "admin", cur, next); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
}

// Why (security review, 2026-10-21): when the new address cannot prove the MAC
// (a password-protected device cannot answer the credential-less probe, an
// impostor can), the stored password is sent there only after the admin
// confirmed exactly that target, and the audit entry says "mac_unverified".
// Another MAC is never accepted, and a typed new password needs no proof (the
// stored one is not sent).
func TestUpdateOfCredentialedDeviceToUnprovenAddressNeedsConfirmation(t *testing.T) {
	m, st, al, _, v := newSvcV(t)
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, HTTPPassword: "stored-pw"})
	move := func(addr string) model.Device { d := st.devs[id]; d.Address = addr; return d }

	if err := m.Update(ctx, "bob", move("172.31.13.9")); !errors.Is(err, ErrMoveUnverified) {
		t.Fatalf("without the confirmation: %v", err)
	}
	if st.devs[id].Address != "172.31.13.5" {
		t.Fatal("the refused edit reached the store")
	}
	if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.10", 0)), "bob", move("172.31.13.9")); !errors.Is(err, ErrMoveUnverified) {
		t.Fatalf("a confirmation for another target must not count: %v", err)
	}
	v.macs["172.31.13.66:80"] = "de:ad:be:ef:00:01"
	if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.66", 0)), "bob", move("172.31.13.66")); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("another MAC is never accepted: %v", err)
	}
	if err := m.Update(WithUnverifiedMoveConfirmed(ctx, MoveTarget("172.31.13.9", 0)), "bob", move("172.31.13.9")); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if got := st.devs[id]; got.Address != "172.31.13.9" || got.HTTPPassword != "stored-pw" {
		t.Errorf("%+v", got)
	}
	if last := al.last(t); last.Action != "device.update" || !strings.Contains(last.Detail, `"mac_unverified":true`) {
		t.Errorf("the audit entry must say MAC unverified: %+v", last)
	}
	d := move("172.31.13.11")
	d.HTTPPassword = "new-typed-pw"
	if err := m.Update(ctx, "bob", d); err != nil {
		t.Fatalf("a new password needs no proof: %v", err)
	}
}

// Why: DEVICE_NETWORKS can change while the server runs. Addresses are judged
// against the networks in force now, in both directions.
func TestNetworkSetChangesApplyAtOnce(t *testing.T) {
	set := tasmota.NewNetworkSet(nil)
	st := newMemStore()
	m, err := New(Options{Store: st, Audit: &auditLog{}, NetworkSet: set})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5"}); !errors.Is(err, ErrOutsideNetwork) {
		t.Fatalf("empty set: %v", err)
	}
	set.Set([]netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")})
	if _, err := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5"}); err != nil {
		t.Fatalf("after adding the network: %v", err)
	}
	set.Set(nil)
	if _, err := m.Create(ctx, "t", model.Device{Name: "b", Address: "172.31.13.6"}); !errors.Is(err, ErrOutsideNetwork) {
		t.Fatalf("after removing it: %v", err)
	}
}

// Why: the importer's plan asks the inventory whether an address would be
// accepted, so a row that Upsert will refuse shows as skipped before Apply.
// The answer must be the same one Create gives, or the plan promises wrongly.
func TestCheckAddressAgreesWithCreate(t *testing.T) {
	m, _, _, _ := newSvc(t)
	for _, a := range []string{"8.8.8.8", "127.0.0.1", "", "bad host", "172.31.13.5", "plug.example.internal"} {
		_, cerr := m.Create(context.Background(), "t", dev("n"+a, a))
		if got := m.CheckAddress(a); (got == nil) != (cerr == nil) {
			t.Errorf("address %q: CheckAddress = %v, Create = %v", a, got, cerr)
		}
	}
	if err := m.CheckAddress("8.8.8.8"); !errors.Is(err, ErrOutsideNetwork) {
		t.Errorf("outside network must be matchable: %v", err)
	}
}

func (s *memStore) SetMQTTPassword(context.Context, model.DeviceID, model.MQTTPasswordMode, string) error {
	return nil
}
func (s *memStore) MarkMQTTPasswordBad(context.Context, model.DeviceID, time.Time) error { return nil }
func (s *memStore) SetFWReach(context.Context, model.DeviceID, model.FWReach) error      { return nil }
