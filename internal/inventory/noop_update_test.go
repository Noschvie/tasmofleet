package inventory_test

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// rowLog collects the audit rows of a real-store inventory.
type rowLog struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (l *rowLog) Record(_ context.Context, e model.AuditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *rowLog) count(action string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if e.Action == action {
			n++
		}
	}
	return n
}

func (l *rowLog) last(action string) (model.AuditEvent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i].Action == action {
			return l.events[i], true
		}
	}
	return model.AuditEvent{}, false
}

// realInventory is the inventory on the real SQLite store. The in-memory store of the package tests keeps an
// empty tag list as it was given, the real store loads it as nil; the no-op rule must hold for the real form,
// or a device without tags (the common case) is never "unchanged" (#988).
func realInventory(t *testing.T) (*inventory.Manager, *store.SQLite, *rowLog) {
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
	rec := &rowLog{}
	m, err := inventory.New(inventory.Options{Store: st, Audit: rec, Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, rec
}

// Why (#988): a save that changes nothing (the edit form with only the MQTT password mode changed) used to
// write the record again and audit an empty device.update {"changed":null}: noise in the audit log that reads
// like a change nobody made, and updated_at moving for nothing. Update and UpdateIf must write and audit
// only when a field differs, with and without tags (the store loads no tags as nil, validate makes them
// an empty list).
func TestUnchangedUpdateWritesNothingAndAuditsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
	}{{"no tags", nil}, {"with tags", []string{"kitchen"}}} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, rec := realInventory(t)
			ctx := context.Background()
			id, err := m.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.10", Tags: tc.tags})
			if err != nil {
				t.Fatal(err)
			}
			read, err := m.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			before := rec.count("device.update")

			if err := m.UpdateIf(ctx, "admin", read, read); err != nil {
				t.Fatal(err)
			}
			if err := m.Update(ctx, "admin", read); err != nil {
				t.Fatal(err)
			}
			if n := rec.count("device.update") - before; n != 0 {
				e, _ := rec.last("device.update")
				t.Fatalf("%d device.update rows for a save that changed nothing, last %s", n, e.Detail)
			}
			if got, _ := m.Get(ctx, id); !got.UpdatedAt.Equal(read.UpdatedAt) {
				t.Fatalf("updated_at moved from %s to %s without a change", read.UpdatedAt, got.UpdatedAt)
			}
		})
	}
}

// Why (#988): validate puts the primary MAC first, the store loads the MAC set sorted. A device whose primary
// MAC sorts after its second one (Wi-Fi plus Ethernet, after an admin MAC edit or a device swap) must still
// be "unchanged" when saved as read, or the empty device.update {"changed":null} is back for exactly those
// devices.
func TestUnchangedUpdateOfADeviceWithTwoMACsWritesNothing(t *testing.T) {
	m, _, rec := realInventory(t)
	ctx := context.Background()
	id, err := m.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.10", MAC: "aa:bb:cc:dd:ee:02", MACs: []string{"aa:bb:cc:dd:ee:01"}})
	if err != nil {
		t.Fatal(err)
	}
	read, err := m.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	before := rec.count("device.update")

	if err := m.UpdateIf(ctx, "admin", read, read); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, "admin", read); err != nil {
		t.Fatal(err)
	}
	if n := rec.count("device.update") - before; n != 0 {
		e, _ := rec.last("device.update")
		t.Fatalf("%d device.update rows for a save that changed nothing, last %s", n, e.Detail)
	}
	if got, _ := m.Get(ctx, id); !got.UpdatedAt.Equal(read.UpdatedAt) {
		t.Fatalf("updated_at moved from %s to %s without a change", read.UpdatedAt, got.UpdatedAt)
	}
}

// Why (#988): the no-op rule must not swallow a real edit. A changed field is still written and audited
// with its name.
func TestChangedUpdateStillWritesAndAudits(t *testing.T) {
	m, _, rec := realInventory(t)
	ctx := context.Background()
	id, _ := m.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.10"})
	read, _ := m.Get(ctx, id)
	next := read
	next.Notes = "moved to the porch"
	if err := m.UpdateIf(ctx, "admin", read, next); err != nil {
		t.Fatal(err)
	}
	e, ok := rec.last("device.update")
	if !ok || e.Detail != `{"changed":["notes"]}` {
		t.Fatalf("audit row %+v", e)
	}
	if got, _ := m.Get(ctx, id); got.Notes != "moved to the porch" {
		t.Fatalf("notes %q", got.Notes)
	}
}

// Why (#988): the no-op check comes after the compare-and-set. A caller whose copy is stale must still be told
// (ErrConflict) and the refusal must still be audited, even when the record it wants to write equals the stored one.
func TestStaleUpdateIfStillConflictsWhenNothingWouldChange(t *testing.T) {
	m, _, rec := realInventory(t)
	ctx := context.Background()
	id, _ := m.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.10"})
	stale, _ := m.Get(ctx, id)
	edited := stale
	edited.Notes = "someone else was faster"
	if err := m.Update(ctx, "admin", edited); err != nil {
		t.Fatal(err)
	}
	current, _ := m.Get(ctx, id)
	before := rec.count("device.update")
	if err := m.UpdateIf(ctx, "admin", stale, current); !errors.Is(err, inventory.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if n := rec.count("device.update") - before; n != 1 {
		t.Fatalf("%d audit rows for the refused save, want 1", n)
	}
}

// Why (#988): an unchanged device that is announced again (discovery, import) is the same no-op as an
// unchanged save. Without tags the comparison used to differ (empty list against nil) and every sighting wrote
// an empty device.update row.
func TestUpsertOfAnUnchangedDeviceWithoutTagsAuditsNothing(t *testing.T) {
	m, _, rec := realInventory(t)
	ctx := context.Background()
	in := model.Device{Name: "lamp", Address: "172.31.13.10"}
	if _, created, err := m.Upsert(ctx, "discovery", in); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	before := rec.count("device.update")
	if _, created, err := m.Upsert(ctx, "discovery", in); err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if n := rec.count("device.update") - before; n != 0 {
		e, _ := rec.last("device.update")
		t.Fatalf("%d device.update rows for an unchanged device, last %s", n, e.Detail)
	}
}
