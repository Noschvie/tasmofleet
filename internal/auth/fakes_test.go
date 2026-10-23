package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func (c *fakeClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type fakeSessions struct {
	mu    sync.Mutex
	m     map[string]store.Session
	users *fakeUsers // if set, local sessions need a current password generation (like the real store)
}

func (f *fakeSessions) CreateSession(_ context.Context, s store.Session) error {
	if f.users != nil && s.Principal.Method == "local" && !f.users.current(s.Principal.Name, s.PasswordGen) {
		return store.ErrStaleActor
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[s.IDHash] = s
	return nil
}
func (f *fakeSessions) GetSession(_ context.Context, id string) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[id]
	if !ok {
		return s, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeSessions) TouchSession(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[id]
	if !ok {
		return store.ErrNotFound
	}
	s.LastSeen = at
	f.m[id] = s
	return nil
}
func (f *fakeSessions) DeleteSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	return nil
}
func (f *fakeSessions) UpdateSessionRecheck(_ context.Context, u store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[u.IDHash]
	if !ok {
		return store.ErrNotFound
	}
	s.Principal, s.IDToken, s.RefreshToken, s.RecheckedAt = u.Principal, u.IDToken, u.RefreshToken, u.RecheckedAt
	f.m[u.IDHash] = s
	return nil
}
func (f *fakeSessions) DeleteUserSessions(_ context.Context, subject string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, s := range f.m {
		if s.Principal.Subject == subject {
			delete(f.m, k)
			n++
		}
	}
	return n, nil
}
func (f *fakeSessions) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, s := range f.m {
		if !now.Before(s.ExpiresAt) {
			delete(f.m, k)
			n++
		}
	}
	return n, nil
}
func (f *fakeSessions) all() []store.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, s := range f.m {
		out = append(out, s)
	}
	return out
}

// fakeDir is an in-memory store.UserDirectory; CountSessions reads the fake
// session store, like the real one reads the sessions table.
type fakeDir struct {
	mu       sync.Mutex
	rows     map[string]store.PrincipalRecord
	sessions *fakeSessions
}

func (d *fakeDir) RecordLogin(_ context.Context, p store.PrincipalRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p.LastLogin = p.UpdatedAt
	d.rows[p.Subject] = p
	return nil
}
func (d *fakeDir) ListPrincipals(context.Context) ([]store.PrincipalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []store.PrincipalRecord
	for _, r := range d.rows {
		out = append(out, r)
	}
	return out, nil
}
func (d *fakeDir) CountSessions(_ context.Context, now time.Time, idle time.Duration) (map[string]int, error) {
	out := map[string]int{}
	for _, s := range d.sessions.all() {
		if now.Before(s.ExpiresAt) && s.LastSeen.After(now.Add(-idle)) {
			out[s.Principal.Subject]++
		}
	}
	return out, nil
}

// fakeUsers is an in-memory store.LocalUserStore.
type fakeUsers struct {
	t     *testing.T
	mu    sync.Mutex
	users map[string]store.LocalUser
	reads int
}

// current reports whether name is enabled and at generation gen.
func (f *fakeUsers) current(name string, gen int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	return ok && !u.Disabled && u.PasswordGen == gen
}

func (f *fakeUsers) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// cli simulates the CLI changing the accounts out of band.
func (f *fakeUsers) cli(change func(users map[string]store.LocalUser)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.users)
}

func (f *fakeUsers) GetLocalUser(_ context.Context, name string) (store.LocalUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	u, ok := f.users[name]
	if !ok {
		return u, store.ErrNotFound
	}
	return u, nil
}
func (f *fakeUsers) ListLocalUsers(context.Context) ([]store.LocalUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.LocalUser
	for _, u := range f.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b store.LocalUser) int { return strings.Compare(a.Username, b.Username) })
	return out, nil
}
func (f *fakeUsers) HasLocalUsers(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users) > 0, nil
}
func (f *fakeUsers) AddLocalUser(_ context.Context, u store.LocalUser, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	if u.Role != model.RoleOperator && u.Role != model.RoleAdmin {
		f.t.Errorf("AddLocalUser(%q) with invalid role %d", u.Username, u.Role)
	}
	if _, ok := f.users[u.Username]; ok {
		return store.ErrConflict
	}
	u.PasswordGen = 1
	f.users[u.Username] = u
	return nil
}

func (f *fakeUsers) SetLocalPassword(_ context.Context, name, hash string, at time.Time, must bool, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash, u.UpdatedAt, u.MustChangePassword, u.OTPExpiresAt = hash, at, must, time.Time{}
	u.PasswordGen++
	u.InitialSetup = false
	f.users[name] = u
	return nil
}

// actorOK mirrors the store: the zero actor is the host CLI; otherwise the
// actor must be an enabled admin at the stated generation. f.mu is held.
func (f *fakeUsers) actorOK(by store.Actor) bool {
	if by.Username == "" {
		return true
	}
	u, ok := f.users[by.Username]
	return ok && u.Role == model.RoleAdmin && !u.Disabled && u.PasswordGen == by.PasswordGen
}

func (f *fakeUsers) ChangeOwnPassword(_ context.Context, name string, expectGen int64, hash string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok || u.Disabled || u.PasswordGen != expectGen {
		return store.ErrStaleActor
	}
	u.PasswordHash, u.UpdatedAt, u.MustChangePassword, u.OTPExpiresAt = hash, at, false, time.Time{}
	u.PasswordGen++
	u.InitialSetup = false
	f.users[name] = u
	return nil
}

// otherAdmin mirrors the store's last-admin rule.
func (f *fakeUsers) otherAdmin(except string) bool {
	for n, u := range f.users {
		if n != except && u.Role == model.RoleAdmin && !u.Disabled {
			return true
		}
	}
	return false
}

func (f *fakeUsers) SetLocalRole(_ context.Context, name string, role model.Role, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	if u.Role == model.RoleAdmin && !u.Disabled && role != model.RoleAdmin && !f.otherAdmin(name) {
		return store.ErrLastAdmin
	}
	u.Role = role
	f.users[name] = u
	return nil
}

func (f *fakeUsers) SetLocalDisabled(_ context.Context, name string, disabled bool, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	if disabled {
		if u.Role == model.RoleAdmin && !u.Disabled && !f.otherAdmin(name) {
			return store.ErrLastAdmin
		}
		u.PasswordGen++
		u.InitialSetup = false
	}
	u.Disabled = disabled
	f.users[name] = u
	return nil
}

func (f *fakeUsers) DeleteLocalUser(_ context.Context, name string, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	if u.Role == model.RoleAdmin && !u.Disabled && !f.otherAdmin(name) {
		return store.ErrLastAdmin
	}
	delete(f.users, name)
	return nil
}

// fakeCookies is an in-memory store.DeviceCookieStore.
type fakeCookies struct {
	mu sync.Mutex
	m  map[string]string // id hash -> username
	g  map[string]int64  // id hash -> password generation

	users *fakeUsers // if set, cookies need a current password generation (like the real store)
}

func (f *fakeCookies) PutDeviceCookie(_ context.Context, id, user string, _ time.Time, gen int64) error {
	if f.users != nil && !f.users.current(user, gen) {
		return store.ErrStaleActor
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = user
	if f.g == nil {
		f.g = map[string]int64{}
	}
	f.g[id] = gen
	return nil
}
func (f *fakeCookies) HasDeviceCookie(_ context.Context, id, user string, gen int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[id] == user && user != "" && f.g[id] == gen, nil
}
func (f *fakeCookies) DeleteDeviceCookie(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	return nil
}
func (f *fakeCookies) DeleteUserDeviceCookies(_ context.Context, user string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, u := range f.m {
		if u == user {
			delete(f.m, k)
			n++
		}
	}
	return n, nil
}
func (f *fakeCookies) DeleteDeviceCookiesBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeCookies) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

type fakeAudit struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *fakeAudit) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e)
}
func (a *fakeAudit) last() model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ev) == 0 {
		return model.AuditEvent{}
	}
	return a.ev[len(a.ev)-1]
}

// Test password; obviously fake.
const testPassword = "correct horse battery"

var testHash = func() string {
	h, err := HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
}()

// env is a running app (auth stack + two protected routes) and, in OIDC
// mode, a mock IdP.
type env struct {
	t        *testing.T
	srv      *httptest.Server
	idp      *oidcmock.Server
	svc      *Service
	sessions *fakeSessions
	audit    *fakeAudit
	clock    *fakeClock
	proxy    httpx.Proxy
	users    *fakeUsers
	cookies  *fakeCookies
	dir      *fakeDir
}

type envOpt func(*Options)

func newEnv(t *testing.T, mode string, opts ...envOpt) *env {
	t.Helper()
	e := &env{t: t, sessions: &fakeSessions{m: map[string]store.Session{}}, audit: &fakeAudit{},
		clock: &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}}
	e.users = &fakeUsers{t: t, users: map[string]store.LocalUser{"admin": {Username: "admin", PasswordHash: testHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleAdmin}}}
	e.cookies = &fakeCookies{m: map[string]string{}, users: e.users}
	e.sessions.users = e.users
	e.dir = &fakeDir{rows: map[string]store.PrincipalRecord{}, sessions: e.sessions}
	var h http.Handler
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	e.proxy = httpx.Proxy{PublicURL: e.srv.URL, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	o := Options{
		Mode: mode, Proxy: e.proxy, LocalLogin: true,
		Roles:    RoleMapping{AdminGroups: []string{"fleet-admins"}, OperatorGroups: []string{"fleet-ops"}},
		Sessions: e.sessions, Audit: e.audit, Clock: e.clock,
		LocalUsers: e.users, DeviceCookies: e.cookies, Directory: e.dir,
		SecretKey: []byte("test-only-secret-key-0123456789abcdef"),
		Header:    HeaderOptions{LogoutURL: "https://auth.example/outpost.goauthentik.io/sign_out"},
	}
	if mode == ModeOIDC {
		idp, err := oidcmock.Start(oidcmock.Options{RedirectURIs: []string{e.srv.URL + "/oidc/callback"}, Now: e.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(idp.Close)
		e.idp = idp
		o.OIDC = OIDCOptions{Issuer: idp.Issuer(), ClientID: "tasmofleet", ClientSecret: "mock-secret", HTTPClient: idp.Client()}
	}
	for _, f := range opts {
		f(&o)
	}
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	if mode == ModeOIDC {
		if err := svc.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	h = e.handler()
	return e
}

// flowName is the OIDC flow cookie name for this env's (plain HTTP) requests.
func (e *env) flowName() string {
	return e.svc.flowCookie(httptest.NewRequest(http.MethodGet, "/", nil))
}

func (e *env) handler() http.Handler {
	e.proxy = e.svc.o.Proxy // envOpts may change the proxy settings
	var origins []string
	if e.proxy.PublicURL != "" {
		origins = append(origins, e.proxy.PublicURL)
	}
	csrf, err := httpx.NewCSRF(e.svc, origins...)
	if err != nil {
		e.t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		_, _ = io.WriteString(w, "ok "+p.Subject) // test response
	})
	mux := http.NewServeMux()
	routes := e.svc.Routes()
	for _, p := range e.svc.MountPaths() {
		mux.Handle(p, routes)
	}
	mux.Handle("GET /viewer", e.svc.Require(model.RoleOperator)(ok))
	mux.Handle("POST /admin-action", e.svc.Require(model.RoleAdmin)(ok))
	mux.Handle("POST /stepped", e.svc.Require(model.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pw := r.PostFormValue("current"); pw != "" {
			okPw, limited := e.svc.StepUp(r, pw)
			_, _ = fmt.Fprintf(w, "stepup ok=%v limited=%v ", okPw, limited) // test response
		}
		_, _ = fmt.Fprintf(w, "stepped=%v", e.svc.SteppedUp(r)) // test response
	})))
	return e.proxy.Middleware(e.svc.Middleware(csrf.Middleware(mux)))
}

// browser is a cookie-keeping client that does not follow redirects.
type browser struct {
	t *testing.T
	c *http.Client
	e *env
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: e.t, e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

type resp struct {
	code int
	loc  string
	body string
	hdr  http.Header
}

func (b *browser) do(method, rawURL string, form url.Values, hdr map[string]string) resp {
	b.t.Helper()
	if strings.HasPrefix(rawURL, "/") {
		rawURL = b.e.srv.URL + rawURL
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(b.t.Context(), method, rawURL, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	data, err := io.ReadAll(r.Body)
	if cerr := r.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return resp{code: r.StatusCode, loc: r.Header.Get("Location"), body: string(data), hdr: r.Header}
}

func (b *browser) get(u string) resp { return b.do(http.MethodGet, u, nil, nil) }

func (b *browser) cookie(name string) string {
	u, _ := url.Parse(b.e.srv.URL + "/")
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (b *browser) setCookie(name, value string) {
	u, _ := url.Parse(b.e.srv.URL + "/")
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/"}})
}

// oidcLogin runs the code flow up to the callback and returns its URL.
func (b *browser) startOIDC(next string, u oidcmock.User) (callback string) {
	b.t.Helper()
	b.e.idp.NextLogin(u)
	r := b.get("/login/sso?next=" + url.QueryEscape(next))
	if r.code != http.StatusFound {
		b.t.Fatalf("/login/sso: %d %s", r.code, r.body)
	}
	r = b.get(r.loc)
	if r.code != http.StatusFound || !strings.HasPrefix(r.loc, b.e.srv.URL+"/oidc/callback") {
		b.t.Fatalf("authorize: %d %s", r.code, r.loc)
	}
	return r.loc
}

func (b *browser) csrfToken() string {
	b.t.Helper()
	s := b.e.sessions.all()
	if len(s) != 1 {
		b.t.Fatalf("want 1 session, have %d", len(s))
	}
	return s[0].CSRFToken
}

// peekGrant returns the OAuth grant_type of a token request without
// consuming its body.
func peekGrant(r *http.Request) string {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	return v.Get("grant_type")
}

func (f *fakeUsers) SetLocalOneTimePassword(_ context.Context, name, hash string, at, expires time.Time, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash, u.UpdatedAt, u.MustChangePassword, u.OTPExpiresAt = hash, at, true, expires
	u.PasswordGen++
	u.InitialSetup = false
	f.users[name] = u
	return nil
}

func (f *fakeUsers) SetLocalProfile(_ context.Context, name, display, email string, by store.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.actorOK(by) {
		return store.ErrStaleActor
	}
	u, ok := f.users[name]
	if !ok {
		return store.ErrNotFound
	}
	u.DisplayName, u.Email = display, email
	f.users[name] = u
	return nil
}

func (f *fakeUsers) SetOwnProfile(_ context.Context, name string, expectGen int64, display, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok || u.Disabled || u.PasswordGen != expectGen {
		return store.ErrStaleActor
	}
	u.DisplayName, u.Email = display, email
	f.users[name] = u
	return nil
}

func (d *fakeDir) ListSubjectSessions(_ context.Context, subject string, now time.Time, idle time.Duration) ([]store.Session, error) {
	var out []store.Session
	for _, s := range d.sessions.all() {
		if s.Principal.Subject == subject && now.Before(s.ExpiresAt) && s.LastSeen.After(now.Add(-idle)) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b store.Session) int { return b.LastSeen.Compare(a.LastSeen) })
	return out, nil
}

func (d *fakeDir) DeleteSubjectSession(_ context.Context, subject, id string) (bool, error) {
	d.sessions.mu.Lock()
	defer d.sessions.mu.Unlock()
	s, ok := d.sessions.m[id]
	if !ok || s.Principal.Subject != subject {
		return false, nil
	}
	delete(d.sessions.m, id)
	return true, nil
}
