package inventory

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func countAudit(al *auditLog, action string) int {
	n := 0
	for _, e := range al.events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// Why: identity rule (spec 11.1). A stored hostname is the admin's choice and
// survives DHCP changes; a discovery or import that only knows the IP must
// never overwrite it.
func TestUpsertNeverReplacesHostnameWithIP(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, _, _, v := newSvcV(t)
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "plug.example.internal", MAC: mac})
	v.macs["172.31.13.9:80"] = mac // even a proven IP must not replace the name
	if _, _, err := m.Upsert(ctx, "discovery", model.Device{Name: "a", Address: "172.31.13.9", MAC: mac}); err != nil {
		t.Fatal(err)
	}
	if got := st.devs[id].Address; got != "plug.example.internal" {
		t.Fatalf("address = %q, the hostname was replaced", got)
	}
}

// Why: a re-scan or re-import that changes nothing must leave no trace; 255
// refused/no-op audit rows from one scheduled scan buried real events.
func TestUpsertWithoutChangesWritesNoAuditRow(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	m, _, al, _, _ := newSvcV(t)
	m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, Topic: "ta"}) //nolint:errcheck // setup
	before := len(al.events)
	if _, _, err := m.Upsert(ctx, "import", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, Topic: "ta"}); err != nil {
		t.Fatal(err)
	}
	if len(al.events) != before {
		t.Fatalf("no-op upsert wrote audit rows: %+v", al.events[before:])
	}
}

// Why: a device with a web user but no password has no authentication
// (Tasmota's user is always "admin"). It must not be treated as credentialed,
// or DHCP drift of all 27 imported devices would be refused.
func TestUserWithoutPasswordIsNotCredentials(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	m, st, _, _, v := newSvcV(t)
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, HTTPUser: "admin"})
	if got := st.devs[id]; got.HTTPUser != "" || got.HasCredentials() {
		t.Fatalf("a user without password was kept: %+v", got)
	}
	v.macs["172.31.13.9:80"] = mac
	if _, _, err := m.Upsert(ctx, "discovery", model.Device{Name: "a", Address: "172.31.13.9", MAC: mac, HTTPUser: "admin"}); err != nil {
		t.Fatalf("move of a device without password: %v", err)
	}
	if st.devs[id].Address != "172.31.13.9" {
		t.Fatalf("not moved: %+v", st.devs[id])
	}
}

// Why: ESP32 boards with Ethernet report two MACs. Either one is the same
// device; treating the second as a mismatch would block the device or create
// a duplicate.
func TestUpsertMatchesAnyMACOfTheSet(t *testing.T) {
	ctx := context.Background()
	const wifi, eth = "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"
	m, st, _, _, _ := newSvcV(t)
	id, err := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: wifi, MACs: []string{wifi, "AA-BB-CC-DD-EE-02"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.devs[id].MACs; len(got) != 2 {
		t.Fatalf("MACs = %v", got)
	}
	gotID, created, err := m.Upsert(ctx, "mqtt", model.Device{Name: "a", Address: "172.31.13.5", MAC: eth})
	if err != nil || created || gotID != id {
		t.Fatalf("id %d created %v err %v", gotID, created, err)
	}
	if st.devs[id].MAC != wifi {
		t.Fatalf("an automatic path changed the primary MAC: %q", st.devs[id].MAC)
	}
}

// Why: refused attempts are worth one audit row, not one per scan. At most
// one per device per day keeps the log readable and still shows that the
// device keeps announcing a new address.
func TestRefusedUpsertAuditIsDeduplicatedPerDevicePerDay(t *testing.T) {
	ctx := context.Background()
	const mac = "aa:bb:cc:dd:ee:01"
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	clk := &mutClock{t: now}
	st, al := newMemStore(), &auditLog{}
	m, _ := New(Options{Store: st, Audit: al, Clock: clk, Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}})
	id, _ := m.Create(ctx, "t", model.Device{Name: "a", Address: "172.31.13.5", MAC: mac, HTTPPassword: "pw"})
	other, _ := m.Create(ctx, "t", model.Device{Name: "b", Address: "172.31.13.6", MAC: "aa:bb:cc:dd:ee:09", HTTPPassword: "pw"})
	try := func(dev model.DeviceID, mac, addr string) {
		t.Helper()
		if _, _, err := m.Upsert(ctx, "discovery", model.Device{Name: "x", Address: addr, MAC: mac}); !errors.Is(err, ErrNeedsVerification) {
			t.Fatalf("device %d: %v", dev, err)
		}
	}
	for i := 0; i < 5; i++ {
		try(id, mac, "172.31.13.9")
	}
	if n := countAudit(al, "device.upsert"); n != 1 {
		t.Fatalf("%d refused rows for one device on one day, want 1", n)
	}
	try(other, "aa:bb:cc:dd:ee:09", "172.31.13.10")
	if n := countAudit(al, "device.upsert"); n != 2 {
		t.Fatalf("another device must get its own row, rows = %d", n)
	}
	clk.t = now.Add(24 * time.Hour)
	try(id, mac, "172.31.13.9")
	if n := countAudit(al, "device.upsert"); n != 3 {
		t.Fatalf("a new day must audit again, rows = %d", n)
	}
}

type mutClock struct{ t time.Time }

func (c *mutClock) Now() time.Time                       { return c.t }
func (c *mutClock) After(time.Duration) <-chan time.Time { return nil }
