package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// inputTag returns the opening tag of the input, select or textarea called name.
func inputTag(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`<(?:input|select|textarea)\b[^>]*name="` + regexp.QuoteMeta(name) + `"[^>]*>`).FindString(body)
	if m == "" {
		t.Fatalf("no input %q in: %s", name, body)
	}
	return m
}

// wantFieldError checks the whole pattern for one input: marked invalid, linked
// to its message, focus there, the message shown exactly once, and no page
// banner repeating it.
func wantFieldError(t *testing.T, body, name, msg string) {
	t.Helper()
	tag := inputTag(t, body, name)
	for _, want := range []string{`aria-invalid="true"`, `aria-describedby="err-` + name + `"`, "autofocus"} {
		if !strings.Contains(tag, want) {
			t.Errorf("%s: %s missing in %s", name, want, tag)
		}
	}
	if n := strings.Count(body, msg); n != 1 {
		t.Errorf("%s: %q shown %d times, want once", name, msg, n)
	}
	if !strings.Contains(body, `id="err-`+name+`"`) {
		t.Errorf("%s: no message element", name)
	}
	if n := strings.Count(body, "autofocus"); n != 1 {
		t.Errorf("%s: %d autofocus attributes, want exactly the failing field", name, n)
	}
}

func renderAccountPage(t *testing.T, pages *Pages, f func(w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), model.Principal{Subject: "local:admin", Name: "admin", Role: model.RoleAdmin, Method: "local"})
		f(w, r.WithContext(ctx))
	}))
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/tf/x", nil))
	return rec.Body.String()
}

// Why: Package F finding "auth: password shorter than 12 characters": the
// sentence sits under the New password input, the focus goes there (not to
// Current password), and the banner does not repeat it. Both page variants
// (in the app shell and the centred forced-change page) must do this.
func TestPasswordPagesShowTheErrorAtTheField(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	for _, forced := range []bool{false, true} {
		d := auth.PasswordPageData{BasePath: "/tf", CSRFToken: "tok", Forced: forced, Local: true, ErrField: "new", Error: "Use at least 12 characters."}
		body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
			pages.ChangePassword(w, r, http.StatusBadRequest, d)
		})
		wantFieldError(t, body, "new", "Use at least 12 characters.")
		if strings.Contains(inputTag(t, body, "current"), "autofocus") {
			t.Errorf("forced=%v: focus stayed on Current password", forced)
		}
		if strings.Contains(body, `class="banner tone-danger"`) {
			t.Errorf("forced=%v: the page banner repeats the field error", forced)
		}
	}
	// Without a field the banner is the right place and the first input keeps the focus.
	d := auth.PasswordPageData{BasePath: "/tf", CSRFToken: "tok", Local: true, Error: "Too many attempts. Wait a minute and try again."}
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.ChangePassword(w, r, http.StatusTooManyRequests, d)
	})
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "Too many attempts") || !strings.Contains(inputTag(t, body, "current"), "autofocus") {
		t.Errorf("page-level error lost its banner or the first focus: %s", body)
	}
}

// Why: Package F finding "users create loses the typed username and shows no
// 12-character hint": after a failed create the add-user page comes back refilled
// (never the passwords), the password input carries the message, and a fresh form
// says how long the password must be.
func TestUsersCreateFormKeepsInputAndShowsFieldError(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	d := auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", ErrField: "password", Error: "Use at least 12 characters.", Username: "newbie", Role: "admin",
		DisplayName: "New Bie", New: true, Users: []auth.UserRow{{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin"}}}
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) { pages.Users(w, r, http.StatusBadRequest, d) })
	wantFieldError(t, body, "password", "Use at least 12 characters.")
	if !strings.Contains(inputTag(t, body, "username"), `value="newbie"`) || !strings.Contains(inputTag(t, body, "display_name"), `value="New Bie"`) {
		t.Error("the typed username or name was lost")
	}
	if !regexp.MustCompile(`<input type="radio" name="role" value="admin" checked>`).MatchString(body) {
		t.Error("the chosen role was lost")
	}
	if !strings.Contains(body, "1 field needs attention.") {
		t.Error("no error summary above the form")
	}
	d.ErrField, d.Error = "", ""
	fresh := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) { pages.Users(w, r, http.StatusOK, d) })
	if !strings.Contains(fresh, "Use at least 12 characters.") {
		t.Error("no length hint at the password input")
	}
}

// Why: a device save with a bad port used to land in the page banner only; the
// Port input must carry the message and the focus.
func TestDeviceFormBadPortIsAFieldError(t *testing.T) {
	e := setup(t)
	r, body := e.do(t, "POST", "/devices/1", "admin", url.Values{"address": {"10.0.0.5"}, "name": {"plug"}, "port": {"99999"}})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", r.StatusCode)
	}
	wantFieldError(t, body, "port", "Use a port number between 1 and 65535")
	if strings.Contains(body, "the port must be") {
		t.Error("raw message shown")
	}
}

// Why: inventory errors read "inventory: invalid device: address ..." and must
// land at the address input in words.
func TestFormProblemMapsInventoryErrors(t *testing.T) {
	cases := []struct {
		err   string
		field string
	}{
		{"address is empty", "address"},
		{`address "http://x" must be an IP or DNS name without scheme, port or path`, "address"},
		{"name must be 1-100 printable characters", "name"},
		{"port 70000", "port"},
	}
	for _, c := range cases {
		field, msg := formProblem(wrapInvalid(c.err))
		if field != c.field || msg == "" || strings.Contains(msg, "inventory:") {
			t.Errorf("%s -> %q %q", c.err, field, msg)
		}
	}
	if f, _ := formProblem(errors.New("db is down")); f != "" {
		t.Errorf("unrelated error bound to %q", f)
	}
}

// Why: "Backup failed: backup: download config: tasmota: ... HTTP 404" is an
// error chain for the log. People read what happened and what to try.
func TestBackupFailureIsHumanText(t *testing.T) {
	e := setup(t)
	e.bk.failErr = errors.New("backup: download config: tasmota: GET /dl: HTTP 404")
	_, body := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
	if strings.Contains(body, "HTTP 404") || strings.Contains(body, "download config") || !strings.Contains(body, "Backup failed.") {
		t.Fatalf("raw chain or no message: %s", body)
	}
}

// Why: a rejected system setting must be marked at its own input (border,
// aria-invalid, linked message), not only listed beside it.
func TestSystemSettingErrorMarksTheInput(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"0"}})
	tag := inputTag(t, body, "v.BACKUP_KEEP")
	if !strings.Contains(tag, `aria-invalid="true"`) || !strings.Contains(tag, `aria-describedby="err-v.BACKUP_KEEP"`) {
		t.Fatalf("input not marked: %s", tag)
	}
	if strings.Contains(inputTag(t, body, "v.POLL_INTERVAL"), "aria-invalid") {
		t.Fatal("a valid input was marked")
	}
}

// Why: the rejection starts a sentence and the card below must not read as a
// continuation of it.
func TestUpdatePreviewWithoutDevicesAsksInFullSentence(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	r, body := e.do(t, "POST", "/updates/preview", "operator", url.Values{"release": {"16.1.0"}, "policy": {"only-newer"}})
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Select at least one device.") {
		t.Fatalf("%d: %s", r.StatusCode, body)
	}
}

func wrapInvalid(detail string) error { return fmt.Errorf("%w: %s", inventory.ErrInvalid, detail) }
