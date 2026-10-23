package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// postLocal submits the break-glass form as a client at addr (the test
// server sees loopback, so the client address comes from the trusted
// proxy's X-Forwarded-For).
func postLocal(b *browser, addr, user, pass string) resp {
	b.t.Helper()
	// A returning admin: the old session is gone, the device cookie stays.
	u, _ := url.Parse(b.e.srv.URL + "/")
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: CookieInsecure, Path: "/", MaxAge: -1}})
	b.do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-For": addr})
	return b.do(http.MethodPost, "/login/local", url.Values{
		"csrf": {b.cookie(CookieInsecure + "-csrf")}, "username": {user}, "password": {pass}, "next": {"/"},
	}, map[string]string{"X-Forwarded-For": addr})
}

// A third party hammering the admin account must not lock the real admin
// out: break-glass is the way in when the IdP is down.
func TestAttackerCannotLockOutBreakGlassAdmin(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	attacker := e.browser()
	for i := range 5 {
		postLocal(attacker, "198.51.100.7", "admin", "wrong guess "+string(rune('a'+i)))
	}
	// The attacker keeps trying every minute for half an hour; the
	// exponential delay leaves it only a handful of further attempts.
	admitted := 0
	for range 30 {
		e.clock.Advance(time.Minute)
		if r := postLocal(attacker, "198.51.100.7", "admin", "another guess"); r.code == http.StatusUnauthorized {
			admitted++
		}
	}
	if admitted > 5 {
		t.Fatalf("attacker admitted %d times in 30 min after the burst", admitted)
	}
	if r := postLocal(e.browser(), "203.0.113.20", "admin", testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("real admin from another address locked out: %d %s", r.code, r.body)
	}
}

// Rotating addresses inside one IPv6 /64 (one subscriber) does not reset the limit.
func TestRateLimitGroupsIPv6By64(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	for i := range 5 {
		postLocal(b, "2001:db8:1:2::"+string(rune('a'+i)), "admin", "wrong password!")
	}
	if r := postLocal(b, "2001:db8:1:2::ff", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("address rotation inside the /64 bypassed the limit: %d", r.code)
	}
	if r := postLocal(e.browser(), "2001:db8:1:3::1", "admin", testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("neighbouring /64 limited: %d", r.code)
	}
}

func TestLimitKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":           "192.0.2.1",
		"::ffff:192.0.2.1":    "192.0.2.1",
		"2001:db8::1":         "2001:db8::/64",
		"2001:db8:0:0:ffff::": "2001:db8::/64",
	} {
		if got := limitKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("limitKey(%s) = %s, want %s", in, got, want)
		}
	}
}

// A login flood must queue for the bounded KDF slots, and a client that
// gives up frees its place instead of running the KDF.
func TestKDFConcurrencyBounded(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	for range kdfSlots {
		e.svc.kdf <- struct{}{} // all slots busy
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := e.svc.verifyBounded(ctx, testHash, testPassword); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("KDF ran beyond the slot limit: %v", err)
	}
	<-e.svc.kdf
	if err := e.svc.verifyBounded(t.Context(), testHash, testPassword); err != nil {
		t.Fatalf("free slot not used: %v", err)
	}
	if len(e.svc.kdf) != kdfSlots-1 {
		t.Fatalf("slot not released: %d busy", len(e.svc.kdf))
	}
}

// Changing or deleting the break-glass account (CLI) or disabling local
// login must end existing local sessions at once, not after 7 days.
func TestLocalSessionRevokedByCredentialChanges(t *testing.T) {
	cases := map[string]func(e *env){
		"password changed": func(e *env) {
			e.users.cli(func(m map[string]store.LocalUser) {
				m["admin"] = store.LocalUser{Username: "admin", PasswordHash: testHash, UpdatedAt: e.clock.Now()}
			})
		},
		"account deleted": func(e *env) {
			e.users.cli(func(m map[string]store.LocalUser) { delete(m, "admin") })
		},
		"local login disabled": func(e *env) { e.svc.o.LocalLogin = false },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, ModeOIDC)
			b := e.browser()
			if r := localLogin(b, "admin", testPassword, "/"); r.code != http.StatusSeeOther {
				t.Fatalf("login %d", r.code)
			}
			if r := b.get("/viewer"); r.code != http.StatusOK {
				t.Fatalf("before: %d", r.code)
			}
			e.clock.Advance(time.Second)
			change(e)
			if r := b.get("/viewer"); r.code != http.StatusSeeOther {
				t.Fatalf("session survived %s: %d", name, r.code)
			}
			if len(e.sessions.all()) != 0 {
				t.Fatal("revoked session not deleted")
			}
		})
	}
}

// Removing a group from ADMIN_GROUPS (config change and restart) must
// apply to existing sessions, not only to new logins.
func TestOIDCSessionRoleFollowsCurrentMapping(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", alice)) // admin via fleet-admins
	tok := b.csrfToken()
	if r := b.do(http.MethodPost, "/admin-action", url.Values{"csrf": {tok}}, nil); r.code != http.StatusOK {
		t.Fatalf("admin before: %d", r.code)
	}
	e.svc.o.Roles = RoleMapping{OperatorGroups: []string{"fleet-admins"}}
	if r := b.do(http.MethodPost, "/admin-action", url.Values{"csrf": {tok}}, nil); r.code != http.StatusForbidden {
		t.Fatalf("stale admin role used: %d", r.code)
	}
	e.svc.o.Roles = RoleMapping{AdminGroups: []string{"other"}}
	if r := b.get("/viewer"); r.code != http.StatusSeeOther || len(e.sessions.all()) != 0 {
		t.Fatalf("session without any mapped group kept: %d", r.code)
	}
}

// After switching AUTH_MODE to header, old OIDC sessions must not bypass
// the proxy's authentication; local break-glass sessions stay valid.
func TestOIDCSessionRejectedInHeaderMode(t *testing.T) {
	e := newEnv(t, ModeHeader)
	sess := store.Session{
		IDHash: hashID("raw-cookie"), CSRFToken: "t", CreatedAt: e.clock.Now(), LastSeen: e.clock.Now(),
		ExpiresAt: e.clock.Now().Add(time.Hour),
		Principal: model.Principal{Subject: "sub-alice", Groups: []string{"fleet-admins"}, Role: model.RoleAdmin, Method: ModeOIDC},
	}
	if err := e.sessions.CreateSession(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieInsecure, Value: "raw-cookie"})
	if _, err := e.svc.Principal(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("OIDC session accepted in header mode: %v", err)
	}
	b := e.browser()
	if r := localLogin(b, "admin", testPassword, "/"); r.code != http.StatusSeeOther {
		t.Fatalf("break-glass login in header mode: %d", r.code)
	}
	if r := b.get("/viewer"); r.code != http.StatusOK || !strings.Contains(r.body, "local:admin") {
		t.Fatalf("local session in header mode: %d %s", r.code, r.body)
	}
}

func TestRevokeAllEndsEverySession(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", alice))
	localLogin(e.browser(), "admin", testPassword, "/")
	// A session from an older, longer SESSION_MAX must go too.
	if err := e.sessions.CreateSession(t.Context(), store.Session{IDHash: "old", ExpiresAt: e.clock.Now().AddDate(1, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.Sessions().RevokeAll(t.Context())
	if err != nil || n != 3 || len(e.sessions.all()) != 0 {
		t.Fatalf("revoked %d (%v), left %d", n, err, len(e.sessions.all()))
	}
	if r := b.get("/viewer"); r.code != http.StatusSeeOther {
		t.Fatalf("still logged in: %d", r.code)
	}
}

// Regression (review of 496b7d0): without a user-wide budget, a botnet
// whose members each stay within their own budget could guess the admin
// password at an unbounded aggregate rate. The rate must stay bounded.
func TestDistributedGuessingIsSlowed(t *testing.T) {
	var l Limiter
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	allowed := 0
	for s := range 900 { // a fresh network every second for 15 minutes
		if ok, _ := l.Allow(start.Add(time.Duration(s)*time.Second), "net-"+strconv.Itoa(s), "admin", false); ok {
			allowed++
		}
	}
	if max := userBudget + 900/int(userSlow/time.Second); allowed > max {
		t.Fatalf("%d guesses in 15 min from distinct networks, want <= %d", allowed, max)
	}
	// Slowed, not locked: the user still gets a slot after userSlow.
	if ok, _ := l.Allow(start.Add(900*time.Second+userSlow), "admin-home", "admin", false); !ok {
		t.Fatal("user-wide budget locked the account")
	}
}

// The same through the handler: the login path must apply the user budget.
func TestLoginAppliesUserWideBudget(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	for i := range userBudget {
		e.svc.o.Limiter.Allow(e.clock.Now(), "10.9.0."+strconv.Itoa(i), "admin", false)
	}
	if r := postLocal(e.browser(), "203.0.113.20", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("user-wide budget not applied by the handler: %d", r.code)
	}
	e.clock.Advance(userSlow)
	if r := postLocal(e.browser(), "203.0.113.20", "admin", testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("admin not admitted after the slow interval: %d", r.code)
	}
}

// Rotating usernames from one network must hit the per-network budget.
func TestUsernameRotationFromOneNetwork(t *testing.T) {
	var l Limiter
	now := time.Now()
	for i := range netBudget {
		if ok, _ := l.Allow(now, "198.51.100.7", "user"+strconv.Itoa(i), false); !ok {
			t.Fatalf("attempt %d refused early", i+1)
		}
	}
	if ok, _ := l.Allow(now, "198.51.100.7", "admin", false); ok {
		t.Fatal("username rotation bypassed the per-network budget")
	}
	e := newEnv(t, ModeOIDC)
	for i := range netBudget {
		e.svc.o.Limiter.Allow(e.clock.Now(), "198.51.100.7", "user"+strconv.Itoa(i), false)
	}
	if r := postLocal(e.browser(), "198.51.100.7", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("handler ignores the per-network budget: %d", r.code)
	}
}

// Unknown usernames are limited exactly like known ones (no free
// enumeration), and the check happens before lookup and hashing.
func TestUnknownUserIsRateLimited(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	for i := 1; i <= 5; i++ {
		if r := postLocal(b, "198.51.100.8", "nobody", "whatever-password"); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	lookups := e.users.lookups()
	if r := postLocal(b, "198.51.100.8", "nobody", "whatever-password"); r.code != http.StatusTooManyRequests {
		t.Fatalf("unknown user not limited: %d", r.code)
	}
	if e.users.lookups() != lookups {
		t.Fatal("account looked up although the attempt was rate limited")
	}
}

// A success clears only its own (network, user) pair, never an attacker's.
func TestSuccessClearsOnlyOwnPair(t *testing.T) {
	var l Limiter
	now := time.Now()
	for range pairFree {
		l.Allow(now, "evil", "admin", false)
	}
	l.Allow(now, "home", "admin", false)
	l.Success("home", "admin")
	if ok, _ := l.Allow(now, "evil", "admin", false); ok {
		t.Fatal("admin's success reset the attacker's counter")
	}
	for range pairFree - 1 {
		if ok, _ := l.Allow(now, "home", "admin", false); !ok {
			t.Fatal("own pair not cleared by success")
		}
	}
}

// Parallel requests are counted at admission, so a burst cannot slip past.
func TestLimiterCountsAtAdmission(t *testing.T) {
	var l Limiter
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Go(func() {
			if ok, _ := l.Allow(now, "evil", "admin", false); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != pairFree {
		t.Fatalf("%d parallel attempts admitted, want %d", allowed, pairFree)
	}
}

// From an untrusted peer, X-Forwarded-For must not change the limiter key.
func TestSpoofedForwardedForDoesNotEvadeLimit(t *testing.T) {
	e := newEnv(t, ModeOIDC, func(o *Options) { o.Proxy.TrustedProxies = nil })
	b := e.browser()
	for i := range 5 {
		postLocal(b, "203.0.113."+strconv.Itoa(i+1), "admin", "wrong password!")
	}
	if r := postLocal(b, "203.0.113.99", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For reset the limit: %d", r.code)
	}
}

// throttleUser pushes user into the user-wide slow mode, as an attacker
// with many networks would.
func throttleUser(e *env, user string) {
	for i := range userBudget {
		e.svc.o.Limiter.Allow(e.clock.Now(), "attacker-net-"+strconv.Itoa(i), user, false)
	}
}

// The real admin's browser keeps working while an attacker throttles the
// username (review of abd0dd8: the user-wide tier alone allowed lockout).
func TestDeviceCookieBypassesGlobalTiers(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	admin := e.browser()
	if r := postLocal(admin, "192.0.2.10", "admin", testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("first login: %d", r.code)
	}
	if admin.cookie(CookieInsecure+"-device") == "" {
		t.Fatal("no device cookie after a successful login")
	}
	e.clock.Advance(time.Hour)
	throttleUser(e, "admin")
	if r := postLocal(e.browser(), "192.0.2.77", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("unrecognised client not throttled: %d", r.code)
	}
	if r := postLocal(admin, "192.0.2.77", "admin", testPassword); r.code != http.StatusSeeOther {
		t.Fatalf("admin with device cookie locked out: %d", r.code)
	}
}

// A device cookie does not lift the per-(network, user) limit: a stolen
// cookie still allows only slow guessing.
func TestDeviceCookieStillPairLimited(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	postLocal(b, "192.0.2.10", "admin", testPassword)
	for i := range pairFree {
		if r := postLocal(b, "192.0.2.10", "admin", "wrong password!"); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, r.code)
		}
	}
	if r := postLocal(b, "192.0.2.10", "admin", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("device cookie bypassed the pair limit: %d", r.code)
	}
}

func TestDeviceCookieCannotBeForgedOrReused(t *testing.T) {
	cases := map[string]func(e *env, b *browser){
		"tampered MAC": func(_ *env, b *browser) {
			v := b.cookie(CookieInsecure + "-device")
			b.setCookie(CookieInsecure+"-device", v[:len(v)-2]+"AA")
		},
		"username swapped, MAC kept": func(_ *env, b *browser) {
			parts := strings.Split(b.cookie(CookieInsecure+"-device"), ".")
			parts[1] = "b3Bz" // "ops"
			b.setCookie(CookieInsecure+"-device", strings.Join(parts, "."))
		},
		"replayed for another user": func(*env, *browser) {}, // genuine admin cookie, login as ops
		"password changed since": func(e *env, _ *browser) {
			e.users.cli(func(m map[string]store.LocalUser) {
				for _, n := range []string{"admin", "ops"} {
					u := m[n]
					u.UpdatedAt = e.clock.Now()
					m[n] = u
				}
			})
		},
		"older than 180 days": func(e *env, _ *browser) { e.clock.Advance(181 * 24 * time.Hour) },
		"valid-looking payload without MAC": func(e *env, b *browser) {
			b.setCookie(CookieInsecure+"-device", devicePayload("admin", e.clock.Now(), "id"))
		},
		"valid-looking payload with empty MAC": func(e *env, b *browser) {
			b.setCookie(CookieInsecure+"-device", devicePayload("admin", e.clock.Now(), "id")+".")
		},
		"valid-looking payload with another cookie's MAC": func(e *env, b *browser) {
			parts := strings.Split(b.cookie(CookieInsecure+"-device"), ".")
			b.setCookie(CookieInsecure+"-device", devicePayload("admin", e.clock.Now(), "other-id")+"."+parts[4])
		},
		"issued in the future": func(e *env, b *browser) {
			p := devicePayload("admin", e.clock.Now().Add(time.Hour), "id")
			b.setCookie(CookieInsecure+"-device", p+"."+e.svc.deviceMAC(p))
		},
		"unknown version": func(e *env, b *browser) {
			p := "v0" + strings.TrimPrefix(devicePayload("admin", e.clock.Now(), "id"), "v1")
			b.setCookie(CookieInsecure+"-device", p+"."+e.svc.deviceMAC(p))
		},
		"signed with another key": func(e *env, b *browser) {
			other, err := New(Options{Mode: ModeOIDC, Proxy: e.proxy, Sessions: e.sessions, Audit: e.audit, Clock: e.clock,
				SecretKey: []byte(strings.Repeat("k", 32)), LocalLogin: true, LocalUsers: e.users, DeviceCookies: e.cookies, OIDC: OIDCOptions{Issuer: "http://unused.invalid/", ClientID: "x"}})
			if err != nil {
				e.t.Fatal(err)
			}
			w := httptest.NewRecorder()
			other.setDeviceCookie(w, httptest.NewRequestWithContext(e.t.Context(), http.MethodGet, "/", nil), "ops", 0)
			b.setCookie(CookieInsecure+"-device", w.Result().Cookies()[0].Value)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, ModeOIDC)
			e.users.cli(func(m map[string]store.LocalUser) {
				m["ops"] = store.LocalUser{Username: "ops", PasswordHash: testHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator}
			})
			b := e.browser()
			if r := postLocal(b, "192.0.2.10", "admin", testPassword); r.code != http.StatusSeeOther {
				t.Fatalf("login: %d", r.code)
			}
			e.clock.Advance(time.Minute)
			tamper(e, b)
			user := "admin"
			if strings.Contains(name, "user") || strings.Contains(name, "key") {
				user = "ops"
			}
			throttleUser(e, user)
			if r := postLocal(b, "192.0.2.10", user, testPassword); r.code != http.StatusTooManyRequests {
				t.Fatalf("invalid device cookie bypassed the throttle: %d", r.code)
			}
		})
	}
}

// A flood of distinct networks and usernames must not grow memory without
// bound; once full, new keys stay limited and recognised clients still pass.
func TestLimiterMemoryBounded(t *testing.T) {
	l := Limiter{MaxKeys: 1000}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 1_000_000 {
		l.Allow(now.Add(time.Duration(i)*time.Microsecond), "2001:db8:"+strconv.Itoa(i)+"::/64", "user-"+strconv.Itoa(i), false)
	}
	for i, n := range l.size() {
		if n > 1000 {
			t.Fatalf("tier %d holds %d keys, cap 1000", i, n)
		}
	}
	for i := range l.tiers {
		if len(l.tiers[i].overflow) > userBudget+int(userWindow/userSlow)+pairFree {
			t.Fatalf("overflow bucket %d grew to %d", i, len(l.tiers[i].overflow))
		}
	}
	later := now.Add(2 * time.Second)
	if ok, _ := l.Allow(later, "fresh-net", "fresh-user", false); ok {
		t.Fatal("full table admitted an unknown key without limit")
	}
	if ok, _ := l.Allow(later, "fresh-net", "admin", true); !ok {
		t.Fatal("recognised client blocked by the flood")
	}
	// Expired entries are pruned, so the table recovers.
	if ok, _ := l.Allow(later.Add(2*time.Hour), "fresh-net", "fresh-user", false); !ok {
		t.Fatal("table did not recover after expiry")
	}
	if s := l.size(); s[tPair] > 1 || s[tNet] > 1 || s[tUser] > 1 {
		t.Fatalf("expired keys not pruned: %v", s)
	}
}

// A stolen device cookie used from many networks is still bounded by the
// recognised per-user tier, and unrecognised attackers cannot exhaust it.
func TestRecognisedUserTier(t *testing.T) {
	var l Limiter
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	allowed := 0
	for s := range 900 {
		if ok, _ := l.Allow(now.Add(time.Duration(s)*time.Second), "thief-"+strconv.Itoa(s), "admin", true); ok {
			allowed++
		}
	}
	if max := recUserBudget + 900/int(recUserSlow/time.Second); allowed > max {
		t.Fatalf("stolen device cookie: %d guesses in 15 min, want <= %d", allowed, max)
	}
	var l2 Limiter
	for s := range 900 {
		l2.Allow(now.Add(time.Duration(s)*time.Second), "bot-"+strconv.Itoa(s), "admin", false)
	}
	if ok, _ := l2.Allow(now.Add(900*time.Second), "home", "admin", true); !ok {
		t.Fatal("unrecognised flood exhausted the recognised tier")
	}
}

// Exponential pair delay is capped at 15 minutes (never a permanent lock).
func TestPairDelayCapped(t *testing.T) {
	var l Limiter
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var last time.Duration
	for range 12 {
		ok, retry := l.Allow(now, "n", "admin", false)
		for !ok {
			last = retry
			now = now.Add(retry)
			ok, retry = l.Allow(now, "n", "admin", false)
		}
	}
	if last != pairCap {
		t.Fatalf("delay after many attempts %v, want cap %v", last, pairCap)
	}
}

// After the network budget the network gets one attempt per minute.
func TestNetworkTierSlowRate(t *testing.T) {
	var l Limiter
	now := time.Now()
	for i := range netBudget {
		l.Allow(now, "n", "u"+strconv.Itoa(i), false)
	}
	if ok, retry := l.Allow(now, "n", "x", false); ok || retry != netSlow {
		t.Fatalf("over budget: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.Allow(now.Add(netSlow), "n", "x", false); !ok {
		t.Fatal("no attempt after one minute")
	}
}
