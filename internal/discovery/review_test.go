package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Regression tests for the Opus review of M2-3 (F1-F9).

// Why (audit row 14, 2026-10-21; replaces F1, tightened by the security review):
// "Update address" works for a device with a password, but only on the
// admin's explicit confirmation for exactly that address when the MAC is not
// proven (a password-protected device cannot answer the credential-less probe,
// an impostor can). The scan alone never moves it
// (TestCredentialedAddressChangeIsASuggestionWithoutAuditNoise).
func TestUpdateAddressOfCredentialedDeviceNeedsConfirmationWhenMACIsUnproven(t *testing.T) {
	e, id, p := credentialedProposal(t)
	if _, err := e.mgr.AcceptVersion(bg, "admin", p.ID, p.Version); !errors.Is(err, inventory.ErrMoveUnverified) {
		t.Fatalf("without the confirmation: %v", err)
	}
	other := inventory.WithUnverifiedMoveConfirmed(bg, inventory.MoveTarget("172.31.13.99", 80))
	if _, err := e.mgr.AcceptVersion(other, "admin", p.ID, p.Version); !errors.Is(err, inventory.ErrMoveUnverified) {
		t.Fatalf("a confirmation for another address must not count: %v", err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" || d.HTTPPassword != "secret" {
		t.Fatalf("refused attempts moved the device: %+v", d)
	}
	ctx := inventory.WithUnverifiedMoveConfirmed(bg, inventory.MoveTarget("172.31.13.40", 80))
	if _, err := e.mgr.AcceptVersion(ctx, "admin", p.ID, p.Version); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.40" || d.HTTPPassword != "secret" {
		t.Fatalf("device = %+v", d)
	}
	if n := auditDetail(e, "discovery.accept", `"mac_unverified":true`); n != 1 {
		t.Fatalf("the accept must be audited as MAC unverified, found %d", n)
	}
}

// Why (second security review): an answer without the password is no proof for
// a password-protected device, so even a matching MAC needs the explicit
// confirmation; the audit entry says so.
func TestUpdateAddressWithMatchingMACStillNeedsConfirmationForCredentialedDevice(t *testing.T) {
	e, id, p := credentialedProposal(t)
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	if _, err := e.mgr.AcceptVersion(bg, "admin", p.ID, p.Version); !errors.Is(err, inventory.ErrMoveUnverified) {
		t.Fatalf("matching MAC without the confirmation: %v", err)
	}
	ctx := inventory.WithUnverifiedMoveConfirmed(bg, inventory.MoveTarget("172.31.13.40", 80))
	if _, err := e.mgr.AcceptVersion(ctx, "admin", p.ID, p.Version); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.40" {
		t.Fatalf("device = %+v", d)
	}
	if n := auditDetail(e, "discovery.accept", `"mac_unverified":true`); n != 1 {
		t.Fatalf("found %d", n)
	}
}

// Why: even with the admin's consent, a host that answers with another MAC
// is not the device.
func TestUpdateAddressStopsOnAnotherMAC(t *testing.T) {
	e, id, p := credentialedProposal(t)
	e.ver.macs["172.31.13.40"] = "02:00:5e:79:43:7d"
	ctx := inventory.WithUnverifiedMoveConfirmed(bg, inventory.MoveTarget("172.31.13.40", 80))
	if _, err := e.mgr.AcceptVersion(ctx, "admin", p.ID, p.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if d, _ := e.inv.Get(bg, id); d.Address != "172.31.13.31" {
		t.Fatalf("device = %+v", d)
	}
}

// Why (F1b): a device without MAC must not get one bound from a claim, or any
// MAC naming its topic would take it over together with its credentials.
func TestUnboundDeviceIsNeverBoundOrMovedByDiscovery(t *testing.T) {
	for _, creds := range []bool{true, false} {
		e := newEnv(t, "172.31.0.0/16", nil)
		d := model.Device{Name: "lamp", Address: "172.31.13.31", Topic: "lamp"}
		if creds {
			d.HTTPUser, d.HTTPPassword = "admin", "secret"
		}
		id, _ := e.inv.Create(bg, "t", d)
		e.mgr.handleMessage(bg, discoveryMsg("02AAAAAAAAAA", "172.31.13.66", "lamp"))
		p := mustProposals(t, e)
		if len(p) != 1 {
			t.Fatalf("creds=%v proposals = %+v", creds, p)
		}
		e.ver.macs["172.31.13.66"] = "02:aa:aa:aa:aa:aa"
		if _, err := e.mgr.AcceptVersion(bg, "admin", p[0].ID, p[0].Version); !errors.Is(err, ErrNotAcceptable) {
			t.Fatalf("creds=%v: err = %v", creds, err)
		}
		if got, _ := e.inv.Get(bg, id); got.Address != "172.31.13.31" || got.MAC != "" {
			t.Fatalf("creds=%v: device = %+v", creds, got)
		}
	}
}

// Why (F2): a publisher must not make the queue hold megabytes per message.
func TestOversizedPayloadIsDroppedBeforeQueueing(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	big := make([]byte, 4<<20)
	for i := 0; i < 50; i++ {
		e.mgr.HandleMQTT(mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: fmt.Sprintf("x%d", i), Payload: big})
	}
	e.mgr.qmu.Lock()
	n, b := len(e.mgr.pending), e.mgr.qbytes
	e.mgr.qmu.Unlock()
	if n != 0 || b != 0 {
		t.Fatalf("queued %d messages, %d bytes", n, b)
	}
}

// Why (F2): many just-legal messages are bounded in bytes, not only in count.
func TestQueueIsBoundedInBytes(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	pl := make([]byte, maxDiscoveryPayload)
	for i := 0; i < 2000; i++ {
		e.mgr.HandleMQTT(mqttbus.Message{Kind: mqttbus.KindDiscovery, Topic: fmt.Sprintf("x%d", i), Payload: pl})
	}
	e.mgr.qmu.Lock()
	b := e.mgr.qbytes
	e.mgr.qmu.Unlock()
	if b > maxQueuedBytes {
		t.Fatalf("queued %d bytes, limit %d", b, maxQueuedBytes)
	}
	// draining restores the budget
	for {
		if _, ok := e.mgr.pop(); !ok {
			break
		}
	}
	e.mgr.qmu.Lock()
	b = e.mgr.qbytes
	e.mgr.qmu.Unlock()
	if b != 0 {
		t.Fatalf("qbytes after drain = %d", b)
	}
}

// Why (F3): junk MACs must not reset the rate limit of a real MAC; a full
// table fails closed.
func TestRateLimitSurvivesTableOverflow(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	target := "02:00:5e:d2:2c:14"
	if !e.mgr.allowMAC(target) || e.mgr.allowMAC(target) {
		t.Fatal("limit not working")
	}
	for i := 0; i < maxGapEntries+10; i++ {
		e.mgr.allowMAC(fmt.Sprintf("02:00:00:%02x:%02x:%02x", i>>16&255, i>>8&255, i&255))
	}
	if e.mgr.allowMAC(target) {
		t.Fatal("rate limit bypassed after junk MACs")
	}
	if e.mgr.allowMAC("02:ff:ff:ff:ff:ff") {
		t.Error("a full table must refuse new MACs, not forget old ones")
	}
}

// Why (F4): fake MACs filling the "new" list must not hide the proposals
// about devices we manage (the spoofing signal).
func TestNewProposalsCannotCrowdOutAddressChanges(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	for i := 0; i < maxNewProposals; i++ {
		mac := fmt.Sprintf("0200%08X", i)
		e.mgr.handleMessage(bg, discoveryMsg(mac, "172.31.9.9", fmt.Sprintf("t%d", i)))
		e.mgr.last = map[string]time.Time{}
	}
	var dropped []string
	e.mgr.o.OnDropped = func(kind string) { dropped = append(dropped, kind) }
	e.mgr.handleMessage(bg, discoveryMsg("02FFFFFFFFFF", "172.31.9.10", "overflow"))
	rows, _ := e.st.ListProposals(bg)
	if len(rows) != maxNewProposals {
		t.Fatalf("%d proposals, want the cap %d", len(rows), maxNewProposals)
	}
	// A dropped proposal is visible to the operator (metric), not silent.
	if len(dropped) != 1 || dropped[0] != KindNew {
		t.Fatalf("dropped callbacks %v, want one %q", dropped, KindNew)
	}
	e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}) //nolint:errcheck // setup
	e.mgr.handleMessage(bg, discoveryMsg("02AAAAAAAAAA", "172.31.13.50", "lamp"))
	found := false
	for _, p := range mustProposals(t, e) {
		found = found || p.Kind == KindConflict
	}
	if !found {
		t.Fatal("the conflict proposal was dropped because the new-device list is full")
	}
}

// Why (F5): discovery must never create a device. If the matched record is
// deleted between the lookup and the Upsert, the claim must not resurrect it.
func TestDeletedDeviceIsNotResurrectedByUpsert(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14"})
	e.mgr.o.Inventory = deleteAfterList{Inventory: e.mgr.o.Inventory, del: func() { _ = e.inv.Delete(bg, "t", id) }}
	e.ver.macs["172.31.13.77"] = "02:00:5e:d2:2c:14"
	e.mgr.handleMessage(bg, discoveryMsg("02005ED22C14", "172.31.13.77", "lamp"))
	if devs, _ := e.inv.List(bg); len(devs) != 0 {
		t.Fatalf("discovery created %+v", devs)
	}
}

type deleteAfterList struct {
	Inventory
	del func()
}

func (d deleteAfterList) List(ctx context.Context) ([]model.Device, error) {
	devs, err := d.Inventory.List(ctx)
	d.del()
	return devs, err
}

// Why (F6): an edit that lands between the admin's confirmation and the write
// must not be overwritten by the values read before.
func TestAcceptMoveDoesNotOverwriteConcurrentEdit(t *testing.T) {
	e, id, p := plainProposal(t)
	e.ver.macs["172.31.13.40"] = "02:00:5e:d2:2c:14"
	e.mgr.o.Inventory = editDuringVerify{Inventory: e.mgr.o.Inventory, edit: func() {
		d, _ := e.inv.Get(bg, id)
		d.Notes = "edited by an admin"
		_ = e.inv.Update(bg, "t", d)
	}}
	e.mgr.o.Verifier = hookVerifier{inner: e.ver, hook: e.mgr.o.Inventory.(editDuringVerify).edit}
	if _, err := e.mgr.AcceptVersion(bg, "admin", p.ID, p.Version); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if d, _ := e.inv.Get(bg, id); d.Notes != "edited by an admin" || d.Address != "172.31.13.31" {
		t.Fatalf("device = %+v", d)
	}
}

type editDuringVerify struct {
	Inventory
	edit func()
}

type hookVerifier struct {
	inner interface {
		VerifyMAC(context.Context, string, int) (string, error)
	}
	hook func()
}

func (h hookVerifier) VerifyMAC(ctx context.Context, a string, p int) (string, error) {
	h.hook()
	return h.inner.VerifyMAC(ctx, a, p)
}

// Why (F8): a dismissed proposal is hidden and must not be acceptable by id.
func TestDismissedProposalCannotBeAccepted(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02BBBBBBBBBB", "172.31.13.77", "x"))
	p := mustProposals(t, e)
	if err := e.mgr.Dismiss(bg, "admin", p[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.AcceptVersion(bg, "admin", p[0].ID, p[0].Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// Why (F7): bidi overrides and zero-width characters let a name imitate
// another device; they are removed.
func TestFormatCharactersAreStrippedFromNames(t *testing.T) {
	got := cleanName("Lamp\u202e\u200bgnp.txt", "02:00:5e:d2:2c:14")
	if strings.ContainsAny(got, "\u202e\u200b") {
		t.Fatalf("name = %q", got)
	}
	if cleanTopic("a\u200bb") != "" {
		t.Error("a topic with a format character must be dropped")
	}
}

// Why (F9): the version is compared as a whole 256-bit digest.
func TestVersionIsFullDigest(t *testing.T) {
	_, _, p := credentialedProposal(t)
	if len(p.Version) != 64 {
		t.Fatalf("version length %d", len(p.Version))
	}
}

// Why (F7/F8): every field the admin could be shown and Accept applies is in
// the version, including the full topic; and a changed name or topic makes a
// dismissed proposal visible again.
func TestChangedNameOrTopicChangesVersionAndUnhides(t *testing.T) {
	e := newEnv(t, "172.31.0.0/16", nil)
	e.mgr.handleMessage(bg, discoveryMsg("02BBBBBBBBBB", "172.31.13.77", "x"))
	p := mustProposals(t, e)[0]
	if err := e.mgr.Dismiss(bg, "admin", p.ID); err != nil {
		t.Fatal(err)
	}
	e.mgr.last = map[string]time.Time{}
	e.mgr.handleMessage(bg, discoveryMsg("02BBBBBBBBBB", "172.31.13.77", "other-topic"))
	got := mustProposals(t, e)
	if len(got) != 1 || got[0].Version == p.Version {
		t.Fatalf("proposals = %+v", got)
	}
}

// Why: DEVICE_NETWORKS can change on the settings page while the server runs; a
// scan must be planned against the networks in force now.
func TestScanFollowsTheNetworkSetLive(t *testing.T) {
	set := tasmota.NewNetworkSet(nil)
	e := newEnv(t, "127.0.0.0/8", func(o *Options) {
		o.NetworkSet = set
		o.Client = tasmota.NewHTTPClient(tasmota.Options{Networks: set})
	})
	e.run(t)
	cidr := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	if _, err := e.mgr.StartScan(bg, "admin", cidr); !errors.Is(err, ErrScanRange) {
		t.Fatalf("an empty set must refuse every range: %v", err)
	}
	set.Set([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	if _, err := e.mgr.StartScan(bg, "admin", cidr); err != nil {
		t.Fatalf("after adding the network: %v", err)
	}
}
