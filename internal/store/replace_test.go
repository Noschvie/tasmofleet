package store

import (
	"errors"
	"slices"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func replaceFixture(t *testing.T) (*SQLite, model.DeviceID, model.DeviceID) {
	t.Helper()
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "kitchen", Address: "10.0.0.1", Port: 80, MAC: "aa:bb:cc:00:00:01",
		Topic: "kitchen", Tags: []string{"light"}, Notes: "above the sink", Identity: model.Identity{Family: model.ChipESP8266, Version: "15.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateDevice(ctx, model.Device{Name: "other", Address: "10.0.0.2", Port: 80, MAC: "aa:bb:cc:00:00:09", MACs: []string{"aa:bb:cc:00:00:0a"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, id, other
}

func macsOf(t *testing.T, s *SQLite, id model.DeviceID) []string {
	t.Helper()
	d, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d.MACs
}

// Why: the point of "Replace device" is that the entry keeps its name, tags,
// notes, topic and history while the hardware behind it changes. The old MACs
// must be gone (a returning old board must not match the entry any more) and
// the new ones must find it.
func TestSwapHardwareKeepsTheEntryAndMovesTheMACSet(t *testing.T) {
	s, id, _ := replaceFixture(t)
	old, err := s.SwapHardware(ctx, id, HardwareSwap{ExpectMACs: []string{"aa:bb:cc:00:00:01"}, MAC: "AA:BB:CC:00:00:11",
		MACs: []string{"aa:bb:cc:00:00:12"}, Address: "10.0.0.7", Port: 80, Identity: model.Identity{Family: model.ChipESP32, Version: "15.1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(old, []string{"aa:bb:cc:00:00:01"}) {
		t.Errorf("old MACs = %v", old)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "kitchen" || d.Topic != "kitchen" || d.Notes != "above the sink" || !slices.Equal(d.Tags, []string{"light"}) {
		t.Errorf("entry data lost: %+v", d)
	}
	if d.MAC != "aa:bb:cc:00:00:11" || !slices.Equal(d.MACs, []string{"aa:bb:cc:00:00:11", "aa:bb:cc:00:00:12"}) || d.Address != "10.0.0.7" || d.Identity.Family != model.ChipESP32 {
		t.Errorf("hardware not taken over: %+v", d)
	}
	if _, err := s.GetDeviceByMAC(ctx, "aa:bb:cc:00:00:01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old board still matches the entry: %v", err)
	}
	if g, err := s.GetDeviceByMAC(ctx, "aa:bb:cc:00:00:12"); err != nil || g.ID != id {
		t.Errorf("the new Ethernet MAC does not find the entry: %v", err)
	}
}

// Why: the swap writes the MAC set, the primary MAC, the address and the
// identity. If any step fails the entry must look exactly as before; a
// half-moved entry (new MAC, old address) would point tasmofleet at a dead
// board while believing it is the new one.
func TestSwapHardwareIsAtomic(t *testing.T) {
	cases := []struct {
		name string
		sw   HardwareSwap
	}{
		{"second MAC of the new hardware is bound to another entry", HardwareSwap{MAC: "aa:bb:cc:00:00:21", MACs: []string{"aa:bb:cc:00:00:0a"}, Address: "10.0.0.7"}},
		{"primary MAC is bound to another entry", HardwareSwap{MAC: "aa:bb:cc:00:00:09", Address: "10.0.0.7"}},
		{"address belongs to another entry", HardwareSwap{MAC: "aa:bb:cc:00:00:21", Address: "10.0.0.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id, other := replaceFixture(t)
			before, _ := s.GetDevice(ctx, id)
			beforeOther := macsOf(t, s, other)
			tc.sw.ExpectMACs, tc.sw.Port = []string{"aa:bb:cc:00:00:01"}, 80
			tc.sw.Identity = model.Identity{Family: model.ChipESP32, Version: "99.0.0"}
			if _, err := s.SwapHardware(ctx, id, tc.sw); !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want ErrConflict", err)
			}
			after, _ := s.GetDevice(ctx, id)
			if after.MAC != before.MAC || after.Address != before.Address || after.Identity != before.Identity || !slices.Equal(after.MACs, before.MACs) {
				t.Errorf("partial state after a failed swap:\nbefore %+v\nafter  %+v", before, after)
			}
			if !slices.Equal(macsOf(t, s, other), beforeOther) {
				t.Error("the other entry's MACs changed")
			}
			if _, err := s.GetDeviceByMAC(ctx, tc.sw.MAC); err == nil && tc.sw.MAC != "aa:bb:cc:00:00:09" {
				t.Error("a MAC of the failed swap stayed behind")
			}
		})
	}
}

// Why: between the wizard's start and its end an admin may have edited the
// entry (new MAC, rebinding). Swapping on top of that would silently undo
// the edit, so a changed set refuses.
func TestSwapHardwareRefusesAnEntryThatChanged(t *testing.T) {
	s, id, _ := replaceFixture(t)
	_, err := s.SwapHardware(ctx, id, HardwareSwap{ExpectMACs: []string{"aa:bb:cc:00:00:77"}, MAC: "aa:bb:cc:00:00:21", Address: "10.0.0.7"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.SwapHardware(ctx, 999, HardwareSwap{MAC: "aa:bb:cc:00:00:21", Address: "10.0.0.7"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry: %v", err)
	}
}

// Why: restoring a device whose MAC was never stored reads the live MAC and
// stores it. That must fill only an empty slot: a stored MAC is the identity
// the other checks rely on and must never be overwritten by whatever answers.
func TestAdoptMACFillsOnlyAnEmptySlot(t *testing.T) {
	s, id, _ := replaceFixture(t)
	bare, err := s.CreateDevice(ctx, model.Device{Name: "bare", Address: "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdoptMAC(ctx, bare, "AA:BB:CC:00:00:31"); err != nil {
		t.Fatal(err)
	}
	if d, err := s.GetDeviceByMAC(ctx, "aa:bb:cc:00:00:31"); err != nil || d.ID != bare || d.MAC != "aa:bb:cc:00:00:31" {
		t.Fatalf("adopted MAC not found: %+v %v", d, err)
	}
	if err := s.AdoptMAC(ctx, id, "aa:bb:cc:00:00:32"); !errors.Is(err, ErrConflict) {
		t.Errorf("overwrote a stored MAC: %v", err)
	}
	other2, _ := s.CreateDevice(ctx, model.Device{Name: "bare2", Address: "10.0.0.6"})
	if err := s.AdoptMAC(ctx, other2, "aa:bb:cc:00:00:09"); !errors.Is(err, ErrConflict) {
		t.Errorf("adopted a MAC that belongs to another entry: %v", err)
	}
	if d, _ := s.GetDevice(ctx, other2); d.MAC != "" {
		t.Errorf("failed adoption left a MAC: %q", d.MAC)
	}
}

// Why: replace jobs and plain restores share one table; the kind and the
// target hardware must survive a round trip so the job page and the audit can
// tell them apart, and old rows (kind restore) keep working.
func TestRestoreJobKeepsKindAndTarget(t *testing.T) {
	s, id, _ := replaceFixture(t)
	rid, err := s.CreateRestore(ctx, RestoreJob{DeviceID: id, BackupID: 3, Actor: "a", Kind: KindReplace, NewMAC: "AA:BB:CC:00:00:11", NewAddress: "10.0.0.7", NewPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.GetRestore(ctx, rid)
	if err != nil || j.Kind != KindReplace || j.NewMAC != "aa:bb:cc:00:00:11" || j.NewAddress != "10.0.0.7" || j.NewPort != 80 {
		t.Fatalf("%+v %v", j, err)
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: id, BackupID: 3, Actor: "a"}); !errors.Is(err, ErrConflict) {
		t.Errorf("a plain restore may not run beside a replace of the same entry: %v", err)
	}
	j.State = RestoreDone
	if err := s.UpdateRestore(ctx, j); err != nil {
		t.Fatal(err)
	}
	r2, err := s.CreateRestore(ctx, RestoreJob{DeviceID: id, BackupID: 3, Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRestore(ctx, r2); got.Kind != KindRestore {
		t.Errorf("default kind = %q", got.Kind)
	}
}
