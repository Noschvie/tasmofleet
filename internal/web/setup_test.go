package web

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

type fakeSetup struct {
	done bool
	svc  *sysconfig.Service
}

func (f *fakeSetup) Done() bool { return f.done }
func (f *fakeSetup) Finish(context.Context, string) error {
	f.done = true
	return nil
}
func (f *fakeSetup) Suggestions() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("192.168.7.0/24")}
}
func (f *fakeSetup) NetworksConfigured() bool { return len(f.svc.Snapshot().DeviceNetworks) > 0 }

func setupHandlerEnv(t *testing.T) (*env, *sysconfig.Service, *fakeSetup) {
	t.Helper()
	fs := &fakeSetup{}
	var svc *sysconfig.Service
	e, svc2, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Setup = fs })
	svc = svc2
	fs.svc = svc
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"DEVICE_NETWORKS"}}); err != nil {
		t.Fatal(err)
	}
	return e, svc, fs
}

// Why: a fresh install must lead the admin to the setup, with suggestions that are only
// suggestions: nothing is configured until the admin saves.
func TestSetupGuidesAFreshInstallAndOnlySuggests(t *testing.T) {
	e, svc, fs := setupHandlerEnv(t)
	r, _ := e.do(t, "GET", "/", "admin", nil)
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/setup") {
		t.Fatalf("an admin on a fresh install goes to the setup: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.do(t, "GET", "/", "operator", nil); r.StatusCode != 200 {
		t.Fatalf("operators are not sent to the setup: %d", r.StatusCode)
	}
	r, body := e.do(t, "GET", "/setup", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "192.168.7.0/24") {
		t.Fatalf("setup page: %d\n%s", r.StatusCode, body)
	}
	// The banner points at the setup from every other page, never from the setup itself,
	// and it is a warning, not the green success notice.
	if strings.Contains(body, "Open the setup") {
		t.Fatal("the setup page must not link to itself")
	}
	if _, other := e.do(t, "GET", "/devices", "admin", nil); !strings.Contains(other, `<p class="banner tone-warn" role="status"><span class="banner-dot"></span><span class="banner-text">No device networks`) || !strings.Contains(other, "Open the setup") {
		t.Fatalf("the banner on other pages is a warning with a link: %s", other)
	}
	if len(svc.Snapshot().DeviceNetworks) != 0 {
		t.Fatal("a suggestion must not configure anything")
	}
	if r, _ := e.do(t, "GET", "/setup", "operator", nil); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 303 || !fs.done {
		t.Fatalf("finish: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/", "admin", nil); r.StatusCode != 200 {
		t.Fatalf("after the setup the dashboard opens: %d", r.StatusCode)
	}
}

// Why: the setup is one form (design v2 step 3). Finish saves the networks and the broker and
// ends the setup; a refused value ends nothing and keeps what was typed; the form may carry only
// the setup's own settings (no mass assignment), and only admins may post it.
func TestSetupFinishSavesAndRejectsOtherKeys(t *testing.T) {
	e, svc, fs := setupHandlerEnv(t)
	r, body := e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"not-a-cidr"}, "mqtt.host": {"broker.lan"}})
	if r.StatusCode != 422 || !strings.Contains(body, "Nothing was saved") || fs.done || len(svc.Snapshot().DeviceNetworks) != 0 {
		t.Fatalf("invalid: %d, done %v", r.StatusCode, fs.done)
	}
	if !strings.Contains(body, `>not-a-cidr</textarea>`) || !strings.Contains(body, `value="broker.lan"`) {
		t.Error("a refused form must keep what was typed")
	}
	for _, f := range []url.Values{{"v.BACKUP_KEEP": {"3"}}, {"v.OIDC_ISSUER": {"https://x/"}}, {"evil": {"1"}}} {
		f.Set("csrf", "tok")
		if r, _ := e.do(t, "POST", "/setup/finish", "admin", f); r.StatusCode != 400 || fs.done {
			t.Errorf("%v: %d", f, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "POST", "/setup/finish", "operator", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"10.0.0.0/8"}}); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	r, body = e.do(t, "POST", "/setup/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"192.168.7.0/24"}, "mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}})
	if r.StatusCode != 200 || !strings.Contains(body, "connected") || fs.done || len(svc.Snapshot().DeviceNetworks) != 0 {
		t.Fatalf("mqtt test: %d; a test saves nothing and finishes nothing", r.StatusCode)
	}
	// The stored password belongs to the old broker: a new broker without one is refused at the field.
	r, body = e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"192.168.7.0/24"}, "mqtt.host": {"broker2.lan"}})
	if r.StatusCode != 422 || !strings.Contains(body, `id="err-v.MQTT_PASSWORD"`) || fs.done {
		t.Fatalf("new broker without its password: %d", r.StatusCode)
	}
	r, body = e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"192.168.7.0/24"}, "mqtt.host": {"broker2.lan"}, "mqtt.port": {""}, "v.MQTT_PASSWORD": {"fake-new-pw"}})
	if r.StatusCode != 303 || !fs.done || len(svc.Snapshot().DeviceNetworks) != 1 || svc.Snapshot().MQTTURL != "tcp://broker2.lan:1883" {
		t.Fatalf("finish: %d, done %v, url %q: %s", r.StatusCode, fs.done, svc.Snapshot().MQTTURL, body)
	}
}

// Why: the setup is a page of its own (the prototype's card without navigation); it shows the
// three steps with the first two done, so the admin knows this is the last one.
func TestSetupIsTheLastStepWithoutNavigation(t *testing.T) {
	e, _, _ := setupHandlerEnv(t)
	_, body := e.do(t, "GET", "/setup", "admin", nil)
	for _, want := range []string{`<main id="main" class="bare">`, `aria-label="Setup steps"`, `aria-current="step"`, "Basic settings", "Finish setup"} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page lacks %q", want)
		}
	}
	if strings.Contains(body, `<header class="top">`) {
		t.Error("the setup has no navigation")
	}
}

// Why: "Scan when I finish" may only cover what a scan accepts: IPv4 ranges of at most /22.
func TestSetupScanRanges(t *testing.T) {
	got := setupScanRanges([]sysconfig.FieldState{{Field: sysconfig.Field{Key: "DEVICE_NETWORKS"}, Value: "192.168.7.0/24, 10.0.0.0/8,fd00::/64,172.31.12.0/22"}})
	if len(got) != 2 || got[0].String() != "192.168.7.0/24" || got[1].String() != "172.31.12.0/22" {
		t.Fatalf("%v", got)
	}
}

func setupAccountBody(t *testing.T, d auth.SetupAccountData) string {
	t.Helper()
	w := httptest.NewRecorder()
	NewPages().SetupAccount(w, httptest.NewRequest("GET", "/setup/account", nil), 200, d)
	return w.Body.String()
}

// Why (N12): without the token from the container log nobody may set the first admin's password,
// so the page without a valid token explains where the link is and offers no form at all.
func TestSetupStepOneWithoutTokenHasNoForm(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		body := setupAccountBody(t, auth.SetupAccountData{BasePath: "/tf", NeedToken: true, TokenInvalid: invalid, Username: "admin"})
		if strings.Contains(body, "<form") || strings.Contains(body, `name="password"`) {
			t.Errorf("invalid=%v: a form without a token", invalid)
		}
		if !strings.Contains(body, "container log") && !strings.Contains(body, "docker logs") {
			t.Errorf("invalid=%v: the page does not say where the link is", invalid)
		}
		if invalid != strings.Contains(body, "That link isn’t valid.") {
			t.Errorf("invalid=%v: wrong token state", invalid)
		}
		if !strings.Contains(body, `<li class="is-current" aria-current="step">`) || strings.Contains(body, "is-done") {
			t.Errorf("invalid=%v: step 1 is not the current step", invalid)
		}
	}
}

// Why (N12): the token is a credential (it sets the admin password). It travels only in the hidden
// form field, never as text on the page; a refused field gets the focus and its message.
func TestSetupStepOneKeepsTheTokenHiddenAndFocusesTheError(t *testing.T) {
	const tok = "fake-setup-token-7KQ2M9XW"
	body := setupAccountBody(t, auth.SetupAccountData{BasePath: "/tf", CSRFToken: "csrf-tok", Token: tok, Username: "admin",
		DisplayName: "Maren", ErrField: "confirm", Error: "The two passwords differ."})
	if n := strings.Count(body, tok); n != 1 || !strings.Contains(body, `<input type="hidden" name="token" value="`+tok+`">`) {
		t.Errorf("the token must appear once, in the hidden field (found %d)", n)
	}
	if !strings.Contains(body, `action="/tf/setup/account"`) || !strings.Contains(body, `name="csrf" value="csrf-tok"`) {
		t.Error("the form does not post to the setup step with its CSRF token")
	}
	if strings.Count(body, "autofocus") != 1 || !regexp.MustCompile(`name="confirm"[^>]*autofocus`).MatchString(body) {
		t.Error("the refused field does not get the focus")
	}
	if !strings.Contains(body, "The two passwords differ.") || !strings.Contains(body, "1 field needs attention.") || !strings.Contains(body, `value="Maren"`) {
		t.Error("the error or the typed name is lost")
	}
	if !strings.Contains(body, `<li class="is-done">`) || !strings.Contains(body, "Admin account") {
		t.Error("step 2 must follow a done step 1")
	}
}
