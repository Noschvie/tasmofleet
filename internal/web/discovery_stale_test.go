package web

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// reuseEnv is the Add page on the real discovery manager and the real store, because the bug of #745
// lives between them: the store hands a deleted row's number to the next device, and only the
// manager can tell the two apart by version. A fake that records its arguments cannot show that.
type reuseEnv struct {
	*env
	st  *store.SQLite
	mgr *discovery.Manager
}

func newReuseEnv(t *testing.T) *reuseEnv {
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
	rec := &fakeAudit{}
	inv, err := inventory.New(inventory.Options{Store: st, Audit: rec})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := discovery.New(discovery.Options{Inventory: inv, Store: st, Audit: rec})
	if err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, func(d *Deps) { d.Inventory, d.Discovery = inv, mgr })
	return &reuseEnv{env: e, st: st, mgr: mgr}
}

// announce stores a new-device proposal the way a sighting does and returns it as the page lists it.
func (r *reuseEnv) announce(t *testing.T, mac, addr, name string) discovery.Proposal {
	t.Helper()
	row := store.ProposalRow{Kind: store.ProposalNew, Via: discovery.ViaMQTT, SeenAt: time.Now(),
		Device: model.Device{Name: name, Address: addr, MAC: mac}}
	if _, err := r.st.UpsertProposal(t.Context(), row, 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.listed(t) {
		if p.Device.MAC == mac {
			return p
		}
	}
	t.Fatalf("%s is not listed after it was announced", mac)
	return discovery.Proposal{}
}

func (r *reuseEnv) listed(t *testing.T) []discovery.Proposal {
	t.Helper()
	p, err := r.mgr.Proposals(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *reuseEnv) ignored(t *testing.T) []discovery.Proposal {
	t.Helper()
	p, err := r.mgr.Ignored(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// reused is the page of an admin who loaded the Add page while "shown" was listed. Its row is deleted
// (Add, the overtaken cleanup and the 30-day prune all do that), then "fresh" is announced and gets
// the same number. The admin has never seen "fresh".
func (r *reuseEnv) reused(t *testing.T) (shown, fresh discovery.Proposal) {
	t.Helper()
	shown = r.announce(t, "aa:bb:cc:dd:ee:01", "10.0.0.50", "shown")
	if err := r.st.DeleteProposal(t.Context(), shown.ID); err != nil {
		t.Fatal(err)
	}
	fresh = r.announce(t, "aa:bb:cc:dd:ee:02", "10.0.0.51", "fresh")
	if fresh.ID != shown.ID {
		t.Fatalf("the new row has number %d, not the reused %d: the premise of #745 no longer holds, rethink these tests", fresh.ID, shown.ID)
	}
	return shown, fresh
}

func (r *reuseEnv) post(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	res, body := r.do(t, "POST", path, "admin", form)
	return res.StatusCode, body
}

func num(p discovery.Proposal) string { return strconv.FormatInt(p.ID, 10) }

const outOfDate = "The form is out of date. Reload the page and try again."

// Why (#745): the Ignore button of a page loaded earlier must not hide a device that appeared after
// the page was loaded and that the admin never saw. The number is the same, the version is not.
func TestIgnoreOnAStalePageLeavesTheNewDeviceListed(t *testing.T) {
	r := newReuseEnv(t)
	shown, fresh := r.reused(t)
	status, body := r.post(t, "/discovery/proposals/"+num(shown)+"/dismiss", url.Values{"version": {shown.Version}})
	if status != 409 || !strings.Contains(body, outOfDate) {
		t.Fatalf("status %d, out-of-date message shown: %v", status, strings.Contains(body, outOfDate))
	}
	if got := r.listed(t); len(got) != 1 || got[0].Device.MAC != fresh.Device.MAC {
		t.Fatalf("the new device is no longer listed under New devices: %+v", got)
	}
	if got := r.ignored(t); len(got) != 0 {
		t.Fatalf("something was ignored: %+v", got)
	}
}

// Why (#745): "Ignore selected" posts id:version pairs and used to drop the version, so a ticked
// row whose number now names another device hid that device.
func TestIgnoreSelectedOnAStalePageLeavesTheNewDeviceListed(t *testing.T) {
	r := newReuseEnv(t)
	shown, fresh := r.reused(t)
	status, body := r.post(t, "/discovery/proposals/dismiss", url.Values{"id": {num(shown) + ":" + shown.Version}})
	if status != 409 || !strings.Contains(body, outOfDate) {
		t.Fatalf("status %d, out-of-date message shown: %v", status, strings.Contains(body, outOfDate))
	}
	if got := r.listed(t); len(got) != 1 || got[0].Device.MAC != fresh.Device.MAC {
		t.Fatalf("the new device is no longer listed under New devices: %+v", got)
	}
}

// Why: one stale tick must not cost the admin the ticks that are still right, and the page must
// say that some were left alone, or the admin believes they are all ignored.
func TestIgnoreSelectedIgnoresTheCurrentTicksAndReportsTheStaleOne(t *testing.T) {
	r := newReuseEnv(t)
	shown, fresh := r.reused(t)
	other := r.announce(t, "aa:bb:cc:dd:ee:03", "10.0.0.52", "other")
	status, body := r.post(t, "/discovery/proposals/dismiss", url.Values{"id": {num(shown) + ":" + shown.Version, num(other) + ":" + other.Version}})
	if status != 409 || !strings.Contains(body, "1 device ignored, 1 not") || !strings.Contains(body, outOfDate) {
		t.Fatalf("status %d, body lacks the partial report", status)
	}
	if got := r.listed(t); len(got) != 1 || got[0].Device.MAC != fresh.Device.MAC {
		t.Fatalf("listed = %+v, want only the new device", got)
	}
	if got := r.ignored(t); len(got) != 1 || got[0].Device.MAC != other.Device.MAC {
		t.Fatalf("ignored = %+v, want only the current tick", got)
	}
}

// Why (#745): Restore on a page loaded earlier must not bring back a device that was ignored on
// purpose only because it now has the number of the one the page showed.
func TestRestoreOnAStalePageLeavesTheNewDeviceIgnored(t *testing.T) {
	r := newReuseEnv(t)
	shown, fresh := r.reused(t)
	if err := r.mgr.Dismiss(t.Context(), "admin", fresh.ID, fresh.Version); err != nil { // another admin ignores it
		t.Fatal(err)
	}
	status, body := r.post(t, "/discovery/proposals/"+num(shown)+"/undismiss", url.Values{"version": {shown.Version}})
	if status != 409 || !strings.Contains(body, outOfDate) {
		t.Fatalf("status %d, out-of-date message shown: %v", status, strings.Contains(body, outOfDate))
	}
	if got := r.ignored(t); len(got) != 1 || got[0].Device.MAC != fresh.Device.MAC {
		t.Fatalf("the device is no longer ignored: %+v", got)
	}
	if got := r.listed(t); len(got) != 0 {
		t.Fatalf("the ignored device was restored: %+v", got)
	}
}

// Why (#745): a request without the version cannot be checked. It is refused, not treated as
// "whatever is behind the number", or a client that omits it is back to the old behaviour.
func TestIgnoreAndRestoreWithoutVersionAreRefused(t *testing.T) {
	r := newReuseEnv(t)
	p := r.announce(t, "aa:bb:cc:dd:ee:01", "10.0.0.50", "lamp")
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/discovery/proposals/" + num(p) + "/dismiss", url.Values{}},
		{"/discovery/proposals/" + num(p) + "/undismiss", url.Values{}},
		{"/discovery/proposals/dismiss", url.Values{"id": {num(p)}}},
		{"/discovery/proposals/dismiss", url.Values{"id": {num(p) + ":"}}},
	} {
		if status, body := r.post(t, c.path, c.form); status != 422 || !strings.Contains(body, outOfDate) {
			t.Errorf("%s %v: status %d", c.path, c.form, status)
		}
	}
	if got := r.listed(t); len(got) != 1 {
		t.Fatalf("a refused request changed the list: %+v", got)
	}
}

// Why: the check must not get in the way of the normal case: the version the page showed
// ignores the device, and the version the Ignored list showed restores it.
func TestIgnoreAndRestoreWithTheShownVersionWork(t *testing.T) {
	r := newReuseEnv(t)
	p := r.announce(t, "aa:bb:cc:dd:ee:01", "10.0.0.50", "lamp")
	if status, _ := r.post(t, "/discovery/proposals/"+num(p)+"/dismiss", url.Values{"version": {p.Version}}); status != 303 {
		t.Fatalf("ignore: status %d", status)
	}
	ign := r.ignored(t)
	if len(ign) != 1 || len(r.listed(t)) != 0 {
		t.Fatalf("ignored = %+v", ign)
	}
	if status, _ := r.post(t, "/discovery/proposals/"+num(ign[0])+"/undismiss", url.Values{"version": {ign[0].Version}}); status != 303 {
		t.Fatalf("restore: status %d", status)
	}
	if len(r.listed(t)) != 1 || len(r.ignored(t)) != 0 {
		t.Fatal("the device is not listed again")
	}
}

// Why (#745): the server can only check a version the page sends. Every Ignore and Restore button
// of the Add page and of the scan results must carry the version of its own row.
func TestIgnoreAndRestoreButtonsCarryTheShownVersion(t *testing.T) {
	now := time.Now()
	fd := &fakeDiscovery{
		props:   []discovery.Proposal{{ID: 4, Version: "v4", Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "fresh", Address: "10.0.0.50", MAC: "aa:bb:cc:dd:ee:01"}}},
		ignored: []discovery.Proposal{{ID: 5, Version: "v5", Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "old", Address: "10.0.0.51", MAC: "aa:bb:cc:dd:ee:05"}}},
		scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 2, Done: 2, StartedAt: now, FinishedAt: now,
			Hits: []discovery.Hit{
				{Address: "10.0.0.50", Port: 80, MAC: "aa:bb:cc:dd:ee:01", Name: "fresh", Outcome: "new proposal"},
				{Address: "10.0.0.51", Port: 80, MAC: "aa:bb:cc:dd:ee:05", Name: "old", Outcome: "new proposal"},
			}},
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, add := e.do(t, "GET", "/devices/new?ignored=1", "admin", nil)
	_, scan := e.do(t, "GET", "/discovery/scan", "admin", nil)
	for _, c := range []struct {
		page, name, want string
		count            int
	}{
		// the desktop table and the phone cards each have one button per row
		{add, "Add page Ignore", `formaction="/tf/discovery/proposals/4/dismiss" name="version" value="v4"`, 2},
		{add, "Add page Restore", `formaction="/tf/discovery/proposals/5/undismiss" name="version" value="v5"`, 2},
		{scan, "scan Ignore", `formaction="/tf/discovery/proposals/4/dismiss" name="version" value="v4"`, 2},
		{scan, "scan Show again", `formaction="/tf/discovery/proposals/5/undismiss" name="version" value="v5"`, 2},
	} {
		if n := strings.Count(c.page, c.want); n != c.count {
			t.Errorf("%s: %d buttons carry the version, want %d", c.name, n, c.count)
		}
	}
}
