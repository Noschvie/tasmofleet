package store

import "testing"

// Why (#884): the schedule loop remembers which clock it last read its schedule on, across a
// restart. A key that is missing must say so (the first start after an upgrade is not a change), and
// a second write must replace the first (one row per loop, never a growing list).
func TestKVRoundTripAndReplace(t *testing.T) {
	s, _, _ := newStore(t)
	if _, ok, err := s.GetKV(ctx, "k"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v, want not found", ok, err)
	}
	if err := s.PutKV(ctx, "k", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutKV(ctx, "k", "two"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutKV(ctx, "other", "x"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.GetKV(ctx, "k"); err != nil || !ok || v != "two" {
		t.Errorf("got %q ok=%v err=%v, want the replacing value", v, ok, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key = 'k'").Scan(&n); err != nil || n != 1 {
		t.Errorf("rows for the key: %d %v, want 1", n, err)
	}
}
