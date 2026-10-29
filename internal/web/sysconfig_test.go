package web

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

type sysMem struct {
	mu   sync.Mutex
	rows map[string]store.SystemSetting
}

func (m *sysMem) LoadSystemSettings(context.Context) (map[string]store.SystemSetting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]store.SystemSetting{}
	for k, v := range m.rows {
		out[k] = v
	}
	return out, nil
}

func (m *sysMem) SaveSystemSettings(_ context.Context, actor string, ch []store.SystemSettingChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range ch {
		if c.Delete {
			delete(m.rows, c.Key)
		} else {
			m.rows[c.Key] = store.SystemSetting{Key: c.Key, Value: c.Value, Secret: c.Secret, Target: c.Target, UpdatedBy: actor}
		}
	}
	return nil
}

// sysEnv builds the UI over the real sysconfig service.
func sysEnv(t *testing.T, stepped bool) (*env, *sysconfig.Service, *sysMem, *atomic.Bool) {
	return sysEnvWith(t, stepped)
}

func sysEnvWith(t *testing.T, stepped bool, extraEnv ...string) (*env, *sysconfig.Service, *sysMem, *atomic.Bool) {
	t.Helper()
	return sysEnvFull(t, stepped, nil, extraEnv...)
}

// sysEnvWithDeps is sysEnv with a hook that adds fields to the UI dependencies.
func sysEnvWithDeps(t *testing.T, mutate func(*Deps)) (*env, *sysconfig.Service, *sysMem, *atomic.Bool) {
	t.Helper()
	return sysEnvFull(t, true, mutate)
}

func sysEnvFull(t *testing.T, stepped bool, mutate func(*Deps), extraEnv ...string) (*env, *sysconfig.Service, *sysMem, *atomic.Bool) {
	t.Helper()
	return sysEnvTesters(t, stepped, nil, nil, mutate, extraEnv...)
}

// sysEnvTesters is sysEnvFull with the connection testers swapped; nil keeps the passing ones.
func sysEnvTesters(t *testing.T, stepped bool, testMQTT func(context.Context, sysconfig.MQTTParams) error, testSSO func(context.Context, string) (string, error), mutate func(*Deps), extraEnv ...string) (*env, *sysconfig.Service, *sysMem, *atomic.Bool) {
	t.Helper()
	if testMQTT == nil {
		testMQTT = func(context.Context, sysconfig.MQTTParams) error { return nil }
	}
	if testSSO == nil {
		testSSO = func(context.Context, string) (string, error) { return "discovery OK", nil }
	}
	mem := &sysMem{rows: map[string]store.SystemSetting{
		"MQTT_URL":      {Key: "MQTT_URL", Value: "tcp://broker.lan:1883"},
		"MQTT_PASSWORD": {Key: "MQTT_PASSWORD", Value: "fake-stored-pw", Secret: true, Target: "tcp://broker.lan:1883"},
	}}
	cfg, err := config.Load(append([]string{
		"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null",
	}, extraEnv...))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DeviceNetworks = []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}
	aud := &fakeAudit{}
	svc, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: mem, Audit: aud,
		TestMQTT: testMQTT, TestSSO: testSSO})
	if err != nil {
		t.Fatal(err)
	}
	st := &atomic.Bool{}
	st.Store(stepped)
	e := setupWith(t, func(d *Deps) {
		d.SysConfig = svc
		d.Guard = stepGuard{fakeGuard: fakeGuard{}, stepped: st}
		d.Recorder = aud
		if mutate != nil {
			mutate(d)
		}
	})
	return e, svc, mem, st
}

func TestSystemSettingsAreAdminOnly(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	for _, role := range []string{"", "operator"} {
		want := 403
		if role == "" {
			want = 401
		}
		if r, _ := e.do(t, "GET", "/settings/system", role, nil); r.StatusCode != want {
			t.Errorf("GET as %q: %d", role, r.StatusCode)
		}
		if r, _ := e.do(t, "POST", "/settings/system", role, url.Values{"v.BACKUP_KEEP": {"4"}}); r.StatusCode != want {
			t.Errorf("POST as %q: %d", role, r.StatusCode)
		}
	}
}

// Why: the page tells the admin what they can change and shows the bootstrap settings read-only with secrets masked.
func TestSystemSettingsPageShowsOperationalFieldsAndReadOnlySettings(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, body := e.do(t, "GET", "/settings/system", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if !strings.Contains(body, `name="v.BACKUP_KEEP"`) || !strings.Contains(body, `name="v.DEVICE_NETWORKS"`) {
		t.Error("editable fields must be inputs")
	}
	if strings.Contains(body, `name="v.LISTEN"`) || !strings.Contains(body, "LISTEN") {
		t.Error("bootstrap settings are listed read-only")
	}
	for _, leak := range []string{"fake-token-77", "fake-stored-pw"} {
		if strings.Contains(body, leak) {
			t.Errorf("secret %q leaked", leak)
		}
	}
	// CSP: no inline script, style or event handler.
	if strings.Contains(strings.ReplaceAll(body, "<script src=", ""), "<script") {
		t.Error("inline script")
	}
	for _, bad := range []string{" style=", " onclick=", " onchange=", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q, which the CSP would block", bad)
		}
	}
}

func TestSystemSettingsSaveAppliesAndAudits(t *testing.T) {
	e, svc, mem, _ := sysEnv(t, true)
	r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}, "v.BACKUP_INTERVAL": {"6h"}, "v.MQTT_PASSWORD": {""}})
	if r.StatusCode != 303 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if svc.Snapshot().BackupKeep != 4 || mem.rows["BACKUP_KEEP"].Value != "4" {
		t.Fatal("not applied")
	}
	if mem.rows["MQTT_PASSWORD"].Value != "fake-stored-pw" {
		t.Fatal("an empty write-only field means unchanged")
	}
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	if !strings.Contains(body, "restart") {
		t.Error("BACKUP_KEEP only applies after a restart: the page must say so")
	}
}

// Why: lesson from TasmoAdmin, which wrote all of $_POST into its config. Only
// keys the page offers may be posted; everything else fails the whole request.
func TestSystemSettingsRejectUnknownAndBootstrapKeys(t *testing.T) {
	for _, form := range []url.Values{
		{"v.LISTEN": {":1"}}, {"v.TRUSTED_PROXIES": {"0.0.0.0/0"}}, {"clear.TRUSTED_PROXIES": {"1"}},
		{"v.NO_SUCH": {"1"}}, {"v.BACKUP_KEEP": {"20"}, "clear.BACKUP_KEEP": {"1"}}, {"LISTEN": {":1"}}, {"v.BACKUP_KEEP": {"4"}, "extra": {"1"}}, {"v.Prov": {"x"}},
	} {
		e, svc, mem, _ := sysEnv(t, true)
		form.Set("csrf", "tok")
		r, _ := e.do(t, "POST", "/settings/system", "admin", form)
		if r.StatusCode != 400 {
			t.Errorf("%v: status %d", form, r.StatusCode)
		}
		if svc.Snapshot().BackupKeep != 10 || len(mem.rows) != 3 || svc.Snapshot().Listen != ":8080" {
			t.Errorf("%v: a rejected request must change nothing", form)
		}
	}
}

func TestSystemSettingsValidationErrorsAreShownNextToTheField(t *testing.T) {
	e, _, mem, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"0"}, "v.POLL_INTERVAL": {"2m"}})
	if r.StatusCode != 422 || !strings.Contains(body, "BACKUP_KEEP") || !strings.Contains(body, `value="2m"`) {
		t.Fatalf("status %d; the page must keep what was typed", r.StatusCode)
	}
	if len(mem.rows) != 3 {
		t.Fatal("nothing may be stored")
	}
}

// Why: the settings that decide who signs in or with which rights need a fresh
// confirmation (here the groups claim); without it the page asks for it and
// nothing is stored. Routine settings and sign-in cosmetics do not (audit #19).
func TestSystemSettingsSignInChangesNeedStepUp(t *testing.T) {
	e, svc, _, st := sysEnv(t, false)
	form := url.Values{"csrf": {"tok"}, "v.OIDC_GROUPS_CLAIM": {"roles"}}
	r, body := e.do(t, "POST", "/settings/system", "admin", form)
	if r.StatusCode != 403 || !strings.Contains(body, `name="current"`) {
		t.Fatalf("status %d; expected the password prompt", r.StatusCode)
	}
	if svc.Snapshot().OIDCGroupsClaim == "roles" {
		t.Fatal("changed without a step-up")
	}
	if r, _ := e.do(t, "POST", "/settings/system/stepup", "admin", url.Values{"csrf": {"tok"}, "current": {"wrong"}}); r.StatusCode != 403 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	// The redirect asks the page to restore what was typed before the prompt (audit #20).
	if r, _ := e.do(t, "POST", "/settings/system/stepup", "admin", url.Values{"csrf": {"tok"}, "current": {"right"}}); r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "restore=1") {
		t.Fatalf("right password: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if !st.Load() {
		t.Fatal("step-up not recorded")
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", form); r.StatusCode != 303 || svc.Snapshot().OIDCGroupsClaim != "roles" {
		t.Fatalf("after step-up: %d", r.StatusCode)
	}
	// Routine settings and sign-in cosmetics never ask.
	st.Store(false)
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_DISPLAY_NAME": {"Authentik"}, "v.OIDC_SCOPES": {"openid,email"}}); r.StatusCode != 303 || svc.Snapshot().OIDCDisplayName != "Authentik" {
		t.Fatalf("sign-in cosmetics: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"172.31.13.0/24,10.0.0.0/8"}, "v.BACKUP_KEEP": {"2"}}); r.StatusCode != 303 {
		t.Fatalf("routine settings: %d", r.StatusCode)
	}
}

// Why: header principals cannot confirm anything (the proxy is their only
// authority), so the sign-in settings are only possible through the container configuration.
func TestSystemSettingsHeaderPrincipalCannotChangeSignIn(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_ISSUER": {"https://idp.example/"}}, "X-Test-Method", "header")
	if r.StatusCode != 403 || !strings.Contains(body, "cannot be confirmed") {
		t.Fatalf("status %d", r.StatusCode)
	}
	if svc.Snapshot().OIDCIssuer != "" {
		t.Fatal("changed without a possible confirmation")
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"2"}}, "X-Test-Method", "header"); r.StatusCode != 303 {
		t.Fatalf("a routine setting stays possible: %d", r.StatusCode)
	}
}

func TestSystemSettingsSecretNeverComesBack(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	// MQTT_PASSWORD is set by the environment in sysEnv; the test uses the MQTT test button instead.
	r, body := e.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "v.MQTT_URL": {""}})
	if r.StatusCode != 200 || !strings.Contains(body, "connected") || strings.Contains(body, "fake-stored-pw") {
		t.Fatalf("status %d", r.StatusCode)
	}
}

// Why: a sign-in setting the operator fixed in the container configuration is
// read-only on the page, and a forged request for it fails as a whole.
func TestSystemSettingsEnvironmentLockedSignInSettingIsReadOnly(t *testing.T) {
	e, svc, mem, _ := sysEnvWith(t, true, "TASMOFLEET_OIDC_CLIENT_ID=envid")
	_, body := e.do(t, "GET", "/settings/system", "admin", nil)
	if strings.Contains(body, `name="v.OIDC_CLIENT_ID"`) || !strings.Contains(body, "Set by environment") || !strings.Contains(body, "envid") {
		t.Fatal("locked fields are shown read-only with the reason")
	}
	r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_CLIENT_ID": {"x"}, "v.BACKUP_KEEP": {"2"}})
	if r.StatusCode != 403 || svc.Snapshot().OIDCClientID != "envid" || svc.Snapshot().BackupKeep != 10 || len(mem.rows) != 3 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

// Why: "Test discovery" is a button of the settings form, so the whole form arrives with it;
// only the issuer may count, and nothing else in the form is saved by a test.
func TestSystemSettingsSSOTest(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	keep := svc.Snapshot().BackupKeep
	r, body := e.do(t, "POST", "/settings/system/sso-test", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_ISSUER": {"https://idp.example/"}, "v.BACKUP_KEEP": {"2"}})
	if r.StatusCode != 200 || !strings.Contains(body, "discovery OK") {
		t.Fatalf("status %d", r.StatusCode)
	}
	if svc.Snapshot().BackupKeep != keep || svc.Snapshot().OIDCIssuer != "" {
		t.Fatal("a test saved a setting")
	}
	if r, _ := e.do(t, "POST", "/settings/system/sso-test", "admin", url.Values{"csrf": {"tok"}, "v.NO_SUCH_KEY": {"x"}}); r.StatusCode != 400 {
		t.Fatalf("unknown key: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/settings/system/sso-test", "operator", url.Values{"csrf": {"tok"}}); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
}

// Why: "Test MQTT" is a button of the settings form, so the whole form arrives with it; the
// test must use the broker fields only and never save the other values that came along.
func TestSystemSettingsMQTTTestIgnoresTheRestOfTheForm(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	keep := svc.Snapshot().BackupKeep
	r, body := e.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "v.MQTT_URL": {""}, "v.BACKUP_KEEP": {"2"}, "clear.BACKUP_INTERVAL": {"1"}})
	if r.StatusCode != 200 || !strings.Contains(body, "connected") {
		t.Fatalf("status %d", r.StatusCode)
	}
	if svc.Snapshot().BackupKeep != keep {
		t.Fatal("a connection test saved a setting")
	}
	if r, _ := e.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "v.NO_SUCH_KEY": {"x"}}); r.StatusCode != 400 {
		t.Fatalf("unknown key: %d", r.StatusCode)
	}
}

// Why: a connection test that ran and failed must not look like a success. The
// green notice with a check mark told the operator the broker was fine while the
// audit log said "failed". Failure uses the error notice, success the success one.
func TestSystemSettingsFailedTestsUseTheErrorNotice(t *testing.T) {
	errBroker := errors.New("this address is not allowed as a broker")
	for _, c := range []struct{ path, field, want string }{
		{"/settings/system/mqtt-test", "v.MQTT_URL", "MQTT connection test: failed: this address is not allowed as a broker"},
		{"/settings/system/sso-test", "v.OIDC_ISSUER", "SSO check: failed: discovery refused"},
	} {
		// one environment per test: the service rate-limits tests to one per few seconds
		e, _, _, _ := sysEnvTesters(t, true,
			func(context.Context, sysconfig.MQTTParams) error { return errBroker },
			func(context.Context, string) (string, error) { return "", errors.New("discovery refused") }, nil)
		val := "tcp://broker.lan:1883"
		if c.field == "v.OIDC_ISSUER" {
			val = "https://idp.example/"
		}
		_, body := e.do(t, "POST", c.path, "admin", url.Values{"csrf": {"tok"}, c.field: {val}})
		if !strings.Contains(body, `<span class="sys-test-res t-danger" role="alert">`+c.want) {
			t.Errorf("%s: a failed test must render as an alert in the danger tone, body lacks %q", c.path, c.want)
		}
		if strings.Contains(body, `<span class="sys-test-res t-ok" role="status">`+strings.SplitN(c.want, ":", 2)[0]) {
			t.Errorf("%s: a failed test rendered as a success", c.path)
		}
	}
	ok, _, _, _ := sysEnv(t, true)
	_, body := ok.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "v.MQTT_URL": {""}})
	if !strings.Contains(body, `<span class="sys-test-res t-ok" role="status">MQTT connection test: connected`) {
		t.Error("a passing test keeps the success notice")
	}
}

// Why (N18): the MQTT tab posts host, port and TLS; the server composes the
// stored MQTT_URL from them. TLS must pick ssl:// and its default port, an
// empty host turns MQTT off, and a bad port changes nothing.
func TestSystemSettingsMQTTPartsComposeTheURL(t *testing.T) {
	cases := []struct {
		form url.Values
		want string
	}{
		{url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}}, "tcp://broker.lan:1883"},
		{url.Values{"mqtt.host": {"broker.lan"}, "mqtt.tls": {"1"}, "v.MQTT_PASSWORD": {"fake-new-pw"}}, "ssl://broker.lan:8883"},
		{url.Values{"mqtt.host": {"other.lan"}, "mqtt.port": {"1884"}, "clear.MQTT_PASSWORD": {"1"}}, "tcp://other.lan:1884"},
		{url.Values{"mqtt.host": {""}, "mqtt.port": {"1883"}, "clear.MQTT_PASSWORD": {"1"}}, ""},
	}
	for _, c := range cases {
		e, svc, _, _ := sysEnv(t, true)
		c.form.Set("csrf", "tok")
		r, body := e.do(t, "POST", "/settings/system", "admin", c.form)
		if r.StatusCode != 303 {
			t.Fatalf("%v: status %d %s", c.form, r.StatusCode, body)
		}
		if got := svc.Snapshot().MQTTURL; got != c.want {
			t.Errorf("%v: MQTT_URL %q, want %q", c.form, got, c.want)
		}
	}
	for _, form := range []url.Values{
		{"mqtt.host": {"broker.lan"}, "mqtt.port": {"99999"}},
		{"mqtt.host": {"u:p@broker.lan"}},
		{"mqtt.host": {"broker.lan"}, "v.MQTT_URL": {"tcp://x:1"}},
		{"mqtt.port": {"1884"}},
		{"mqtt.host": {"broker.lan"}, "mqtt.tls": {"maybe"}},
	} {
		e, svc, _, _ := sysEnv(t, true)
		form.Set("csrf", "tok")
		if r, _ := e.do(t, "POST", "/settings/system", "admin", form); r.StatusCode != 422 {
			t.Errorf("%v: status %d, want 422", form, r.StatusCode)
		}
		if svc.Snapshot().MQTTURL != "tcp://broker.lan:1883" {
			t.Errorf("%v: a refused form must change nothing", form)
		}
	}
}

// Why (N18): the connection test checks the broker the parts describe, not
// the stored one.
func TestSystemSettingsMQTTTestUsesTheParts(t *testing.T) {
	var seen sysconfig.MQTTParams
	e, _, _, _ := sysEnvTesters(t, true, func(_ context.Context, p sysconfig.MQTTParams) error { seen = p; return nil }, nil, nil)
	r, _ := e.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "mqtt.host": {"broker.lan"}, "mqtt.tls": {"on"}, "test_password": {"fake-typed"}})
	if r.StatusCode != 200 || seen.URL != "ssl://broker.lan:8883" {
		t.Fatalf("status %d, tested %q", r.StatusCode, seen.URL)
	}
}

// Why (N18): the page prefills the parts from the stored URL, so a save that
// does not touch the broker posts them unchanged. A stored mqtt:// URL without
// a port must stay as it is: rewriting it to tcp://host:1883 would reconnect
// MQTT and, since the scheme is part of the password's target, refuse the save
// with "enter the secret again" although the admin changed nothing.
func TestSystemSettingsUnchangedMQTTPartsKeepTheStoredURL(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.MQTT_URL": {"mqtt://broker.lan"}, "reuse.MQTT_PASSWORD": {"1"}})
	if r.StatusCode != 303 {
		t.Fatalf("setup: status %d %s", r.StatusCode, body)
	}
	r, body = e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}})
	if r.StatusCode != 303 {
		t.Fatalf("unchanged parts: status %d %s", r.StatusCode, body)
	}
	if got := svc.Snapshot(); got.MQTTURL != "mqtt://broker.lan" || got.MQTTPassword != "fake-stored-pw" {
		t.Errorf("MQTT_URL %q, password kept %v; want the stored URL and password untouched", got.MQTTURL, got.MQTTPassword == "fake-stored-pw")
	}
	// A real change still composes the new URL.
	r, _ = e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "mqtt.host": {"broker.lan"}, "mqtt.port": {"1884"}, "reuse.MQTT_PASSWORD": {"1"}})
	if r.StatusCode != 303 || svc.Snapshot().MQTTURL != "tcp://broker.lan:1884" {
		t.Errorf("changed port: status %d, MQTT_URL %q", r.StatusCode, svc.Snapshot().MQTTURL)
	}
}

// Why (#44): the connection test used to post the whole form and reload the page. A secret is
// never rendered back, so the password the admin had typed was blank afterwards; the Save that
// followed read that as "keep the stored one" and the bus kept connecting with the old password
// although the typed one had just passed the test. The test button must therefore swap only its
// own row, leaving the form (and the typed password) untouched.
func TestSystemSettingsMQTTTestKeepsTheTypedPasswordInThePage(t *testing.T) {
	e, _, _, _ := sysEnvTesters(t, true, func(context.Context, sysconfig.MQTTParams) error { return nil }, nil, nil)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(page, `hx-post="/tf/settings/system/mqtt-test?tab=mqtt"`) || !strings.Contains(page, `hx-include="#sysform"`) {
		t.Fatal("the test button must post the form with htmx and swap only its row")
	}
	r, body := e.do(t, "POST", "/settings/system/mqtt-test", "admin", url.Values{"csrf": {"tok"}, "mqtt.host": {"broker.lan"}, "v.MQTT_PASSWORD": {"typed-pw"}}, "HX-Request", "true")
	if r.StatusCode != 200 || !strings.Contains(body, "connected") {
		t.Fatalf("status %d %s", r.StatusCode, body)
	}
	if strings.Contains(body, "<form") || strings.Contains(body, "<html") || strings.Contains(body, "typed-pw") {
		t.Fatalf("an htmx test must answer with the test row only: %s", body)
	}
}
