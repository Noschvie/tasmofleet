package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func anyIP(netip.Addr) bool { return true }

func idpServer(t *testing.T, issuerOverride string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		iss := srv.URL
		if issuerOverride != "" {
			iss = issuerOverride
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`,
			iss, srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/keys")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Why: the "test SSO" button must tell a working identity provider from a broken
// one without ever being a general-purpose fetcher.
func TestCheckIssuer(t *testing.T) {
	srv := idpServer(t, "")
	msg, err := CheckIssuer(context.Background(), srv.URL, anyIP)
	if err != nil || !strings.Contains(msg, "OK") {
		t.Fatalf("working provider: %q %v", msg, err)
	}
	bad := idpServer(t, "https://somebody-else.example")
	if _, err := CheckIssuer(context.Background(), bad.URL, anyIP); err == nil {
		t.Fatal("a discovery document with another issuer must fail")
	}
	if _, err := CheckIssuer(context.Background(), srv.URL, func(netip.Addr) bool { return false }); err == nil {
		t.Fatal("an address the caller refuses must not be contacted")
	}
	if _, err := CheckIssuer(context.Background(), "ftp://x", anyIP); err == nil {
		t.Fatal("only http(s) issuers")
	}
	if _, err := CheckIssuer(context.Background(), "http://user:pw@example.test/", anyIP); err == nil {
		t.Fatal("no credentials in the issuer URL")
	}
}

func TestCheckIssuerAnswersCoarselyAndDoesNotFollowRedirects(t *testing.T) {
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
	}))
	t.Cleanup(redir.Close)
	_, err := CheckIssuer(context.Background(), redir.URL, anyIP)
	if err == nil || strings.Contains(err.Error(), "169.254") {
		t.Fatalf("err = %v", err)
	}
}
