// Package oidcmock is a minimal OpenID Connect provider for tests and the
// e2e dev environment. It mimics Authentik's semantics: per-application
// issuer path (/application/o/<slug>/), discovery document, authorization
// code + PKCE (S256 enforced), RS256 ID tokens with a "groups" claim
// (list of group names), preferred_username, end_session_endpoint for
// RP-initiated logout, and refresh_token support. Users and groups are
// configured in code. Never imported by production packages.
package oidcmock

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// User is a mock account.
type User struct {
	Subject  string
	Username string
	Name     string // the "name" claim (full name); empty: the username
	Email    string
	Groups   []string
}

// Provider is a running mock IdP.
type Provider interface {
	Issuer() string
	Handler() http.Handler
	// NextLogin makes the next authorization request log in as u (no UI).
	NextLogin(u User)
}

// Options configure a mock provider.
type Options struct {
	Slug         string   // application slug; default "tasmofleet"
	ClientID     string   // default "tasmofleet"
	ClientSecret string   // default "mock-secret"
	RedirectURIs []string // registered redirect URIs; exact match like Authentik's strict mode
	// DefaultUser logs in when no NextLogin is queued (devenv). Without it
	// an authorization request with an empty queue is refused.
	DefaultUser *User
	// Now is the token clock; default time.Now.
	Now func() time.Time
}

// Server is a mock IdP on a loopback httptest server.
type Server struct {
	opts   Options
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	mu     sync.Mutex
	queue  []User
	codes  map[string]authCode
	tokens map[string]User // access and refresh tokens
	tamper func(claims map[string]any)
	ends   []url.Values
	auths  []url.Values // authorization requests, see AuthorizeRequests
}

type authCode struct {
	user        User
	clientID    string
	redirectURI string
	nonce       string
	challenge   string
	authTime    time.Time
	expires     time.Time
}

var _ Provider = (*Server)(nil)

// Start starts a provider on 127.0.0.1. Close it with Close.
func Start(opts Options) (*Server, error) {
	if opts.Slug == "" {
		opts.Slug = "tasmofleet"
	}
	if opts.ClientID == "" {
		opts.ClientID = "tasmofleet"
	}
	if opts.ClientSecret == "" {
		opts.ClientSecret = "mock-secret"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, key: key, kid: "mock-1", codes: map[string]authCode{}, tokens: map[string]User{}}
	s.srv = httptest.NewServer(s.Handler())
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// URL is the server root, e.g. http://127.0.0.1:41234.
func (s *Server) URL() string { return s.srv.URL }

// Client returns an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Issuer returns the Authentik-style issuer URL (with trailing slash).
func (s *Server) Issuer() string { return s.srv.URL + "/application/o/" + s.opts.Slug + "/" }

// NextLogin queues u for the next authorization request.
func (s *Server) NextLogin(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, u)
}

// TamperNextIDToken lets a test modify the claims of the next ID token the
// token endpoint issues (wrong nonce, audience, expiry, ...).
func (s *Server) TamperNextIDToken(f func(claims map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tamper = f
}

// AuthorizeRequests returns the query parameters of every authorization
// request (prompt, max_age, ... for step-up tests).
func (s *Server) AuthorizeRequests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths)
}

// EndSessionCalls returns the query parameters of every end-session request.
func (s *Server) EndSessionCalls() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ends)
}

// Handler serves the Authentik-like endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	app := "/application/o/" + s.opts.Slug + "/"
	mux.HandleFunc("GET "+app+".well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET "+app+"jwks/", s.jwks)
	mux.HandleFunc("GET "+app+"end-session/", s.endSession)
	mux.HandleFunc("GET /application/o/authorize/", s.authorize)
	mux.HandleFunc("POST /application/o/token/", s.token)
	mux.HandleFunc("GET /application/o/userinfo/", s.userinfo)
	return mux
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	base := s.srv.URL + "/application/o/"
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.Issuer(),
		"authorization_endpoint":                base + "authorize/",
		"token_endpoint":                        base + "token/",
		"userinfo_endpoint":                     base + "userinfo/",
		"end_session_endpoint":                  s.Issuer() + "end-session/",
		"jwks_uri":                              s.Issuer() + "jwks/",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	})
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid,
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.ends = append(s.ends, q)
	s.mu.Unlock()
	if u := q.Get("post_logout_redirect_uri"); u != "" {
		http.Redirect(w, r, u, http.StatusFound) //nolint:gosec // G710: test IdP, mirrors Authentik's end-session redirect
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != s.opts.ClientID || !slices.Contains(s.opts.RedirectURIs, redirect) {
		// Never redirect to an unregistered URI (that would be an open redirect).
		http.Error(w, "invalid client_id or redirect_uri", http.StatusBadRequest)
		return
	}
	fail := func(code string) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("error", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // G710: redirect_uri was checked against RedirectURIs
	}
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type")
		return
	case !slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		fail("invalid_scope")
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		fail("invalid_request") // PKCE S256 is mandatory
		return
	}
	s.mu.Lock()
	var user User
	switch {
	case len(s.queue) > 0:
		user, s.queue = s.queue[0], s.queue[1:]
	case s.opts.DefaultUser != nil:
		user = *s.opts.DefaultUser
	default:
		s.mu.Unlock()
		fail("access_denied")
		return
	}
	s.auths = append(s.auths, q)
	code := randString()
	s.codes[code] = authCode{authTime: s.opts.Now(),
		user: user, clientID: s.opts.ClientID, redirectURI: redirect, nonce: q.Get("nonce"),
		challenge: q.Get("code_challenge"), expires: s.opts.Now().Add(time.Minute),
	}
	s.mu.Unlock()
	u, _ := url.Parse(redirect)
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // G710: redirect_uri was checked against RedirectURIs
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.opts.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(s.opts.ClientSecret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		c, found := s.codes[r.PostForm.Get("code")]
		delete(s.codes, r.PostForm.Get("code")) // codes are single use
		s.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		switch {
		case !found || s.opts.Now().After(c.expires) || c.redirectURI != r.PostForm.Get("redirect_uri"):
			tokenError(w, "invalid_grant")
		case b64(sum[:]) != c.challenge:
			tokenError(w, "invalid_grant") // PKCE verifier mismatch
		default:
			s.issue(w, c.user, c.nonce, c.authTime)
		}
	case "refresh_token":
		s.mu.Lock()
		u, found := s.tokens["r:"+r.PostForm.Get("refresh_token")]
		s.mu.Unlock()
		if !found {
			tokenError(w, "invalid_grant")
			return
		}
		s.issue(w, u, "", time.Time{})
	default:
		tokenError(w, "unsupported_grant_type")
	}
}

// issue signs the tokens. auth_time (OIDC Core 2.) is the moment the user
// authenticated at authorize time; a refresh keeps no new authentication, so
// it carries none. Tests drop or change it with TamperNextIDToken.
func (s *Server) issue(w http.ResponseWriter, u User, nonce string, authTime time.Time) {
	now := s.opts.Now()
	claims := map[string]any{
		"iss": s.Issuer(), "sub": u.Subject, "aud": s.opts.ClientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"preferred_username": u.Username, "email": u.Email, "name": u.fullName(),
		"groups": append([]string{}, u.Groups...),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if !authTime.IsZero() {
		claims["auth_time"] = authTime.Unix()
	}
	access, refresh := randString(), randString()
	s.mu.Lock()
	if s.tamper != nil {
		s.tamper(claims)
		s.tamper = nil
	}
	s.tokens["a:"+access] = u
	s.tokens["r:"+refresh] = u
	s.mu.Unlock()
	idToken, err := s.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300,
		"refresh_token": refresh, "id_token": idToken,
	})
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	u, found := s.tokens["a:"+tok]
	s.mu.Unlock()
	if !ok || !found {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sub": u.Subject, "preferred_username": u.Username, "email": u.Email, "groups": u.Groups,
	})
}

// sign returns a compact RS256 JWS.
func (s *Server) sign(claims map[string]any) (string, error) {
	h, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.Join(errors.New("oidcmock: sign"), err)
	}
	return input + "." + b64(sig), nil
}

func tokenError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a failed write to a test client has no one to report to
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randString() string { return rand.Text() }

func (u User) fullName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.Username
}
