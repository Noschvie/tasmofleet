package inventory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: discovery must never create a device. If the record it matched is
// deleted before the write, the claim must not bring it back.
func TestUpsertExistingNeverCreates(t *testing.T) {
	m, _, _, _, v := newSvcV(t)
	ctx := context.Background()
	id, err := m.Create(ctx, "t", dev("lamp", "172.31.13.10"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "t", id); err != nil {
		t.Fatal(err)
	}
	in := dev("lamp", "172.31.13.11")
	in.MAC = "02:00:5e:79:43:7d"
	v.macs["172.31.13.11"] = in.MAC
	if _, created, err := m.UpsertExisting(ctx, "discovery", id, in); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if all, _ := m.List(ctx); len(all) != 0 {
		t.Fatalf("device resurrected: %+v", all)
	}
}

// Why: an admin edit that lands after the caller read the record must not be
// overwritten by values read before.
func TestUpdateIfRefusesChangedRecord(t *testing.T) {
	m, _, _, _, _ := newSvcV(t)
	ctx := context.Background()
	id, _ := m.Create(ctx, "t", dev("lamp", "172.31.13.10"))
	read, _ := m.Get(ctx, id)
	edited := read
	edited.Notes = "admin edit"
	if err := m.Update(ctx, "admin", edited); err != nil {
		t.Fatal(err)
	}
	next := read
	next.Address = "172.31.13.20"
	if err := m.UpdateIf(ctx, "discovery", read, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	cur, _ := m.Get(ctx, id)
	if cur.Notes != "admin edit" || cur.Address != "172.31.13.10" {
		t.Fatalf("record = %+v", cur)
	}
	ok := cur
	ok.Address = "172.31.13.20"
	if err := m.UpdateIf(ctx, "discovery", cur, ok); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(ctx, id); got.Address != "172.31.13.20" {
		t.Fatalf("address = %s", got.Address)
	}
}

// Why (N2): the audit entry of a device delete is the record of whether its
// backups were kept or deleted with it; a plain Delete records no choice.
func TestDeleteWithRecordsTheBackupsChoice(t *testing.T) {
	m, _, al, _, _ := newSvcV(t)
	ctx := context.Background()
	id, _ := m.Create(ctx, "t", dev("lamp", "172.31.13.10"))
	if err := m.DeleteWith(ctx, "adm", id, DeleteOptions{Backups: "keep"}); err != nil {
		t.Fatal(err)
	}
	if e := al.last(t); e.Action != "device.delete" || !strings.Contains(e.Detail, `"backups":"keep"`) {
		t.Fatalf("audit %+v", e)
	}
	id, _ = m.Create(ctx, "t", dev("lamp", "172.31.13.10"))
	if err := m.Delete(ctx, "adm", id); err != nil {
		t.Fatal(err)
	}
	if e := al.last(t); strings.Contains(e.Detail, "backups") {
		t.Errorf("plain delete names a choice: %+v", e)
	}
}

// Why (#271 L6): the transport code learns FWReach and the password-refusal mark on its own
// schedule. A discovery or import update that read the record before such a write must still land;
// a conflict there would drop a legitimate address change for a field it does not own.
func TestUpdateIfIgnoresWhatTheTransportCodeLearns(t *testing.T) {
	m, st, _, _, _ := newSvcV(t)
	ctx := context.Background()
	id, _ := m.Create(ctx, "t", dev("lamp", "172.31.13.10"))
	read, _ := m.Get(ctx, id)
	st.mu.Lock() // what SetFWReach and MarkMQTTPasswordBad do in the real store
	d := st.devs[id]
	d.FWReach = model.FWReach{State: model.FWReachYes, At: time.Now()}
	d.MQTTPasswordBadAt = time.Now()
	st.devs[id] = d
	st.mu.Unlock()
	next := read
	next.Address = "172.31.13.20"
	if err := m.UpdateIf(ctx, "discovery", read, next); err != nil {
		t.Fatalf("a learning write turned the update into a conflict: %v", err)
	}
	got, _ := m.Get(ctx, id)
	if got.Address != "172.31.13.20" || got.FWReach.State != model.FWReachYes || got.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("update lost the learned state or the change: %+v", got)
	}
}
