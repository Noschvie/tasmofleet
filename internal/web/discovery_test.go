package web

import (
	"context"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

type fakeDiscovery struct {
	mu        sync.Mutex
	props     []discovery.Proposal
	scan      discovery.ScanStatus
	started   [][]netip.Prefix
	startErr  error
	accepted  []int64
	versions  []string
	dismissed []int64
	ignored   []discovery.Proposal
	undismiss []int64
	unproven  bool // AcceptVersion demands the unverified-move confirmation
	confirmed []string
	passwords []string // discovery.DevicePassword of each AcceptVersion
}

func (f *fakeDiscovery) Ignored(context.Context) ([]discovery.Proposal, error) { return f.ignored, nil }
func (f *fakeDiscovery) Undismiss(_ context.Context, _ string, id int64) error {
	f.undismiss = append(f.undismiss, id)
	return nil
}

func (f *fakeDiscovery) Proposals(context.Context) ([]discovery.Proposal, error) { return f.props, nil }
func (f *fakeDiscovery) StartScan(_ context.Context, _ string, c []netip.Prefix) (discovery.ScanID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, c)
	return "abc123", f.startErr
}
func (f *fakeDiscovery) CancelScan(context.Context, string, discovery.ScanID) error { return nil }
func (f *fakeDiscovery) Accept(_ context.Context, _ string, id int64) (model.DeviceID, error) {
	f.accepted = append(f.accepted, id)
	return 5, nil
}
func (f *fakeDiscovery) AcceptVersion(ctx context.Context, actor string, id int64, v string) (model.DeviceID, error) {
	if f.unproven {
		const target = "10.0.0.51:80"
		if !inventory.UnverifiedMoveConfirmed(ctx, target) {
			return 0, &inventory.UnverifiedMoveError{Target: target}
		}
		f.confirmed = append(f.confirmed, target)
	}
	f.versions = append(f.versions, v)
	f.passwords = append(f.passwords, discovery.DevicePassword(ctx))
	return f.Accept(ctx, actor, id)
}
func (f *fakeDiscovery) Dismiss(_ context.Context, _ string, id int64) error {
	f.dismissed = append(f.dismissed, id)
	return nil
}
func (f *fakeDiscovery) Scan(id discovery.ScanID) (discovery.ScanStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scan, id == f.scan.ID
}
func (f *fakeDiscovery) Scans() []discovery.ScanStatus {
	if f.scan.ID == "" {
		return nil
	}
	return []discovery.ScanStatus{f.scan}
}

func setupDiscovery(t *testing.T) (*env, *fakeDiscovery) {
	t.Helper()
	fd := &fakeDiscovery{
		props: []discovery.Proposal{
			{ID: 1, Version: "v1", Kind: discovery.KindNew, Via: "mqtt", SeenAt: time.Now(), Device: model.Device{Name: `<script>alert(1)</script>`, Address: "10.0.0.50", MAC: "aa:bb:cc:dd:ee:01"}},
			{ID: 2, Version: "v2", Kind: discovery.KindAddressChange, Via: "scan", ExistingID: 2, SeenAt: time.Now(), Device: model.Device{Name: "kitchen", Address: "10.0.0.51", MAC: "aa:bb:cc:dd:ee:02"}},
			{ID: 3, Kind: discovery.KindConflict, Via: "mqtt", ExistingID: 2, SeenAt: time.Now(), Device: model.Device{Name: "x", Address: "10.0.0.52", MAC: "aa:bb:cc:dd:ee:03"}},
		},
		scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 4, Done: 4, StartedAt: time.Now(),
			Hits: []discovery.Hit{{Address: "10.0.0.50", Port: 80, MAC: "aa:bb:cc:dd:ee:01", Name: "n", Outcome: "new proposal"}}},
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	// device 2 is bound and has no credentials: its address change can be confirmed here
	d2 := e.inv.devs[2]
	d2.MAC = "aa:bb:cc:dd:ee:02"
	e.inv.devs[2] = d2
	return e, fd
}

// Why: proposals show addresses and let a user change the inventory, so
// operators must not see or use any discovery page.
func TestDiscoveryIsAdminOnly(t *testing.T) {
	e, fd := setupDiscovery(t)
	for _, c := range [][2]string{{"GET", "/discovery"}, {"POST", "/discovery/scan"}, {"GET", "/discovery/scan/abc123"},
		{"GET", "/discovery/scan/abc123/stream"}, {"POST", "/discovery/proposals/1/accept"}, {"POST", "/discovery/proposals/1/dismiss"}, {"POST", "/discovery/proposals/1/undismiss"}} {
		if r, _ := e.do(t, c[0], c[1], "operator", url.Values{}); r.StatusCode != 403 {
			t.Errorf("%s %s as operator: %d", c[0], c[1], r.StatusCode)
		}
	}
	if len(fd.accepted)+len(fd.started) != 0 {
		t.Fatal("service was called")
	}
}

// Why: names come from the network (any host can publish them); the page
// must show them escaped.
func TestDiscoveryPageEscapesClaimedNames(t *testing.T) {
	e, _ := setupDiscovery(t)
	r, body := e.do(t, "GET", "/discovery", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("claimed name rendered unescaped")
	}
	moves := body[strings.Index(body, `aria-label="Address changed"`):strings.Index(body, `aria-label="Conflicts"`)]
	conflicts := body[strings.Index(body, `aria-label="Conflicts"`):strings.Index(body, "New devices (")]
	if !strings.Contains(moves, ">Apply</button>") || !strings.Contains(conflicts, "Resolve this in the inventory") || strings.Contains(conflicts, ">Apply</button>") {
		t.Error("address-change needs a confirm button, a conflict must not offer accept")
	}
	if !strings.Contains(body, `name="version" value="v2"`) {
		t.Error("the confirm form must carry the shown version")
	}
}

// Why: the server probes the MAC itself at accept time, so the admin needs no
// "I checked" checkbox (over-restriction audit #13); a POST without the shown
// version (a stale form) must still do nothing.
func TestAddressChangeNeedsNoCheckbox(t *testing.T) {
	e, fd := setupDiscovery(t)
	r, _ := e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{})
	if r.StatusCode != 422 || len(fd.accepted) != 0 {
		t.Fatalf("status %d, accepted %v", r.StatusCode, fd.accepted)
	}
	r, body := e.do(t, "GET", "/discovery", "admin", nil)
	if strings.Contains(body, `name="confirm"`) || strings.Contains(body, "I checked") {
		t.Fatalf("the checkbox is gone: %d", r.StatusCode)
	}
	r, _ = e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"v2"}})
	if r.StatusCode != 303 || len(fd.accepted) != 1 || fd.accepted[0] != 2 {
		t.Fatalf("status %d, accepted %v", r.StatusCode, fd.accepted)
	}
	r, _ = e.do(t, "POST", "/discovery/proposals/1/accept", "admin", url.Values{"version": {"v1"}})
	if r.StatusCode != 303 || len(fd.accepted) != 2 {
		t.Fatalf("new device: status %d, accepted %v", r.StatusCode, fd.accepted)
	}
	r, _ = e.do(t, "POST", "/discovery/proposals/1/dismiss", "admin", url.Values{})
	if r.StatusCode != 303 || len(fd.dismissed) != 1 {
		t.Fatalf("dismiss: status %d", r.StatusCode)
	}
}

func TestScanStartParsesRangesAndRedirects(t *testing.T) {
	e, fd := setupDiscovery(t)
	r, _ := e.do(t, "POST", "/discovery/scan", "admin", url.Values{"cidrs": {"10.0.0.0/24, 10.0.1.7\n10.0.2.0/30"}})
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/discovery/scan/abc123") {
		t.Fatalf("status %d, location %q", r.StatusCode, r.Header.Get("Location"))
	}
	if len(fd.started) != 1 || len(fd.started[0]) != 3 || fd.started[0][1].String() != "10.0.1.7/32" {
		t.Fatalf("ranges = %v", fd.started)
	}
	r, body := e.do(t, "POST", "/discovery/scan", "admin", url.Values{"cidrs": {"not-an-ip"}})
	if r.StatusCode != 422 || !strings.Contains(body, "not a range like") || len(fd.started) != 1 {
		t.Fatalf("bad input: status %d", r.StatusCode)
	}
}

// Why: results stream over SSE; the final event must hold the finished table
// and the stream must end by itself (no reconnect loop).
func TestScanStreamSendsFinalFragmentAndEnds(t *testing.T) {
	e, _ := setupDiscovery(t)
	r, body := e.do(t, "GET", "/discovery/scan/abc123/stream", "admin", nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "event: scan") || !strings.Contains(body, `<span class="bdg t-accent">New</span>`) || strings.Contains(body, "sse-connect") {
		t.Fatalf("body = %q", body)
	}
	if r, _ := e.do(t, "GET", "/discovery/scan/nope/stream", "admin", nil); r.StatusCode != 404 {
		t.Errorf("unknown scan: %d", r.StatusCode)
	}
}

// Why: without the shown version the accept cannot be tied to what the
// admin saw, so it must not reach the service.
func TestAcceptWithoutVersionIsRefused(t *testing.T) {
	e, fd := setupDiscovery(t)
	r, _ := e.do(t, "POST", "/discovery/proposals/1/accept", "admin", url.Values{})
	if r.StatusCode != 422 || len(fd.accepted) != 0 {
		t.Fatalf("status %d, accepted %v", r.StatusCode, fd.accepted)
	}
}

// Why (audit row 14, 2026-10-21): the admin can use "Update address" for a
// credentialed device, and the page says that the stored password goes to the
// new address. A device without a password gets no such note.
func TestAddressChangeOfCredentialedDeviceIsOfferedWithANote(t *testing.T) {
	e, fd := setupDiscovery(t)
	_, plain := e.do(t, "GET", "/discovery", "admin", nil)
	if strings.Contains(plain, "sends the password to the new address") {
		t.Fatalf("no password, no note:\n%s", plain)
	}
	d := e.inv.devs[2]
	d.HTTPPassword = "pw"
	e.inv.devs[2] = d
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	if !strings.Contains(body, ">Apply</button>") || !strings.Contains(body, "Apply sends the password to the new address.") {
		t.Fatalf("credentialed address change lacks the button or the note:\n%s", body)
	}
	r, _ := e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"v2"}})
	if r.StatusCode == 409 || len(fd.accepted) != 1 {
		t.Fatalf("status %d, accepted %v", r.StatusCode, fd.accepted)
	}
}

// Why (F8): only proposals that are listed (not dismissed) can be accepted,
// so the confirmation check cannot be skipped through a hidden one.
func TestAcceptOfUnlistedProposalIsNotFound(t *testing.T) {
	e, fd := setupDiscovery(t)
	r, _ := e.do(t, "POST", "/discovery/proposals/99/accept", "admin", url.Values{"version": {"x"}})
	if r.StatusCode != 404 || len(fd.accepted) != 0 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestTooManyRangesRefused(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("10.0.0.0/30\n")
	}
	if _, err := parseCIDRs(sb.String()); err == nil {
		t.Fatal("40 ranges accepted")
	}
}

// Why: "Ignore" must be reversible. The page offers "Show ignored" and each
// ignored device a "Restore" that reaches the service; only admins may use it.
func TestIgnoredDevicesCanBeShownAndUnignored(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.ignored = []discovery.Proposal{{ID: 9, Kind: discovery.KindNew, SeenAt: time.Now(), Device: model.Device{Name: "hidden-lamp", Address: "10.0.0.60", MAC: "aa:bb:cc:dd:ee:09"}}}
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	if !strings.Contains(body, "Show ignored") || strings.Contains(body, "hidden-lamp") {
		t.Fatal("the ignored list must be offered but not shown by default")
	}
	_, body = e.do(t, "GET", "/discovery?ignored=1", "admin", nil)
	if !strings.Contains(body, "hidden-lamp") || !strings.Contains(body, `formaction="/tf/discovery/proposals/9/undismiss">Restore</button>`) {
		t.Fatal("ignored devices are not listed with a Restore button")
	}
	r, _ := e.do(t, "POST", "/discovery/proposals/9/undismiss", "admin", url.Values{})
	if r.StatusCode != 303 || len(fd.undismiss) != 1 || fd.undismiss[0] != 9 {
		t.Fatalf("status %d, undismissed %v", r.StatusCode, fd.undismiss)
	}
}

// Why: the suggestion for a known device must be recognisable as such, not as
// a new one: it is listed under "Address changed" with the stored device's name
// and both addresses, never among the new devices.
func TestAddressChangeIsLabelledAsKnownDevice(t *testing.T) {
	e, _ := setupDiscovery(t)
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	i, j := strings.Index(body, `aria-label="Address changed"`), strings.Index(body, "New devices (")
	if i < 0 || j < i {
		t.Fatal("no Address changed section above the new devices")
	}
	moves, news := body[i:j], body[j:]
	if !strings.Contains(moves, "Address changed (1)") || !strings.Contains(moves, `href="/tf/devices/2"`) || !strings.Contains(moves, "<b>10.0.0.51</b>") {
		t.Errorf("address change: %s", moves)
	}
	if strings.Contains(news, "10.0.0.51") || !strings.Contains(body, "New devices (1)") {
		t.Error("an address change must not count or show as a new device")
	}
}

// Why (security review, 2026-10-21): when the new address cannot prove the MAC,
// "Update address" first shows a stronger warning naming the address, sends
// nothing, and only a repeat with the server-held token (tied to that proposal,
// version and address) moves the device. A forged or foreign token does not.
func TestUpdateAddressOfCredentialedDeviceAsksAgainWhenMACIsUnproven(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.unproven = true
	d := e.inv.devs[2]
	d.HTTPPassword = "pw"
	e.inv.devs[2] = d
	r, body := e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"v2"}})
	if r.StatusCode != 200 || !strings.Contains(body, "Could not verify that") || !strings.Contains(body, "10.0.0.51:80") || len(fd.accepted) != 0 {
		t.Fatalf("status %d accepted %v: %s", r.StatusCode, fd.accepted, body)
	}
	token := confirmToken(t, body)
	if r, _ := e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"other"}, "token": {token}}); r.StatusCode != 409 || len(fd.accepted) != 0 {
		t.Fatalf("a token for another version must not work: %d", r.StatusCode)
	}
	_, body = e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"v2"}})
	token = confirmToken(t, body)
	if r, _ := e.do(t, "POST", "/discovery/proposals/2/accept", "operator", url.Values{"version": {"v2"}, "token": {token}}); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	r, _ = e.do(t, "POST", "/discovery/proposals/2/accept", "admin", url.Values{"version": {"v2"}, "token": {token}})
	if r.StatusCode != 303 || len(fd.accepted) != 1 || len(fd.confirmed) != 1 {
		t.Fatalf("confirmed move: %d accepted %v confirmed %v", r.StatusCode, fd.accepted, fd.confirmed)
	}
}

// Why: the device edit form behaves the same: an unproven move of a device with
// a stored password is refused until the admin saves again with the token for
// that address; typing a new password needs no proof.
func TestDeviceEditToUnprovenAddressNeedsTheStrongerConfirmation(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.inv.unproven = true
	form := url.Values{"name": {"kitchen"}, "address": {"10.0.0.99"}, "port": {"80"}}
	r, body := e.do(t, "POST", "/devices/1", "admin", form)
	if r.StatusCode != 409 || !strings.Contains(body, "Could not verify that") || !strings.Contains(body, "10.0.0.99:80") || e.inv.confirmedMoves != 0 {
		t.Fatalf("%d: %s", r.StatusCode, body)
	}
	form.Set("token", confirmToken(t, body))
	if r, body := e.do(t, "POST", "/devices/1", "admin", form); r.StatusCode != 303 || e.inv.confirmedMoves != 1 {
		t.Fatalf("confirmed save: %d %s", r.StatusCode, body)
	}
}

// Why: on a phone the card is the only place to tell which device you are
// adding: it carries the name, the address and where and when it was seen
// (the prototype's card; the MAC and topic stay on the desktop table).
func TestDiscoveryProposalFactsStayVisibleOnPhones(t *testing.T) {
	e, _ := setupDiscovery(t)
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	i := strings.Index(body, `<div class="add-cards">`)
	if i < 0 {
		t.Fatal("no phone cards")
	}
	card := body[i:]
	for _, want := range []string{"&lt;script&gt;", "10.0.0.50", "MQTT discovery · last seen", `<time datetime=`, `value="1:v1"`, `href="/tf/devices/new?proposal=1"`} {
		if !strings.Contains(card, want) {
			t.Errorf("phone card lacks %q", want)
		}
	}
}

// Why: the "Add devices" badge tells an admin that devices wait for them: new devices to add and
// known devices at a new address to confirm (design v2). A conflict is no such device, and
// operators cannot open the page, so they get no badge.
func TestAddDevicesBadgeCountsNewDevices(t *testing.T) {
	e, _ := setupDiscovery(t)
	badge := `<span class="nav-badge"><span class="sr-only">, new: </span>2</span>`
	if _, body := e.do(t, "GET", "/devices", "admin", nil); !strings.Contains(body, badge) {
		t.Errorf("admin: no badge counting the new device and the address change")
	}
	if _, body := e.do(t, "GET", "/devices", "operator", nil); strings.Contains(body, `class="nav-badge"`) {
		t.Errorf("operator sees the Add devices badge")
	}
}

// Why (N20): accepting a proposal uses the fleet default device password only
// when the form asks for it, and the value comes from the server, never from
// the browser (it is not in the form at all).
func TestDiscoveryAcceptUsesTheDefaultPasswordOnlyWhenAsked(t *testing.T) {
	_, fd := setupDiscovery(t)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.DefaultDevicePassword = func() string { return "fleet-fake-pw" }
	})
	if r, _ := e.do(t, "POST", "/discovery/proposals/1/accept", "admin", url.Values{"version": {"v1"}}); r.StatusCode != 303 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/discovery/proposals/1/accept", "admin", url.Values{"version": {"v1"}, "use_default_password": {"1"}}); r.StatusCode != 303 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if len(fd.passwords) != 2 || fd.passwords[0] != "" || fd.passwords[1] != "fleet-fake-pw" {
		t.Fatalf("passwords handed to accept: %q", fd.passwords)
	}
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	if strings.Contains(body, "fleet-fake-pw") {
		t.Fatal("the default password must never reach the browser")
	}
}
