package auth

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

// Why (#380): "local:<name>" is the subject of a local account, and the audit log, the Users page and
// the principals table decide "local account" from that prefix alone. An identity provider that lets a
// user pick the username or sub "local:admin" must therefore not get a session, or that person would
// appear as, and share sessions and audit rows with, the local admin.
func TestOIDCSubjectOfALocalAccountIsRefused(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	r := b.get(b.startOIDC("/", oidcmock.User{Subject: "local:admin", Username: "admin", Groups: []string{"fleet-admins"}}))
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "reserved") {
		t.Fatalf("callback: %d %s, want a 403 saying the identity is reserved", r.code, r.body)
	}
	if n := len(e.sessions.all()); n != 0 {
		t.Errorf("%d sessions created for a reserved subject", n)
	}
	if _, ok := e.dir.rows["local:admin"]; ok {
		t.Error("the sign-in overwrote the principal row of the local admin")
	}
	if a := e.audit.last(); a.Action != "auth.login.oidc" || a.Actor != "local:admin" || !strings.HasPrefix(a.Result, "denied: reserved") {
		t.Errorf("audit %+v", a)
	}
}

// Why (#380): the same through the trusted proxy's user header, where the name is taken as sent.
func TestHeaderUserOfALocalAccountIsRefused(t *testing.T) {
	e := newEnv(t, ModeHeader)
	hdr := map[string]string{"X-authentik-username": "local:admin", "X-authentik-groups": "fleet-admins"}
	r := e.browser().do(http.MethodGet, "/account", nil, hdr)
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "reserved") {
		t.Fatalf("GET /account: %d %s, want 403", r.code, r.body)
	}
	if a := e.audit.last(); a.Action != "auth.login.header" || a.Actor != "local:admin" || !strings.HasPrefix(a.Result, "denied: reserved") {
		t.Errorf("audit %+v", a)
	}
	// a repeated request must not flood the audit log
	n := len(e.audit.ev)
	e.browser().do(http.MethodGet, "/account", nil, hdr)
	if len(e.audit.ev) != n {
		t.Error("every refused request wrote an audit row")
	}
	ok := map[string]string{"X-authentik-username": "boss", "X-authentik-groups": "fleet-admins"}
	if r := e.browser().do(http.MethodGet, "/account", nil, ok); r.code != http.StatusOK {
		t.Errorf("an ordinary header user: %d", r.code)
	}
}
