package auth

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// standalone is the whole standalone stack with real parts only: SQLite,
// the key file, a self-signed HTTPS listener, no proxy and no IdP.
type standalone struct {
	t   *testing.T
	dir string
	db  *store.SQLite
	svc *Service
	srv *httptest.Server
}

func startStandalone(t *testing.T, dir string) *standalone {
	t.Helper()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // temp DB
	key, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{Mode: ModeLocal, Sessions: db, LocalUsers: db, Audit: &fakeAudit{}, SecretKey: key, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, _, err := httpx.TLSConfig(httpx.TLSOptions{SelfSigned: true, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := httpx.NewCSRF(svc)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, p := range svc.MountPaths() {
		mux.Handle(p, svc.Routes())
	}
	mux.Handle("GET /{$}", svc.Require(model.RoleOperator)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		_, _ = io.WriteString(w, "hello "+p.Name+" "+roleName(p.Role))
	})))
	proxy := httpx.Proxy{} // no PUBLIC_URL, no trusted proxy
	srv := httptest.NewUnstartedServer(proxy.Middleware(svc.Middleware(csrf.Middleware(mux))))
	// httptest's StartTLS would add its own certificate; serve ours.
	srv.Listener = tls.NewListener(srv.Listener, tlsCfg)
	srv.Start()
	srv.URL = strings.Replace(srv.URL, "http://", "https://", 1)
	t.Cleanup(srv.Close)
	return &standalone{t: t, dir: dir, db: db, svc: svc, srv: srv}
}

// client trusts only the persisted self-signed certificate, like a user who
// accepted it, and keeps cookies.
func (s *standalone) client(jar http.CookieJar) *http.Client {
	pem, err := os.ReadFile(filepath.Join(s.dir, "tls", "cert.pem"))
	if err != nil {
		s.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		s.t.Fatal("bad certificate")
	}
	return &http.Client{
		Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (s *standalone) do(c *http.Client, method, path string, form url.Values) (code int, loc, body string) {
	s.t.Helper()
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.srv.URL+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func (s *standalone) cookie(jar http.CookieJar, name string) *http.Cookie {
	u, _ := url.Parse(s.srv.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Why: this is the promise of standalone mode: install, start, sign in over
// HTTPS with the generated password, be forced to change it, and keep
// working after a restart, with no proxy, no IdP and no PUBLIC_URL.
func TestStandaloneEndToEnd(t *testing.T) {
	dir := t.TempDir()
	s := startStandalone(t, dir)
	created, err := s.svc.EnsureInitialAdmin(t.Context(), "")
	if err != nil || !created {
		t.Fatalf("first run: %v, %v", created, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, InitialPasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	generated := strings.TrimSpace(string(raw))

	jar, _ := cookiejar.New(nil)
	c := s.client(jar)
	if code, loc, _ := s.do(c, http.MethodGet, "/", nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("anonymous /: %d %q", code, loc)
	}
	if code, _, _ := s.do(c, http.MethodGet, "/login", nil); code != http.StatusOK {
		t.Fatal(code)
	}
	pre := s.cookie(jar, CookieSecure+"-csrf")
	if pre == nil {
		t.Fatal("over HTTPS the pre-session cookie must be the __Host- one")
	}
	if code, _, _ := s.do(c, http.MethodPost, "/login/local", url.Values{"username": {"admin"}, "password": {"wrong password 123"}, "csrf": {pre.Value}}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	code, loc, _ := s.do(c, http.MethodPost, "/login/local", url.Values{"username": {"admin"}, "password": {generated}, "csrf": {s.cookie(jar, CookieSecure+"-csrf").Value}})
	if code != http.StatusSeeOther || loc != "/account" {
		t.Fatalf("login: %d %q", code, loc)
	}
	if s.cookie(jar, CookieSecure) == nil {
		t.Fatal("no __Host-tasmofleet session cookie after login")
	}
	if code, loc, _ := s.do(c, http.MethodGet, "/", nil); code != http.StatusSeeOther || loc != "/account" {
		t.Fatalf("/ before the change: %d %q", code, loc)
	}
	sess, err := s.db.GetSession(t.Context(), hashID(s.cookie(jar, CookieSecure).Value))
	if err != nil {
		t.Fatal(err)
	}
	const mine = "my own long passphrase"
	code, loc, body := s.do(c, http.MethodPost, "/account/password", url.Values{"current": {generated}, "new": {mine}, "confirm": {mine}, "csrf": {sess.CSRFToken}})
	if code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("change: %d %q %s", code, loc, body)
	}
	if _, err := os.Stat(filepath.Join(dir, InitialPasswordFile)); !os.IsNotExist(err) {
		t.Errorf("password file left behind: %v", err)
	}
	if code, _, body := s.do(c, http.MethodGet, "/", nil); code != http.StatusOK || body != "hello admin admin" {
		t.Fatalf("/ after the change: %d %q", code, body)
	}
	certBefore, err := os.ReadFile(filepath.Join(dir, "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}

	// Restart: new process state, same data directory.
	s.srv.Close()
	s2 := startStandalone(t, dir)
	certAfter, _ := os.ReadFile(filepath.Join(dir, "tls", "cert.pem"))
	if string(certBefore) != string(certAfter) {
		t.Error("the self-signed certificate changed across the restart")
	}
	if created, err := s2.svc.EnsureInitialAdmin(t.Context(), ""); err != nil || created {
		t.Fatalf("restart created an account: %v, %v", created, err)
	}
	// The browser's cookies still work: sessions are in the database, the
	// device cookie's key is persisted. The jar belongs to the old origin, so
	// carry the cookie values over to the new test server's URL.
	jar2, _ := cookiejar.New(nil)
	u2, _ := url.Parse(s2.srv.URL)
	u1, _ := url.Parse(s.srv.URL)
	jar2.SetCookies(u2, jar.Cookies(u1))
	c2 := s2.client(jar2)
	if code, _, body := s2.do(c2, http.MethodGet, "/", nil); code != http.StatusOK || body != "hello admin admin" {
		t.Fatalf("session after restart: %d %q", code, body)
	}
	dev := s2.cookie(jar2, CookieSecure+"-device")
	if dev == nil {
		t.Fatal("no device cookie")
	}
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieInsecure + "-device", Value: dev.Value})
	if !s2.svc.validDeviceCookie(r, "admin") {
		t.Error("the device cookie was not valid after the restart (key not persisted?)")
	}
}
