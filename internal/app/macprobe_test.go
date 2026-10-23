package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// credCheckClient answers Status 5 with a fixed MAC and records, for every
// request, whether its Target carried a web user or password.
type credCheckClient struct {
	tasmota.Client
	mu       sync.Mutex
	mac      string
	requests []string
	withCred []string
}

func (c *credCheckClient) Command(_ context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, cmd)
	if t.HasCredentials() {
		c.withCred = append(c.withCred, cmd)
	}
	return json.RawMessage(`{"StatusNET":{"Mac":"` + c.mac + `"}}`), nil
}

// Why (second security review, point 4): moving a device with a stored password
// must never send that password to the new address before the admin confirmed
// it. The MAC probes (the refused attempt, the other-MAC stop and the confirmed
// move) go out without a web user or password; the fake client asserts it on
// every Target.
func TestMACProbesOfAnAddressChangeCarryNoCredentials(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	client := &credCheckClient{mac: "AA:BB:CC:00:00:01"}
	inv, err := inventory.New(inventory.Options{
		Store: st, Audit: nopAudit{}, Networks: []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")},
		Verifier: macVerifier{client: client},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := inv.Create(ctx, "t", model.Device{Name: "lamp", Address: "172.31.13.5", MAC: "aa:bb:cc:00:00:01", HTTPUser: "admin", HTTPPassword: "stored-pw"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := inv.Get(ctx, id)
	d.Address = "172.31.13.9"
	if err := inv.Update(ctx, "admin", d); !errors.Is(err, inventory.ErrMoveUnverified) {
		t.Fatalf("without the confirmation: %v", err)
	}
	client.mac = "DE:AD:BE:EF:00:01"
	if err := inv.Update(inventory.WithUnverifiedMoveConfirmed(ctx, inventory.MoveTarget("172.31.13.9", 0)), "admin", d); !errors.Is(err, inventory.ErrIdentityMismatch) {
		t.Fatalf("another MAC stops the move: %v", err)
	}
	client.mac = "AA:BB:CC:00:00:01"
	if err := inv.Update(inventory.WithUnverifiedMoveConfirmed(ctx, inventory.MoveTarget("172.31.13.9", 0)), "admin", d); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if len(client.requests) < 3 {
		t.Fatalf("the probes did not run: %v", client.requests)
	}
	if len(client.withCred) != 0 {
		t.Fatalf("a probe carried the stored credentials: %v", client.withCred)
	}
	if got, _ := inv.Get(ctx, id); got.Address != "172.31.13.9" || got.HTTPPassword != "stored-pw" {
		t.Fatalf("%+v", got)
	}
}
