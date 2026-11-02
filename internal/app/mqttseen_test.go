package app

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// racingStore runs hook once, right after the first device read: the moment an
// admin's form submit can land between "read the record" and "write it back".
type racingStore struct {
	*store.SQLite
	mu   sync.Mutex
	hook func()
}

func (s *racingStore) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	d, err := s.SQLite.GetDevice(ctx, id)
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return d, err
}

// Why (#502): onMQTTSeen only flags the device as "reports to our broker". It runs in the
// background, so it must never undo what a person saved a moment earlier: an admin who just
// typed a new web password would be back on the old one without noticing, and the audit log would
// blame "system". The capability must still be recorded afterwards, because the health tracker
// calls onMQTTSeen once per device and run: a dropped write would leave the flag unset until the
// next restart.
func TestOnMQTTSeenKeepsAnAdminEditMadeAfterItsRead(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	sq, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close() }) // temp DB
	st := &racingStore{SQLite: sq}
	inv, err := inventory.New(inventory.Options{
		Store: st, Audit: nopAudit{}, Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	id, err := inv.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.5", Topic: "lamp", HTTPUser: "admin", HTTPPassword: "old-pw"})
	if err != nil {
		t.Fatal(err)
	}
	st.hook = func() {
		edit, gerr := sq.GetDevice(ctx, id)
		if gerr != nil {
			t.Error(gerr)
			return
		}
		edit.HTTPPassword, edit.Notes = "new-pw", "rewired"
		if uerr := inv.Update(ctx, "admin", edit); uerr != nil {
			t.Error(uerr)
		}
	}
	a := &runtime{svc: &Services{Inventory: inv}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), bgContext: ctx}

	a.onMQTTSeen(id, "lamp")

	got, err := inv.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPPassword != "new-pw" || got.Notes != "rewired" {
		t.Fatalf("the admin's edit was overwritten by the stale copy: password %q, notes %q", got.HTTPPassword, got.Notes)
	}
	if got.MQTT != model.MQTTOurs {
		t.Fatalf("MQTT = %q: the capability was dropped after the conflict, and nothing asks again before a restart", got.MQTT)
	}
}
