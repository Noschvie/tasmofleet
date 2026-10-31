package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

const testSub = "5f2c0e9a7b1d4c3e8a6f0b2d9c1e7a43b8d5f0a2c4e6b8d0f1a3c5e7b9d1f3a5"

type memPrincipals struct {
	mu   sync.Mutex
	rows map[string]store.PrincipalRecord
}

func (m *memPrincipals) UpsertPrincipal(_ context.Context, p store.PrincipalRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]store.PrincipalRecord{}
	}
	// Like the SQL upsert: the sign-in name replaces the provider name, the chosen name stays, and
	// ListPrincipals shows the chosen name when there is one.
	if p.Custom == "" {
		p.Custom = m.rows[p.Subject].Custom
	}
	if p.Provider == "" {
		p.Provider = p.DisplayName
	}
	if p.Custom != "" {
		p.DisplayName = p.Custom
	}
	m.rows[p.Subject] = p
	return nil
}

func (m *memPrincipals) ListPrincipals(context.Context) ([]store.PrincipalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.PrincipalRecord
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

// outsideTooltips is body without the title and data-tip attributes, where a
// full subject is allowed.
var tooltipAttr = regexp.MustCompile(`\s(?:title|data-tip)="[^"]*"`)

// outsideTooltips is the page without what nobody reads as text: tooltips and the values of
// select options (the audit filter's actor select keeps the subject as its value).
func outsideTooltips(body string) string {
	return optionValue.ReplaceAllString(tooltipAttr.ReplaceAllString(body, ""), "<option")
}

var optionValue = regexp.MustCompile(`<option value="[^"]*"`)

// Why: an OIDC subject is a 64-character hash nobody can read. Every place
// that shows a principal must show the name captured at sign-in, the kind as a
// badge, and keep the subject in the tooltip only.
func TestWhoShowsNamesBadgesAndHidesSubjects(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	pr.Record(context.Background(), model.Principal{Subject: testSub, Name: "alice", Method: "oidc"})
	pr.Record(context.Background(), model.Principal{Subject: "proxy-user", Name: "Pat", Method: "header"})
	cases := []struct {
		in           string
		want, absent []string
	}{
		{testSub, []string{">alice <span class=\"tag\">SSO</span>", `title="` + testSub + `"`}, nil},
		{"local:admin", []string{">admin <span class=\"tag\">Local</span>"}, nil},
		{"discovery", []string{`<span class="tag">Discovery</span>`}, nil},
		{"system", []string{`<span class="tag">System</span>`}, nil},
		{"cli", []string{`<span class="tag">CLI</span>`}, nil},
		{"proxy-user", []string{">Pat <span class=\"tag\">Proxy</span>"}, nil},
		{"tasmofleet (OTA restore for job 3, requested by " + testSub + ")", []string{"requested by <span", ">alice</span>"}, nil},
		{strings.Repeat("ab", 32), []string{">Unknown user</span>", `title="` + strings.Repeat("ab", 32) + `"`}, []string{"abababab<"}},
		{"plain", []string{"plain"}, []string{"<span"}},
	}
	for _, c := range cases {
		got := string(pr.Who(c.in))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("Who(%.20q) = %s, lacks %q", c.in, got, w)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("Who(%.20q) = %s, has %q", c.in, got, a)
			}
		}
		if m := regexp.MustCompile(`[0-9a-f]{64}`).FindString(outsideTooltips(got)); m != "" {
			t.Errorf("Who(%.20q) prints a raw subject outside a tooltip: %s", c.in, got)
		}
	}
}

// Why: the display name can change at the identity provider; the next sign-in
// must replace it, so old audit rows show the current name of the same subject.
func TestRecordUpdatesAChangedName(t *testing.T) {
	st := &memPrincipals{}
	pr := newPrincipalDir(st)
	ctx := context.Background()
	pr.Record(ctx, model.Principal{Subject: testSub, Name: "alice", Method: "oidc"})
	pr.Record(ctx, model.Principal{Subject: testSub, Name: "Alice K.", Method: "oidc"})
	if st.rows[testSub].DisplayName != "Alice K." || !strings.Contains(string(pr.Who(testSub)), "Alice K.") {
		t.Fatalf("%+v", st.rows[testSub])
	}
}

// Why: the audit user filter must find rows by what the page shows: the display
// name, "admin (local)" or a system actor, as well as a whole subject.
func TestCandidatesResolveWhatThePageShows(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	pr.Record(context.Background(), model.Principal{Subject: testSub, Name: "Alice", Method: "oidc"})
	for q, want := range map[string]string{"alice": testSub, "admin (local)": "local:admin", "Admin": "local:admin", "Discovery": "discovery", testSub: testSub} {
		got, amb := pr.Candidates(q)
		if amb || !slices.Contains(got, want) {
			t.Errorf("Candidates(%q) = %v ambiguous=%v, lacks %q", q, got, amb, want)
		}
	}
}

const otherSub = "aa11bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900"

// Why (spoofing): two people can pick the same display name at the identity
// provider. The audit log must not let one pass as the other, so both are shown
// with the start of their subject, and the full subject stays in the tooltip.
func TestSharedDisplayNamesGetASubjectSuffix(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	ctx := context.Background()
	pr.Record(ctx, model.Principal{Subject: testSub, Name: "alice", Method: "oidc"})
	if got := string(pr.Who(testSub)); strings.Contains(got, "\u00b7") {
		t.Fatalf("a unique name needs no suffix: %s", got)
	}
	pr.Record(ctx, model.Principal{Subject: otherSub, Name: "Alice", Method: "oidc"}) // case-insensitive clash
	for sub, suffix := range map[string]string{testSub: "5f2c0e", otherSub: "aa11bb"} {
		got := string(pr.Who(sub))
		if !strings.Contains(got, " \u00b7 "+suffix) || !strings.Contains(got, `title="`+sub+`"`) {
			t.Errorf("Who(%s) = %s", sub[:6], got)
		}
	}
}

// Why (spoofing): a user who sets their display name to "System", "Discovery"
// or "admin (local)" must not look like the system or a local account.
func TestReservedDisplayNamesGetASubjectSuffix(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	ctx := context.Background()
	pr.Record(ctx, model.Principal{Subject: "local:alice", Name: "alice", Method: "local"})
	for i, name := range []string{"System", "discovery", "Root (local)", "alice"} {
		sub := strings.Repeat(string(rune('a'+i)), 40)
		pr.Record(ctx, model.Principal{Subject: sub, Name: name, Method: "oidc"})
		if got := string(pr.Who(sub)); !strings.Contains(got, " \u00b7 ") {
			t.Errorf("%q is reserved but shown plain: %s", name, got)
		}
	}
	if got := string(pr.Who("local:alice")); strings.Contains(got, "\u00b7") || !strings.Contains(got, `alice <span class="tag">Local</span>`) {
		t.Errorf("the local account itself: %s", got)
	}
}

// Why: a name two people share must not silently merge their audit rows: the
// filter offers both subjects to choose from. The row itself keeps the subject.
func TestAmbiguousNameAsksInsteadOfMerging(t *testing.T) {
	st := &memPrincipals{}
	e := setupWith(t, func(d *Deps) { d.Principals = st })
	ctx := context.Background()
	_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: testSub, DisplayName: "alice", Kind: "sso"})
	_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: otherSub, DisplayName: "Alice", Kind: "sso"})
	e.aud.events = []model.AuditEvent{
		{ID: 1, At: time.Now(), Actor: testSub, Action: "device.update", Target: "device:1", Result: "ok"},
		{ID: 2, At: time.Now(), Actor: otherSub, Action: "device.update", Target: "device:1", Result: "ok"},
	}
	_, body := e.do(t, "GET", "/audit?since=all&actor=alice", "admin", nil)
	if !strings.Contains(body, "Several users match") || !strings.Contains(body, "actor="+testSub) || !strings.Contains(body, "actor="+otherSub) || strings.Contains(body, "device.update") {
		t.Fatalf("expected a chooser and no merged rows: %s", body[strings.Index(body, "<main"):])
	}
	_, body = e.do(t, "GET", "/audit?since=all&actor="+otherSub, "admin", nil)
	if strings.Contains(body, "Several users") || strings.Count(body, `class="who" title=`) != 1 {
		t.Fatalf("a whole subject is unambiguous and selects its rows only (%d who)", strings.Count(body, `class="who" title=`))
	}
}

// Why: guard for the whole app: a page that prints an actor must go through
// the principal helper. The audit page is the one with every kind of actor.
func TestAuditPageNeverPrintsRawSubjects(t *testing.T) {
	st := &memPrincipals{}
	e := setupWith(t, func(d *Deps) { d.Principals = st })
	e.aud.events = []model.AuditEvent{
		{ID: 1, At: time.Now(), Actor: testSub, Action: "device.update", Target: "device:1", Result: "ok"},
		{ID: 2, At: time.Now(), Actor: "local:admin", Action: "device.update", Target: "device:1", Result: "ok"},
		{ID: 3, At: time.Now(), Actor: "discovery", Action: "device.upsert", Target: "device:1", Result: "ok"},
	}
	_ = st.UpsertPrincipal(context.Background(), store.PrincipalRecord{Subject: testSub, DisplayName: "alice", Kind: "sso"})
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(outsideTooltips(body)) {
		t.Fatal("a raw subject is printed outside a tooltip")
	}
	for _, want := range []string{"alice", `admin <span class="tag">Local</span>`, "Discovery"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit page lacks %q", want)
		}
	}
	// the user filter accepts the display name
	_, body = e.do(t, "GET", "/audit?since=all&actor=Alice", "admin", nil)
	if !strings.Contains(body, "1 events") && !strings.Contains(body, "1 event") {
		t.Fatalf("name filter: %s", body[strings.Index(body, "Pages"):])
	}
}

// Why: a proxy that sends a hash as the user name (real data: a 40-character
// id) would put that hash on the page as the "display name"; a long token is
// always shortened, whatever field it came from.
func TestLongDisplayNamesAreShortened(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	hash := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	pr.Record(context.Background(), model.Principal{Subject: hash, Name: hash, Method: "header"})
	got := string(pr.Who(hash))
	if strings.Contains(outsideTooltips(got), hash) || !strings.Contains(got, "user 9f86d081") {
		t.Fatalf("%s", got)
	}
}

// Why: the Users page is where an admin decides whom to sign out, so two SSO
// users whose IdP name is identical must stay distinguishable (anti-spoofing
// suffix), and a user must not pass for a local account by choosing its name.
// The columns the page promises must all be there, and the controls must follow
// the locked state.
func TestUsersPageShowsDisambiguatedNamesAndMethodColumns(t *testing.T) {
	pages := NewPages()
	mem := &memPrincipals{}
	setupWith(t, func(d *Deps) { d.Pages = pages; d.Principals = mem })
	for _, r := range []store.PrincipalRecord{
		{Subject: "aaaaaa-sub", DisplayName: "Sam", Kind: "sso"},
		{Subject: "bbbbbb-sub", DisplayName: "sam", Kind: "sso"},
		{Subject: "cccccc-sub", DisplayName: "admin (local)", Kind: "sso"},
	} {
		if err := mem.UpsertPrincipal(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Now().Add(-3 * time.Hour)
	data := auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", Users: []auth.UserRow{
		{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin", Guards: usersFixture().Users[0].Guards},
		{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", Guards: allowed},
		{Subject: "aaaaaa-sub", Name: "Sam", Method: auth.MethodSSO, Role: "operator", LastLogin: when, Sessions: 2},
		{Subject: "bbbbbb-sub", Name: "sam", Method: auth.MethodSSO, Role: "", Sessions: 0},
		{Subject: "cccccc-sub", Name: "admin (local)", Method: auth.MethodSSO, Role: "admin", Sessions: 1},
	}}
	render := func(d auth.UsersPageData) string {
		rec := httptest.NewRecorder()
		h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.WithPrincipal(r.Context(), model.Principal{Subject: "local:admin", Name: "admin", Role: model.RoleAdmin, Method: "local"})
			pages.Users(w, r.WithContext(ctx), http.StatusOK, d)
		}))
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tf/users", nil))
		return rec.Body.String()
	}
	locked := data
	locked.NeedStepUp, locked.StepUpKind = true, auth.StepUpOIDC
	body := render(locked)
	for _, want := range []string{"SSO", " · 2 sessions", `action="/tf/oidc/stepup"`, "Sam · aaaaaa", "sam · bbbbbb", "admin (local) · cccccc", ">Role unknown<"} {
		if !strings.Contains(body, want) {
			t.Errorf("locked page lacks %q", want)
		}
	}
	if strings.Contains(body, "/tf/users/sessions") || strings.Contains(body, `action="/tf/users/local/`) {
		t.Error("locked page offers changes")
	}
	open := render(data)
	if strings.Count(open, `action="/tf/users/sessions"`) != 2 || strings.Contains(open, `value="bbbbbb-sub"`) {
		t.Errorf("End sessions only for SSO rows that have sessions")
	}
	if !strings.Contains(open, `action="/tf/users/local/ops"`) || !strings.Contains(open, "Add local user") {
		t.Error("unlocked page lacks the local account controls")
	}
	// finding 88: no change on the signed-in admin's own row (no Disable, Delete or role change)
	if strings.Contains(open, `action="/tf/users/local/admin"`) {
		t.Error("the own row offers account changes")
	}
	view := data
	view.ViewOnly = true
	if body := render(view); !strings.Contains(body, "View only") || strings.Contains(body, "/tf/users/sessions") {
		t.Error("view-only page offers changes or does not say why")
	}
}

// Why: a proxy-header account is the third kind on the Users page. Behind the proxy it is the
// only authority, so the row is display only: it says the proxy manages it (not the identity
// provider) and offers neither account changes nor End sessions (QA #88).
func TestUsersPageShowsProxyHeaderAccountsAsDisplayOnly(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages; d.Principals = &memPrincipals{} })
	render := func(d auth.UsersPageData) string {
		rec := httptest.NewRecorder()
		h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.WithPrincipal(r.Context(), model.Principal{Subject: "local:admin", Name: "admin", Role: model.RoleAdmin, Method: "local"})
			pages.Users(w, r.WithContext(ctx), http.StatusOK, d)
		}))
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tf/users", nil))
		return rec.Body.String()
	}
	proxy := auth.Guard{Reason: "Managed by the authenticating proxy."}
	rows := []auth.UserRow{
		{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin", Guards: usersFixture().Users[0].Guards},
		{Subject: "hdr-1", Name: "Pat", Method: auth.MethodHeader, Role: "operator", Sessions: 1, Guards: auth.UserGuards{Role: proxy, Disable: proxy, Delete: proxy, Reset: proxy, Profile: proxy}},
	}
	body := render(auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", Users: rows})
	for _, want := range []string{"Proxy header", "Managed by the authenticating proxy", "The proxy signs this user in on every request"} {
		if !strings.Contains(body, want) {
			t.Errorf("header row lacks %q", want)
		}
	}
	if strings.Contains(body, "Managed by the identity provider") || strings.Contains(body, `action="/tf/users/sessions"`) || strings.Contains(body, `action="/tf/users/local/hdr-1"`) {
		t.Error("a proxy-header row must be display only")
	}
}

// Why (design v2, N14): an SSO user's rows store a 64-character subject, so a
// search box that only compared the stored text would never find "Alice". The
// search resolves display names to subjects, like it resolves device names.
func TestAuditSearchFindsSSOUserByName(t *testing.T) {
	st := &memPrincipals{}
	e := setupWith(t, func(d *Deps) { d.Principals = st })
	_ = st.UpsertPrincipal(context.Background(), store.PrincipalRecord{Subject: testSub, DisplayName: "Alice Example", Kind: "sso"})
	e.aud.events = []model.AuditEvent{
		{ID: 1, At: time.Now(), Actor: testSub, Action: "device.update", Target: "device:1", Result: "ok"},
		{ID: 2, At: time.Now(), Actor: otherSub, Action: "device.restart", Target: "device:1", Result: "ok"},
	}
	_, body := e.do(t, "GET", "/audit?since=all&q=alice", "admin", nil)
	if !strings.Contains(body, "device.update") || strings.Contains(body, "device.restart") {
		t.Fatalf("search by display name: want only the named user's row: %s", body[strings.Index(body, "<main"):])
	}
}

// Why: a subject nobody recorded (a deleted account) is "Unknown user" in the plain-text form too
// (policy history, "started by"), as on the audit page; "user 9f86d081" read like a name.
func TestTextOfUnrecordedSubjectIsUnknownUser(t *testing.T) {
	pr := newPrincipalDir(&memPrincipals{})
	if got := pr.Text("9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"); got != "Unknown user" {
		t.Errorf("got %q", got)
	}
}

// Why (QA M13): a header or SSO user known only by its subject hash must not print the hash
// as page text: the name falls back to the address, the hash moves to the tooltip.
func TestUsersViewNeverPrintsRawSubject(t *testing.T) {
	sub := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	d := auth.UsersPageData{Self: "local:admin", Users: []auth.UserRow{
		{Subject: sub, Name: sub, Method: auth.MethodHeader, Email: "pat@example.test"},
		{Subject: "hdr-2", Name: "Sam", Method: auth.MethodHeader},
	}}
	v := usersViewOf(d, time.Now(), newPrincipalDir(&memPrincipals{}).Text)
	for _, r := range v.Rows {
		if strings.Contains(r.Name, sub[:32]) || strings.Contains(r.Sub, sub[:32]) {
			t.Errorf("row %q prints the raw subject: %q", r.Name, r.Sub)
		}
	}
	if v.Rows[0].Name != "pat@example.test" || v.Rows[0].SubTitle != sub {
		t.Errorf("hash-only user: name %q, tooltip %q", v.Rows[0].Name, v.Rows[0].SubTitle)
	}
	if v.Rows[1].SubTitle != "" {
		t.Error("a readable subject needs no tooltip")
	}
}

// Why (#283): a name the person chose replaces the identity provider's name in the header and avatar,
// and is escaped like any other text (it is typed by a user and shown to admins).
func TestChosenDisplayNameShowsInTheHeaderEscaped(t *testing.T) {
	st := &memPrincipals{}
	e := setupWith(t, func(d *Deps) { d.Principals = st })
	chosen := `Dr. <b>Tester</b>`
	_ = st.UpsertPrincipal(context.Background(), store.PrincipalRecord{Subject: "test:admin", DisplayName: chosen, Provider: "Tester", Custom: chosen, Kind: "sso"})
	_, body := e.do(t, "GET", "/whats-new", "admin", nil)
	if !strings.Contains(body, `<div class="um-name" data-visual-mask>Dr. &lt;b&gt;Tester&lt;/b&gt;</div>`) {
		t.Fatalf("header does not show the chosen name, escaped: %s", body[strings.Index(body, "um-who")-40:][:300])
	}
	if strings.Contains(body, "<b>Tester</b>") {
		t.Fatal("the chosen name reached the page unescaped")
	}
}

// Why (#283): the audit trail names the actor by the stable subject. A chosen name is only the label
// shown for it (the subject stays in the tooltip and the row stays filterable by it), and a name
// that imitates a system actor or a local account is told apart by the subject suffix.
func TestAuditShowsTheChosenNameAsALabelForTheStableSubject(t *testing.T) {
	st := &memPrincipals{}
	e := setupWith(t, func(d *Deps) { d.Principals = st })
	ctx := context.Background()
	_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: testSub, DisplayName: "Thomas Kroll", Provider: "thomas", Custom: "Thomas Kroll", Kind: "sso"})
	_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: otherSub, DisplayName: "System", Provider: "mallory", Custom: "System", Kind: "sso"})
	e.aud.events = []model.AuditEvent{
		{ID: 1, At: time.Now(), Actor: testSub, Action: "device.update", Target: "device:1", Result: "ok"},
		{ID: 2, At: time.Now(), Actor: otherSub, Action: "device.restart", Target: "device:1", Result: "ok"},
	}
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	if !strings.Contains(body, `class="who" title="`+testSub) || !strings.Contains(body, "Thomas Kroll") {
		t.Errorf("the chosen name must label the row with the subject in the tooltip: %s", body[strings.Index(body, "<main"):])
	}
	if !strings.Contains(body, "System · "+subjectSuffix(otherSub)) {
		t.Errorf("a chosen name that imitates a system actor must carry the subject suffix")
	}
	_, body = e.do(t, "GET", "/audit?since=all&actor="+testSub, "admin", nil)
	if !strings.Contains(body, "device.update") || strings.Contains(body, "device.restart") {
		t.Errorf("the row stays selectable by the stable subject")
	}
}

// Why (#283): a chosen name must not let its owner pass as someone else in the audit trail. A name with
// anything but printable ASCII looks like another name without equalling it (Cyrillic A, fullwidth
// brackets), so it always carries the subject suffix; ASCII look-alikes that differ only in case or
// spacing are caught by the comparison itself.
func TestLookAlikeChosenNamesAlwaysCarryTheSubjectSuffix(t *testing.T) {
	st := &memPrincipals{}
	pr := newPrincipalDir(st)
	ctx := context.Background()
	_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: "local:alice", DisplayName: "alice", Kind: "local"})
	names := []string{"\u0410lice", "admin \uff08local\uff09", "S\u0443stem", "J\u00fcrgen", "ALICE", "Sys tem", "SYSTEM  "}
	subs := make([]string, len(names))
	for i, name := range names {
		subs[i] = strings.Repeat(string(rune('a'+i)), 40)
		_ = st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: subs[i], DisplayName: name, Provider: "p", Custom: name, Kind: "sso"})
	}
	for i, name := range names {
		if name == "Sys tem" {
			continue // a distinct name: nothing it could be mistaken for
		}
		if got := pr.Text(subs[i]); !strings.Contains(got, "\u00b7 "+subjectSuffix(subs[i])) {
			t.Errorf("%q shown without the subject suffix: %q", name, got)
		}
	}
}
