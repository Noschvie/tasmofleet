package web

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Why: Go's plain "404 page not found" has no app shell and no way back; a mistyped or
// stale link must land on the app's own page (visual QA finding 43).
func TestUnknownPathShowsAppNotFoundPage(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/no/such/page", "/transactions/7/nope"} {
		resp, body := e.do(t, "GET", p, "operator", nil)
		if resp.StatusCode != 404 {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
		if !strings.Contains(body, "404 · Not found") || !strings.Contains(body, `<nav class="tb-groups" aria-label="Main">`) || !strings.Contains(body, ">Go to dashboard</a>") {
			t.Fatalf("%s: not the app's not-found page: %.300s", p, body)
		}
	}
}

// Why: a bookmark to /firmware, /transactions or /updates/new must explain that OTA is off
// (an operator reads who can turn it on, #1085) instead of looking like a broken link (finding 43).
func TestSwitchedOffUpdatePagesExplain(t *testing.T) {
	e := setup(t) // no OTA, no firmware service
	for _, p := range []string{"/transactions", "/updates/new", "/firmware"} {
		resp, body := e.do(t, "GET", p, "operator", nil)
		if resp.StatusCode != 404 || !strings.Contains(body, "This feature is switched off. An admin can turn it on.") || !strings.Contains(body, "Back to dashboard") {
			t.Fatalf("%s: status %d, body %.300s", p, resp.StatusCode, body)
		}
	}
}

// Why: the Add devices tabs must not link to /import/tasmoadmin when the web import is off
// (header mode): that tab was a dead link (finding 43).
func TestAddTabsHideImportWhenUnavailable(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "/import/tasmoadmin") {
		t.Fatalf("import tab without the import feature: %s", body)
	}
	if !strings.Contains(body, "Add by address") {
		t.Fatalf("add tabs missing: %.300s", body)
	}
}

// Why: "No devices match" for a fresh install sends the admin in circles; an empty inventory
// says so and offers the ways to add devices, a filter that matches nothing offers to clear the
// filter, and neither shows a table whose header stops short (finding 14, 46; design v2 wording).
func TestDevicesEmptyStates(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Inventory = newInv() })
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, "No devices yet") || !strings.Contains(body, "Add manually") || strings.Contains(body, "<table") {
		t.Fatalf("empty inventory: %.600s", body)
	}
	e = setup(t)
	_, body = e.do(t, "GET", "/devices?q=zzz-no-such-device", "admin", nil)
	if !strings.Contains(body, "No devices match these filters") || !strings.Contains(body, "Clear filters") || strings.Contains(body, "<table") {
		t.Fatalf("no match: %.600s", body)
	}
}

// Why: Go's list and duration forms ("[127.0.0.1/32]", "720h0m0s") are not for people, and a
// blank value must not look like a masked secret (finding 77).
func TestCfgText(t *testing.T) {
	for in, want := range map[string]string{"[127.0.0.1/32]": "127.0.0.1/32", "[a b]": "a, b", "[]": "", "720h0m0s": "30d 0h", "debug": "debug", "http://h/x": "http://h/x"} {
		if got := cfgText(in); got != want {
			t.Errorf("cfgText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why: a service's lowercase clause ("replaced; restored") is not a sentence on a page (finding 55, 57).
func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"replaced; restored": "Replaced; restored.", " already done. ": "Already done.", "": ""} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why (design v2, e403): a signed-in user who opens a page above their role keeps the
// navigation and reads which role the page needs, what theirs allows and which address was
// refused, with a way back; a bare "access denied" leaves them guessing.
func TestForbiddenPageKeepsTheShellAndExplains(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.Message(w, r, http.StatusForbidden, auth.MessageData{BasePath: "/tf", Title: "Only admins can open this page", Text: "You are signed in as ops with the operator role."})
	})
	for _, want := range []string{`<nav class="tb-groups" aria-label="Main">`, "403 · Not allowed", "<h1>Only admins can open this page</h1>", "with the operator role", `<div class="errpage-path">/tf/x</div>`, `href="/tf/">Back to dashboard</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("403 page lacks %q", want)
		}
	}
}

// ---- #1085: refused, missing and switched-off pages keep their place and offer a next step ----

// authedUI wires the real auth service (header mode, the production Pages) in front of the UI, so a refusal is
// drawn by the code a person meets: auth.require decides, Pages.Message draws. The fake guard of the other tests
// answers a bare 403 with no body and cannot show these pages.
type authedUI struct {
	svc  *auth.Service
	inv  *countInv
	jobs *countJobs
	hdr  map[string][]string // the identity headers of a role: "admin", "operator", "none"
}

// countInv and countJobs count the reads of the inventory and the jobs: a principal without a role must cause none.
type countInv struct {
	*fakeInv
	reads atomic.Int64
}

func (c *countInv) Get(ctx context.Context, id model.DeviceID) (model.Device, error) {
	c.reads.Add(1)
	return c.fakeInv.Get(ctx, id)
}

func (c *countInv) List(ctx context.Context) ([]model.Device, error) {
	c.reads.Add(1)
	return c.fakeInv.List(ctx)
}

type countJobs struct {
	*fakeJobs
	reads atomic.Int64
}

func (c *countJobs) ListJobs(ctx context.Context, f store.JobFilter) ([]model.Job, error) {
	c.reads.Add(1)
	return c.fakeJobs.ListJobs(ctx, f)
}

// mwGuard is the guard of the app: Require behind the middleware that stores the principal in the context.
type mwGuard struct{ *auth.Service }

func (g mwGuard) Require(min model.Role) func(http.Handler) http.Handler {
	req := g.Service.Require(min)
	return func(next http.Handler) http.Handler { return g.Middleware(req(next)) }
}

// realAuth returns the Deps tweak that puts the real auth service and Pages in front of the UI.
func realAuth(t *testing.T) (*authedUI, func(*Deps)) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // cleanup of a temp DB
	pages := NewPages()
	svc, err := auth.New(auth.Options{
		Mode:     auth.ModeHeader,
		Proxy:    httpx.Proxy{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}},
		Roles:    auth.RoleMapping{AdminGroups: []string{"admins"}, OperatorGroups: []string{"ops"}},
		Sessions: st, Audit: &fakeAudit{}, Pages: pages, SecretKey: []byte("test-only-secret-key-0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &authedUI{svc: svc, hdr: map[string][]string{
		"admin":    {"X-authentik-username", "ada", "X-authentik-groups", "admins"},
		"operator": {"X-authentik-username", "jonas", "X-authentik-groups", "ops"},
		"none":     {"X-authentik-username", "dave", "X-authentik-groups", "family"},
	}}
	return a, func(d *Deps) {
		a.inv, a.jobs = &countInv{fakeInv: d.Inventory.(*fakeInv)}, &countJobs{fakeJobs: &fakeJobs{}}
		d.Inventory, d.Pages, d.Guard = a.inv, pages, mwGuard{svc}
		if d.Jobs == nil {
			d.Jobs = a.jobs
		}
	}
}

// get is a GET as the named role through the real auth service.
func (a *authedUI) get(t *testing.T, e *env, role, path string, hdr ...string) (*result, string) {
	t.Helper()
	return e.do(t, "GET", path, "", nil, append(slices.Clone(a.hdr[role]), hdr...)...)
}

// litGroup and litTab are the top-bar group and the sub-navigation tab a page lights ("" for none).
func litGroup(body string) string {
	if m := trailBarGroup.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func litTab(body string) string {
	if m := trailLitTab.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// mainOf is the page's own region, for a failure message that a person can read.
func mainOf(body string) string {
	i := strings.Index(body, `<main`)
	if i < 0 {
		return body
	}
	if j := strings.Index(body[i:], "</main>"); j > 0 {
		return body[i : i+j]
	}
	return body[i:]
}

func titleOf(body string) string {
	m := regexp.MustCompile(`<title>(.*?) - TasmoFleet</title>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

func h1Of(body string) string {
	m := regexp.MustCompile(`<h1[^>]*>(.*?)</h1>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// Why (A7-001): a person who signs in but holds no role has no page of the app to go to. The shell drew its
// navigation anyway, with buttons that all answer 403 again, and the card with Sign out that auth meant to show
// never appeared. The card has no navigation and no status bar, offers Sign out with the request's own CSRF
// token as its one button, and the page costs no read of the inventory or the jobs (a principal without a role
// is not a reader of devices).
func TestNoRoleGetsTheSignOutCard(t *testing.T) {
	a, tweak := realAuth(t)
	e := setupWith(t, tweak)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tf/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	for i := 0; i+1 < len(a.hdr["none"]); i += 2 {
		req.Header.Set(a.hdr["none"][i], a.hdr["none"][i+1])
	}
	csrf := a.svc.CSRFToken(req)
	if csrf == "" {
		t.Fatal("the service issues no CSRF token to the header principal: the test cannot name the one Sign out must carry")
	}
	for _, path := range []string{"/", "/devices", "/devices/1", "/audit"} {
		resp, body := a.get(t, e, "none", path)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if strings.Contains(body, `aria-label="Main"`) || strings.Contains(body, `id="statusbar"`) || strings.Contains(body, "kitchen") {
			t.Errorf("%s: a person without a role is shown the app shell: %s", path, mainOf(body))
		}
		if !strings.Contains(body, `<form method="post" action="/tf/logout">`) || !strings.Contains(body, `name="csrf" value="`+csrf+`"`) {
			t.Errorf("%s: no Sign out form with the CSRF token %q: %s", path, csrf, mainOf(body))
		}
		if !strings.Contains(body, `>Sign out</button>`) || strings.Contains(body, "btn-link") {
			t.Errorf("%s: Sign out is not the one real button of the card: %s", path, mainOf(body))
		}
		for _, want := range []string{"You can sign in, but your account has no access to TasmoFleet yet. Ask an admin to give you a role."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the card lacks %q", path, want)
			}
		}
		if strings.Contains(body, "mapped group") || strings.Contains(body, "Back to dashboard") {
			t.Errorf("%s: admin words or a button back to a page that refuses again: %s", path, mainOf(body))
		}
	}
	if n := a.inv.reads.Load(); n != 0 {
		t.Errorf("a principal without a role caused %d inventory reads", n)
	}
	if n := a.jobs.reads.Load(); n != 0 {
		t.Errorf("a principal without a role caused %d job reads", n)
	}
}

// Why (A3-021, A5-023, A2-026, A6-039, A7-027): an operator who opens a page above the role by a bookmark
// is still somewhere. The page lights the group the address belongs to when that group is in their menu (a device's
// page stays inside the device's header with its tabs), the trail says where the page would have been, the first
// button leads back to what the person can open there, the second to the dashboard, and the words say what to do
// instead of listing nouns. A System page has no group in an operator's menu (#1106: it holds only admin pages), so
// nothing is lit, the two-step trail is not drawn and the way back is the dashboard.
func TestOperator403KeepsItsPlace(t *testing.T) {
	a, tweak := realAuth(t)
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) {
		tweak(d)
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
		d.Policy = noPolicy{}
		d.PolicyRules = rulesServiceFor(t, d.Policy)
	})
	cases := []struct {
		path, group, tab, trail, back, backHref string
	}{
		{"/devices/1/settings", "Devices", "", "Devices / kitchen / Not allowed", "Back to kitchen", "/tf/devices/1"},
		{"/devices/1/edit", "Devices", "", "Devices / kitchen / Not allowed", "Back to kitchen", "/tf/devices/1"},
		{"/devices/9999/settings", "Devices", "List", "Devices / List / Not allowed", "Back to Devices", "/tf/devices"},
		{"/devices/new", "Devices", "List", "Devices / List / Not allowed", "Back to Devices", "/tf/devices"},
		{"/policy/rules/new", "Devices", "Policy", "Devices / Policy / Not allowed", "Back to Policy", "/tf/policy"},
		{"/settings/system", "", "", "", "Back to dashboard", "/tf/"},
		{"/audit", "", "", "", "Back to dashboard", "/tf/"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, body := a.get(t, e, "operator", c.path)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := litGroup(body); got != c.group {
				t.Errorf("lit group %q, want %q", got, c.group)
			}
			if got := litTab(body); got != c.tab {
				t.Errorf("lit tab %q, want %q", got, c.tab)
			}
			if c.trail == "" {
				if strings.Contains(body, `id="crumbs"`) {
					t.Errorf("a two-step trail is drawn")
				}
			} else if got := strings.Join(crumbsOf(t, body), " / "); got != c.trail {
				t.Errorf("trail %q, want %q", got, c.trail)
			}
			if !strings.Contains(body, `href="`+c.backHref+`">`+c.back+`</a>`) {
				t.Errorf("first button is not %q to %s: %s", c.back, c.backHref, mainOf(body))
			}
			if !strings.Contains(body, `>Go to dashboard</a>`) && !strings.Contains(body, `>All devices</a>`) {
				t.Errorf("no second way on: %s", mainOf(body))
			}
			if !strings.Contains(body, "Only admins can do this") || !strings.Contains(body, "You are signed in as jonas with the operator role. Ask an admin if you need this.") {
				t.Errorf("the words do not say who can and what to do: %s", mainOf(body))
			}
			if strings.Contains(body, "cannot manage firmware, policy, users or settings") {
				t.Errorf("the 403 still lists admin areas in a sentence of nouns")
			}
		})
	}
	// A device's page stays in the device's header: its name is the one h1, its tabs are there, the refusal is
	// the card's heading, and the tab bar offers the pages the operator may open.
	_, body := a.get(t, e, "operator", "/devices/1/settings")
	if n := strings.Count(body, "<h1"); n != 1 || h1Of(body) != "kitchen" {
		t.Errorf("a device page has one h1, the device name: %d, %q", n, h1Of(body))
	}
	if !strings.Contains(body, `<h2>Only admins can do this</h2>`) {
		t.Errorf("the refusal is not the card's heading under the device name: %s", mainOf(body))
	}
	for _, tab := range []string{">Overview<", ">Backups<"} {
		if !strings.Contains(body, tab) {
			t.Errorf("device tabs lack %s", tab)
		}
	}
	if titleOf(body) != "Only admins can do this" {
		t.Errorf("the page title is %q, not the refusal", titleOf(body))
	}
	// A refusal that is no page the person reads (an htmx request, a POST) draws no device header: it needs
	// no read of the device, the jobs or the router.
	reads := a.inv.reads.Load()
	_, frag := a.get(t, e, "operator", "/devices/1/settings", "HX-Request", "true")
	if strings.Contains(frag, "devhd") || a.inv.reads.Load() != reads {
		t.Errorf("an htmx refusal drew the device header or read the inventory")
	}
}

// adminGETRoutes lists the patterns of the GET routes the UI registers for admins only, read from the source of
// the route table, so a new admin page cannot be forgotten by a hand-written list. An address the reader cannot
// resolve fails the test: silently dropping it would hide exactly the page that needs a place.
func adminGETRoutes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]ast.Expr{}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range gd.Specs {
				if vs, ok := s.(*ast.ValueSpec); ok {
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							consts[n.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	var eval func(ast.Expr) (string, bool)
	eval = func(x ast.Expr) (string, bool) {
		switch x := x.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(x.Value)
			return s, err == nil && x.Kind == token.STRING
		case *ast.Ident:
			if v, ok := consts[x.Name]; ok {
				return eval(v)
			}
		case *ast.ParenExpr:
			return eval(x.X)
		case *ast.BinaryExpr:
			l, ok1 := eval(x.X)
			r, ok2 := eval(x.Y)
			return l + r, ok1 && ok2 && x.Op == token.ADD
		}
		return "", false
	}
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if g, isIdent := call.Args[1].(*ast.Ident); !ok || fn.Name != "route" || !isIdent || g.Name != "adm" {
				return true
			}
			pat, ok := eval(call.Args[0])
			if !ok {
				t.Errorf("%s: cannot read the pattern of an admin route: extend adminGETRoutes", fset.Position(call.Pos()))
			} else if strings.HasPrefix(pat, "GET ") {
				out = append(out, strings.TrimPrefix(pat, "GET "))
			}
			return true
		})
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Why: navOfPath is a second table of the routes (which group a page belongs to) next to the routes themselves, and
// the 403 is drawn before any handler runs. Without this guard a new admin page lights Dashboard for an operator
// while its admin page lights Devices. Every admin-only GET page that an admin can open lights, for an operator's 403,
// the same top-bar group, when the operator's menu has that group; a group the operator does not see (System, which
// holds only admin pages, #1106) is not lit, and no other group is lit instead.
func TestDeniedPageLightsTheAdminPagesGroup(t *testing.T) {
	a, tweak := realAuth(t)
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) {
		tweak(d)
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
		d.Policy = noPolicy{}
		d.PolicyRules = rulesServiceFor(t, d.Policy)
	})
	fill := strings.NewReplacer("{id}", "1", "{batch}", "1", "{name}", "x", "{version}", "x", "{subject...}", "x")
	compared := 0
	for _, pat := range adminGETRoutes(t) {
		if strings.HasSuffix(pat, "/stream") || strings.HasSuffix(pat, "/status") || strings.HasSuffix(pat, ".csv") || strings.HasSuffix(pat, "/download") {
			continue // not a page: a stream, a file
		}
		path := fill.Replace(pat)
		resp, ad := a.get(t, e, "admin", path)
		want := litGroup(ad)
		if resp.StatusCode != http.StatusOK || want == "" {
			continue // a page this setup does not have, or no shell page
		}
		compared++
		resp, op := a.get(t, e, "operator", path)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: an operator got %d", path, resp.StatusCode)
			continue
		}
		if !strings.Contains(op, `class="tb-group" href="`) {
			t.Errorf("%s: the operator's 403 has no top bar", path)
			continue
		}
		if !regexp.MustCompile(`class="tb-group" href="[^"]*"(?: aria-current="true")?>` + regexp.QuoteMeta(want) + `<`).MatchString(op) {
			want = "" // the operator's menu has no such group
		}
		if got := litGroup(op); got != want {
			t.Errorf("%s: the admin's page lights %q, the operator's 403 lights %q", path, want, got)
		}
	}
	if compared < 8 {
		t.Errorf("only %d admin pages were compared: the guard no longer sees the routes", compared)
	}
}

// Why (A4-042, A7-028): an operator cannot fix an environment variable and has no use for its name; the page says
// the feature is off and who can turn it on. An admin gets the setting's name in the mono line and the way to the
// settings. Both keep the group of the page and the path-or-setting line of the design system.
func TestSwitchedOffPagesSpeakToTheRole(t *testing.T) {
	a, tweak := realAuth(t)
	e, _, _, _ := sysEnvFull(t, true, tweak)
	for _, path := range []string{"/firmware", "/updates/new", "/transactions"} {
		resp, op := a.get(t, e, "operator", path)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		for _, bad := range []string{"FIRMWARE_", "TASMOFLEET_", "base URL", "plain http"} {
			if strings.Contains(op, bad) {
				t.Errorf("%s: the operator page names %q", path, bad)
			}
		}
		if !strings.Contains(op, "This feature is switched off. An admin can turn it on.") || !strings.Contains(op, `<div class="errpage-path">/tf`+path+`</div>`) {
			t.Errorf("%s: the operator page lacks the plain words or the path line: %s", path, op)
		}
		_, ad := a.get(t, e, "admin", path)
		if !strings.Contains(ad, `<div class="errpage-path">FIRMWARE_BASE_URL</div>`) || !strings.Contains(ad, "Open the system settings") {
			t.Errorf("%s: the admin page lacks the setting's name or the way to the settings: %s", path, ad)
		}
	}
}

// Why (A7-026, A2-041): the browser tab and the heading are one answer, the product writes typographic apostrophes, and
// the 404 does not guess a cause. The device 403 is the exception that proves the rule: its one h1 is the device's name,
// so its title is the refusal (the card's heading).
func TestErrorTitleIsTheHeading(t *testing.T) {
	a, tweak := realAuth(t)
	e := setupWith(t, tweak)
	for _, c := range []struct {
		role, path string
		status     int
		heading    string
	}{
		{"operator", "/no/such/page", 404, "This page doesn’t exist"},
		{"operator", "/devices/9999", 404, "This device doesn’t exist"},
		{"operator", "/audit", 403, "Only admins can do this"},
	} {
		resp, body := a.get(t, e, c.role, c.path)
		if resp.StatusCode != c.status {
			t.Fatalf("%s: status %d", c.path, resp.StatusCode)
		}
		if h1Of(body) != c.heading || titleOf(body) != c.heading {
			t.Errorf("%s: h1 %q and <title> %q must both be %q", c.path, h1Of(body), titleOf(body), c.heading)
		}
		if strings.Contains(body, "doesn't") {
			t.Errorf("%s: a straight apostrophe in the page", c.path)
		}
	}
	_, body := a.get(t, e, "operator", "/no/such/page")
	if !strings.Contains(body, "Check the address, or go back to the dashboard.") || strings.Contains(body, "older version") {
		t.Errorf("the 404 guesses a cause instead of saying what to do: %s", mainOf(body))
	}
}

// failingInv is an inventory whose single-device read fails (a busy or broken database).
type failingInv struct{ *countInv }

func (failingInv) Get(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, errors.New("database is busy")
}

// Why: the device header on a refusal is a convenience read. When it fails, the refusal must still be shown, in
// the group of the address, and the error text must not reach the page.
func TestRefusedDevicePageSurvivesAFailedDeviceRead(t *testing.T) {
	a, tweak := realAuth(t)
	e := setupWith(t, func(d *Deps) {
		tweak(d)
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
		d.Inventory = failingInv{a.inv}
	})
	resp, body := a.get(t, e, "operator", "/devices/1/settings")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if litGroup(body) != "Devices" || !strings.Contains(body, "Only admins can do this") || strings.Contains(body, "database is busy") {
		t.Errorf("a failed read changed the refusal or leaked its text: %s", mainOf(body))
	}
}
