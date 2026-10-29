package store

import (
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func proposal(mac, addr string) ProposalRow {
	return ProposalRow{Kind: ProposalNew, Via: ViaMQTT, Device: model.Device{Name: "lamp", Address: addr, MAC: mac, Topic: "lamp"}}
}

// Why: Tasmota publishes discovery retained, so the same message arrives at
// every reconnect. One row per MAC keeps the list stable.
func TestUpsertProposalIsOneRowPerMAC(t *testing.T) {
	s, _, _ := newStore(t)
	a, err := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10)
	if err != nil {
		t.Fatal(err)
	}
	later := proposal("aa:bb:cc:dd:ee:01", "10.0.0.2")
	later.SeenAt = time.Now().Add(time.Hour)
	b, err := s.UpsertProposal(ctx, later, 10)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("ids %d and %d: second sighting must refresh the row", a.ID, b.ID)
	}
	list, err := s.ListProposals(ctx)
	if err != nil || len(list) != 1 || list[0].Device.Address != "10.0.0.2" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if !list[0].FirstSeen.Equal(a.FirstSeen) || !list[0].SeenAt.After(list[0].FirstSeen) {
		t.Errorf("first/seen = %v / %v", list[0].FirstSeen, list[0].SeenAt)
	}
}

// Why: an admin who dismissed a device must not see it again after every
// restart, but a changed address is news.
func TestDismissedProposalStaysHiddenUntilItChanges(t *testing.T) {
	s, _, _ := newStore(t)
	p, _ := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10)
	if err := s.DismissProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListProposals(ctx); len(l) != 0 {
		t.Fatalf("dismissed proposal came back: %+v", l)
	}
	if _, err := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.9"), 10); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListProposals(ctx); len(l) != 1 {
		t.Fatalf("a moved device must be visible again: %+v", l)
	}
}

// Why: any MQTT publisher can invent MACs; the table must not grow without bound.
func TestUpsertProposalLimit(t *testing.T) {
	s, _, _ := newStore(t)
	for _, mac := range []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"} {
		if _, err := s.UpsertProposal(ctx, proposal(mac, "10.0.0.1"), 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:03", "10.0.0.3"), 2); !errors.Is(err, ErrProposalLimit) {
		t.Fatalf("err = %v, want ErrProposalLimit", err)
	}
	if _, err := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.7"), 2); err != nil {
		t.Fatalf("a known MAC must still be refreshed at the limit: %v", err)
	}
}

// Why: "expire after 30 days unless seen again".
func TestPruneProposalsByLastSeen(t *testing.T) {
	s, _, _ := newStore(t)
	now := time.Now()
	old := proposal("aa:bb:cc:dd:ee:01", "10.0.0.1")
	old.SeenAt = now.Add(-40 * 24 * time.Hour)
	fresh := proposal("aa:bb:cc:dd:ee:02", "10.0.0.2")
	fresh.SeenAt = now
	for _, p := range []ProposalRow{old, fresh} {
		if _, err := s.UpsertProposal(ctx, p, 10); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneProposals(ctx, now.Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if l, _ := s.ListProposals(ctx); len(l) != 1 || l[0].Device.MAC != "aa:bb:cc:dd:ee:02" {
		t.Fatalf("remaining = %+v", l)
	}
}

func TestProposalGetDeleteNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	p, _ := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10)
	got, err := s.GetProposal(ctx, p.ID)
	if err != nil || got.Device.MAC != "aa:bb:cc:dd:ee:01" || got.Kind != ProposalNew {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if err := s.DeleteProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProposal(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.DismissProposal(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismiss unknown: %v", err)
	}
}

// Why: proposals about devices we manage (address change, conflict) are the
// spoofing signal; fake "new" MACs must not use up their room.
func TestProposalLimitIsPerClass(t *testing.T) {
	s, _, _ := newStore(t)
	for _, mac := range []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"} {
		if _, err := s.UpsertProposal(ctx, proposal(mac, "10.0.0.1"), 2); err != nil {
			t.Fatal(err)
		}
	}
	rel := proposal("aa:bb:cc:dd:ee:03", "10.0.0.3")
	rel.Kind, rel.ExistingID = ProposalConflict, 7
	if _, err := s.UpsertProposal(ctx, rel, 2); err != nil {
		t.Fatalf("a conflict must not count against the full 'new' class: %v", err)
	}
}

// Why: a changed name or topic is news for a dismissed proposal too.
func TestDismissedProposalReappearsWhenNameOrTopicChanges(t *testing.T) {
	s, _, _ := newStore(t)
	p, _ := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10)
	if err := s.DismissProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	q := proposal("aa:bb:cc:dd:ee:01", "10.0.0.1")
	q.Device.Topic = "other"
	if _, err := s.UpsertProposal(ctx, q, 10); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListProposals(ctx); len(l) != 1 {
		t.Fatalf("list = %+v", l)
	}
}

// Why: "Ignore" must be reversible. A device an admin ignored by mistake would
// otherwise stay hidden for good, so the ignored rows are listable and can be
// made visible again.
func TestDismissedProposalCanBeListedAndUndismissed(t *testing.T) {
	s, _, _ := newStore(t)
	p, _ := s.UpsertProposal(ctx, proposal("aa:bb:cc:dd:ee:01", "10.0.0.1"), 10)
	if err := s.DismissProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListDismissedProposals(ctx); len(l) != 1 || l[0].ID != p.ID {
		t.Fatalf("dismissed = %+v", l)
	}
	if err := s.UndismissProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListProposals(ctx); len(l) != 1 {
		t.Fatalf("visible after undismiss = %+v", l)
	}
	if l, _ := s.ListDismissedProposals(ctx); len(l) != 0 {
		t.Fatalf("still listed as dismissed: %+v", l)
	}
	if err := s.UndismissProposal(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// Why: #130, the hostname a device reported is shown in the New devices list. A new sighting
// refreshes it, and it must not count as a change of the proposal (an ignored device stays ignored).
func TestProposalHostnameIsStoredAndRefreshedWithoutUnhiding(t *testing.T) {
	s, _, _ := newStore(t)
	p := proposal("aa:bb:cc:dd:ee:01", "10.0.0.1")
	p.Device.Net.Hostname = "tasmota-1234"
	row, err := s.UpsertProposal(ctx, p, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DismissProposal(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	p.Device.Net.Hostname = "renamed-host"
	if _, err := s.UpsertProposal(ctx, p, 10); err != nil {
		t.Fatal(err)
	}
	ign, err := s.ListDismissedProposals(ctx)
	if err != nil || len(ign) != 1 || ign[0].Device.Net.Hostname != "renamed-host" {
		t.Fatalf("dismissed = %+v, %v (hostname must be refreshed, the proposal must stay ignored)", ign, err)
	}
}
