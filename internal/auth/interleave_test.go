package auth

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// hookedUsers runs before once, right before the next mutation reaches the
// store: after the request's authentication and step-up checks, exactly where
// a concurrent request can change the acting account.
type hookedUsers struct {
	*fakeUsers
	before func()
}

func (h *hookedUsers) fire() {
	if f := h.before; f != nil {
		h.before = nil
		f()
	}
}

func (h *hookedUsers) AddLocalUser(ctx context.Context, u store.LocalUser, by store.Actor) error {
	h.fire()
	return h.fakeUsers.AddLocalUser(ctx, u, by)
}
func (h *hookedUsers) SetLocalPassword(ctx context.Context, n, hash string, at time.Time, m bool, by store.Actor) error {
	h.fire()
	return h.fakeUsers.SetLocalPassword(ctx, n, hash, at, m, by)
}
func (h *hookedUsers) ChangeOwnPassword(ctx context.Context, n string, g int64, hash string, at time.Time) error {
	h.fire()
	return h.fakeUsers.ChangeOwnPassword(ctx, n, g, hash, at)
}
func (h *hookedUsers) SetLocalRole(ctx context.Context, n string, r model.Role, by store.Actor) error {
	h.fire()
	return h.fakeUsers.SetLocalRole(ctx, n, r, by)
}
func (h *hookedUsers) SetLocalDisabled(ctx context.Context, n string, d bool, by store.Actor) error {
	h.fire()
	return h.fakeUsers.SetLocalDisabled(ctx, n, d, by)
}
func (h *hookedUsers) DeleteLocalUser(ctx context.Context, n string, by store.Actor) error {
	h.fire()
	return h.fakeUsers.DeleteLocalUser(ctx, n, by)
}

func hookedEnv(t *testing.T) (*env, *hookedUsers) {
	t.Helper()
	var h *hookedUsers
	e := newEnv(t, ModeLocal, func(o *Options) {
		o.Proxy.PublicURL = ""
		h = &hookedUsers{fakeUsers: o.LocalUsers.(*fakeUsers)}
		o.LocalUsers = h
	})
	opsHash, err := HashPassword(opsPassword)
	if err != nil {
		t.Fatal(err)
	}
	e.users.cli(func(m map[string]store.LocalUser) {
		m["ops"] = store.LocalUser{Username: "ops", PasswordHash: opsHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator, PasswordGen: 1}
		m["admin"] = store.LocalUser{Username: "admin", PasswordHash: testHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleAdmin, PasswordGen: 1}
		m["boss2"] = store.LocalUser{Username: "boss2", PasswordHash: testHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleAdmin, PasswordGen: 1}
	})
	return e, h
}

// Why: an admin's authority is checked when the request starts and again,
// atomically, when the change is written. If the admin is demoted, disabled
// or has its password changed in between, the change must not happen (review:
// stale identity / race condition in the account handlers).
func TestAdminActionIsRefusedWhenTheActorChangedMidRequest(t *testing.T) {
	ruins := map[string]func(m map[string]store.LocalUser){
		"demoted":          func(m map[string]store.LocalUser) { u := m["admin"]; u.Role = model.RoleOperator; m["admin"] = u },
		"disabled":         func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = true; m["admin"] = u },
		"password changed": func(m map[string]store.LocalUser) { u := m["admin"]; u.PasswordGen++; m["admin"] = u },
	}
	actions := map[string]struct {
		path string
		form url.Values
	}{
		"create":   {"/users", url.Values{"username": {"newbie"}, "password": {newPassword}, "role": {"admin"}}},
		"role":     {"/users/local/ops", url.Values{"action": {"role"}, "role": {"admin"}}},
		"disable":  {"/users/local/ops", url.Values{"action": {"disable"}}},
		"password": {"/users/local/ops", url.Values{"action": {"password"}, "password": {"attacker chosen 1"}}},
		"delete":   {"/users/local/ops", url.Values{"action": {"delete"}}},
	}
	for ruin, doRuin := range ruins {
		for name, a := range actions {
			t.Run(ruin+"/"+name, func(t *testing.T) {
				e, h := hookedEnv(t)
				adm := e.login("admin", testPassword)
				adm.stepUp(testPassword)
				before := e.users.users["ops"]
				h.before = func() { e.users.cli(doRuin) }
				r := adm.post(a.path, a.form)
				if r.code != http.StatusForbidden {
					t.Fatalf("%s after the admin was %s: %d, want 403", name, ruin, r.code)
				}
				if got := e.users.users["ops"]; got != before {
					t.Fatalf("the refused change was applied: %+v -> %+v", before, got)
				}
				if _, ok := e.users.users["newbie"]; ok {
					t.Fatal("the refused create made an account")
				}
				if a := e.audit.last(); a.Result != "denied: acting account changed" {
					t.Errorf("audit %+v", a)
				}
			})
		}
	}
}

// Why: a step-up is bound to the session and the generation it was validated
// against; neither another session nor a later generation may reuse it.
func TestStepUpIsBoundToSessionAndGeneration(t *testing.T) {
	su := newStepUps()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	su.grant("sess-1", 3, now)
	if !su.valid("sess-1", 3, now.Add(time.Minute)) {
		t.Fatal("fresh step-up")
	}
	if su.valid("sess-2", 3, now.Add(time.Minute)) {
		t.Error("step-up valid for another session")
	}
	if su.valid("sess-1", 4, now.Add(time.Minute)) {
		t.Error("step-up survived a password generation change")
	}
	if su.valid("sess-1", 3, now.Add(stepUpTTL)) {
		t.Error("step-up outlived 5 minutes")
	}
}

// Why: the owner's own change is a compare-and-set. If an admin resets the
// password (or the owner changed it elsewhere) while the form is submitted,
// the later write must lose instead of silently overwriting.
func TestOwnPasswordChangeLosesAgainstAConcurrentReset(t *testing.T) {
	e, h := hookedEnv(t)
	ops := e.login("ops", opsPassword)
	h.before = func() {
		e.users.cli(func(m map[string]store.LocalUser) {
			u := m["ops"]
			u.PasswordGen++
			u.PasswordHash = "reset-by-admin"
			m["ops"] = u
		})
	}
	r := ops.post("/account/password", url.Values{"current": {opsPassword}, "new": {newPassword}, "confirm": {newPassword}})
	if r.code != http.StatusConflict {
		t.Fatalf("change that raced a reset: %d, want 409", r.code)
	}
	if e.users.users["ops"].PasswordHash != "reset-by-admin" {
		t.Fatal("the stale change overwrote the concurrent reset")
	}
	if r := ops.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("the pre-reset session still works")
	}
}
