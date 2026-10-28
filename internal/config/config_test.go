package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// base is a minimal valid header-mode environment.
func base(dir string, extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin", // unrelated variables are ignored
		"TASMOFLEET_DATA_DIR=" + dir,
		"TASMOFLEET_AUTH_MODE=header",
		"TASMOFLEET_TRUSTED_PROXIES=172.31.12.20/32",
		"TASMOFLEET_DEVICE_NETWORKS=172.31.13.0/24, 172.31.23.0/24",
		"TASMOFLEET_OPERATOR_GROUPS=Tasmofleet Users",
	}, extra...)
}

func TestDefaultsApply(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.FWListen != ":80" || c.OTAParallel != 2 || c.TelePeriod != 300*time.Second {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if len(c.DeviceNetworks) != 0 {
		t.Fatalf("an operational setting in the environment must not be applied: %v", c.DeviceNetworks)
	}
	if !c.LocalLogin {
		t.Fatal("break-glass login must default to enabled")
	}
}

// The environment must win over the file so a container env can override
// a mounted config file without editing it.
func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	file := "TASMOFLEET_LISTEN=:9\nLOG_LEVEL='debug'\n# comment\n"
	if err := os.WriteFile(filepath.Join(dir, "tasmofleet.env"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(base(dir, "TASMOFLEET_LISTEN=:3"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":3" {
		t.Fatalf("env must win: got %s", c.Listen)
	}
	if c.LogLevel != "debug" {
		t.Fatalf("file value (unprefixed, quoted) not read: %q", c.LogLevel)
	}
}

// Docker secrets: *_FILE works for secret fields, and setting both is an
// error rather than a silent preference.
func TestSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	sec := filepath.Join(dir, "mqtt")
	if err := os.WriteFile(sec, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(base(dir, "TASMOFLEET_METRICS_TOKEN_FILE="+sec))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsToken != "s3cret" {
		t.Fatalf("got %q", c.MetricsToken)
	}
	_, err = Load(base(dir, "TASMOFLEET_METRICS_TOKEN_FILE="+sec, "TASMOFLEET_METRICS_TOKEN=x"))
	if err == nil {
		t.Fatal("both value and _FILE set must fail")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string // substring of the error naming the key
	}{
		// Without device networks the SSRF allowlist would be empty or open.
		// Header auth without trusted proxies would trust any client's headers.
		{"header without proxies", []string{"TASMOFLEET_TRUSTED_PROXIES="}, "TRUSTED_PROXIES"},
		// OIDC needs PUBLIC_URL because redirect_uri must not come from request headers.
		{"oidc without public url", []string{"TASMOFLEET_AUTH_MODE=oidc", "TASMOFLEET_OIDC_ISSUER=https://idp", "TASMOFLEET_OIDC_CLIENT_ID=x"}, "PUBLIC_URL"},
		{"unknown auth mode", []string{"TASMOFLEET_AUTH_MODE=none"}, "AUTH_MODE"},
		{"base path trailing slash", []string{"TASMOFLEET_BASE_PATH=/x/"}, "BASE_PATH"},
		// Group modes must not silently admit every authenticated IdP user.
		{"header without groups", []string{"TASMOFLEET_OPERATOR_GROUPS="}, "ADMIN_GROUPS"},
		// Without proxy or TLS, cookies and passwords would cross the LAN in clear text unless the operator opts in.
		{"half tls pair", []string{"TASMOFLEET_TLS_CERT_FILE=/x.pem"}, "TLS_CERT_FILE"},
		{"session max below idle", []string{"TASMOFLEET_SESSION_IDLE=3h", "TASMOFLEET_SESSION_MAX=1h"}, "SESSION_MAX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty value means "not set", so a case that removes a base
			// setting ("KEY=") drops it from the base environment.
			var env []string
			for _, kv := range base(t.TempDir()) {
				k, _, _ := strings.Cut(kv, "=")
				if !slices.Contains(tc.env, k+"=") {
					env = append(env, kv)
				}
			}
			_, err := Load(append(env, tc.env...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error naming %s, got %v", tc.want, err)
			}
		})
	}
}

func TestExplicitMissingFileFails(t *testing.T) {
	_, err := Load(base(t.TempDir(), "TASMOFLEET_CONFIG_FILE=/nonexistent/x.env"))
	if err == nil {
		t.Fatal("an explicitly named config file that is missing must fail")
	}
}

// Standalone mode: no proxy, no IdP, local accounts only, plain HTTP
// explicitly accepted for a trusted LAN.
func TestStandaloneLocalMode(t *testing.T) {
	c, err := Load([]string{
		"TASMOFLEET_DATA_DIR=" + t.TempDir(),
		"TASMOFLEET_AUTH_MODE=local",
		"TASMOFLEET_DEVICE_NETWORKS=192.168.1.0/24",
		"TASMOFLEET_ALLOW_INSECURE_HTTP=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "" || c.SessionIdle != 2*time.Hour || c.SessionMax != 12*time.Hour {
		t.Fatalf("unexpected: %+v", c)
	}
}

// Why: cookies must be Secure when the listener serves TLS; an http:// PUBLIC_URL next to TLS would
// make the session cookie plain.
func TestPublicURLHTTPWithTLSRejected(t *testing.T) {
	_, err := Load(base(t.TempDir(), "TASMOFLEET_PUBLIC_URL=http://x.example", "TASMOFLEET_TLS_CERT_FILE=/c.pem", "TASMOFLEET_TLS_KEY_FILE=/k.pem"))
	if err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
		t.Fatalf("got %v", err)
	}
}

func TestSecurityDefaults(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsDeviceLabels || c.FWAllowSingleSource || c.FWUpstreamOverride != "" || c.BackupMaxAge != 720*time.Hour {
		t.Fatalf("unsafe defaults: %+v", c)
	}
}

// Spec 8.1: the index is refreshed every 6 h unless the operator says otherwise.
func TestFirmwareRefreshIntervalDefault(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.FWRefreshInterval != 6*time.Hour {
		t.Fatalf("default = %v, want 6h", c.FWRefreshInterval)
	}
}

// Why: the direct admin listener was removed (2026-10-19). An operator who still
// has its variables set must be told at start that they do nothing any more,
// but the server must start: a crash would lock the operator out of the data.
func TestRemovedAdminListenerKeysWarnButDoNotFail(t *testing.T) {
	env := []string{"TASMOFLEET_ADMIN_LISTEN=:8443", "TASMOFLEET_ADMIN_NETWORKS=172.31.11.0/24", "TASMOFLEET_ADMIN_TLS_SANS=10.0.0.1"}
	c, err := Load(base(t.TempDir(), env...))
	if err != nil {
		t.Fatalf("removed keys must not stop the start: %v", err)
	}
	if len(c.Warnings) != 3 {
		t.Fatalf("warnings = %v, want one per removed key", c.Warnings)
	}
	for i, key := range []string{"ADMIN_LISTEN", "ADMIN_NETWORKS", "ADMIN_TLS_SANS"} {
		if !strings.Contains(c.Warnings[i], "TASMOFLEET_"+key) {
			t.Errorf("warning %d does not name %s: %q", i, key, c.Warnings[i])
		}
	}
	if c, err := Load(base(t.TempDir())); err != nil || len(c.Warnings) != 0 {
		t.Errorf("clean config: warnings %v, err %v", c.Warnings, err)
	}
}

// Why: a fresh container must work with no configuration at all and still not
// send passwords in clear text: without proxy, certificate or opt-in it serves a
// self-signed certificate (and the HTTPS port by default).
func TestZeroConfigurationServesSelfSignedTLSWithLocalLogin(t *testing.T) {
	c, err := Load([]string{"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null"})
	if err != nil {
		t.Fatalf("zero configuration must load: %v", err)
	}
	if c.AuthMode != "local" || !c.TLSSelfSigned || !c.AutoTLS || c.Listen != ":8443" || c.FWListen != ":80" || !c.LocalLogin {
		t.Fatalf("%+v", c)
	}
	// Explicit choices are respected.
	c, err = Load([]string{"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_LISTEN=:9000", "TASMOFLEET_ALLOW_INSECURE_HTTP=true"})
	if err != nil || c.TLSSelfSigned || c.AutoTLS || c.Listen != ":9000" {
		t.Fatalf("insecure opt-in: %+v %v", c, err)
	}
	c, err = Load([]string{"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_TRUSTED_PROXIES=10.0.0.1/32", "TASMOFLEET_AUTH_MODE=header", "TASMOFLEET_OPERATOR_GROUPS=ops"})
	if err != nil || c.TLSSelfSigned || c.Listen != ":8080" {
		t.Fatalf("behind a proxy nothing changes: %+v %v", c, err)
	}
}

// Why: the sign-in settings are editable on the settings page unless the
// operator set them in the environment, which locks them.
func TestSignInSettingsAreLockedOnlyWhenTheEnvironmentSetsThem(t *testing.T) {
	c, err := Load(base(t.TempDir(), "TASMOFLEET_OIDC_CLIENT_ID=abc"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLocked("AUTH_MODE") || !c.IsLocked("OIDC_CLIENT_ID") || !c.IsLocked("OPERATOR_GROUPS") || c.IsLocked("OIDC_ISSUER") {
		t.Fatalf("locked: %v", c.Locked)
	}
	got, skipped := Overlay(c, map[string]string{"OIDC_CLIENT_ID": "other", "OIDC_ISSUER": "https://idp.example/"})
	if got.OIDCClientID != "abc" || got.OIDCIssuer != "https://idp.example/" || skipped["OIDC_CLIENT_ID"] == nil {
		t.Fatalf("id=%s issuer=%s skipped=%v", got.OIDCClientID, got.OIDCIssuer, skipped)
	}
}

// Why (secrets only to their bound target): if the operator fixes the client secret,
// the issuer it is sent to is fixed too, and the other way round.
func TestIssuerAndClientSecretLockTogether(t *testing.T) {
	for _, env := range []string{"TASMOFLEET_OIDC_CLIENT_SECRET=x", "TASMOFLEET_OIDC_ISSUER=https://idp.example/"} {
		c, err := Load(base(t.TempDir(), env))
		if err != nil {
			t.Fatal(err)
		}
		if !c.IsLocked("OIDC_ISSUER") || !c.IsLocked("OIDC_CLIENT_SECRET") || c.IsLocked("OIDC_CLIENT_ID") {
			t.Fatalf("%s: %v", env, c.Locked)
		}
	}
}

// Why: the login button must name any provider the operator configures and
// read "Single sign-on" otherwise; the name is a sign-in setting, so the
// environment locks it like the other ones.
func TestOIDCDisplayNameDefaultsAndLocks(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil || c.OIDCDisplayName != "Single sign-on" || c.Locked["OIDC_DISPLAY_NAME"] {
		t.Fatalf("default: %q locked=%v err=%v", c.OIDCDisplayName, c.Locked["OIDC_DISPLAY_NAME"], err)
	}
	c, err = Load(base(t.TempDir(), "TASMOFLEET_OIDC_DISPLAY_NAME=Acme ID"))
	if err != nil || c.OIDCDisplayName != "Acme ID" || !c.Locked["OIDC_DISPLAY_NAME"] {
		t.Fatalf("custom: %q locked=%v err=%v", c.OIDCDisplayName, c.Locked["OIDC_DISPLAY_NAME"], err)
	}
}

// Why (N17): LOG_LEVEL is a page setting that the environment locks. An
// operator who set it in the container must keep it (the page cannot quietly
// turn debug logging off or on), one who did not can change it in the UI; and a
// typo must stop the start instead of silently logging at another level.
func TestLogLevelLocksAndValidates(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" || c.IsLocked("LOG_LEVEL") {
		t.Fatalf("unset: level %q locked %v, want info and editable", c.LogLevel, c.IsLocked("LOG_LEVEL"))
	}
	c, err = Load(base(t.TempDir(), "TASMOFLEET_LOG_LEVEL=DEBUG"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLocked("LOG_LEVEL") || c.Legacy["LOG_LEVEL"] != "" {
		t.Fatalf("env LOG_LEVEL must lock the field, not be imported: %+v %v", c.Locked, c.Legacy)
	}
	if _, err := Load(base(t.TempDir(), "TASMOFLEET_LOG_LEVEL=trace")); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("unknown level must fail the start: %v", err)
	}
}

// Why (N19, N20): the /metrics switch defaults to on so an upgrade keeps
// scrapes working; the default device password defaults to none.
func TestMetricsEnabledAndDefaultPasswordDefaults(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if !c.MetricsEnabled || c.DeviceDefaultPassword != "" {
		t.Fatalf("metrics %v, default password set %v", c.MetricsEnabled, c.DeviceDefaultPassword != "")
	}
}

// Unraid passes unfilled template fields as "KEY=" (issue #148). An empty or
// blank value must behave exactly like an unset one for every key: the default
// applies, nothing is locked and nothing is imported as legacy. Otherwise a
// default CA install crash-loops on LOG_LEVEL, and an empty sign-in field would
// lock the page's field to nothing.
func TestEmptyEnvironmentValueCountsAsNotSet(t *testing.T) {
	dir := t.TempDir()
	want, err := Load([]string{"TASMOFLEET_DATA_DIR=" + dir})
	if err != nil {
		t.Fatal(err)
	}
	rt := reflect.TypeOf(Config{})
	checked := 0
	for i := range rt.NumField() {
		f := rt.Field(i)
		key := f.Tag.Get("env")
		if key == "" {
			continue
		}
		names := []string{key}
		if f.Tag.Get("secret") == "true" {
			names = append(names, key+"_FILE") // an empty file path is not set either
		}
		for _, name := range names {
			for _, blank := range []string{"", "   "} {
				t.Run(name+"="+strings.ReplaceAll(blank, " ", "_"), func(t *testing.T) {
					got, err := Load([]string{"TASMOFLEET_DATA_DIR=" + dir, "TASMOFLEET_" + name + "=" + blank})
					if err != nil {
						t.Fatalf("a blank value must not fail the start: %v", err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("blank value changed the config (default, Locked or Legacy):\n got %+v\nwant %+v", got, want)
					}
				})
			}
		}
		checked++
	}
	if checked < 30 {
		t.Fatalf("only %d keys found: the struct walk is broken", checked)
	}
}

// A blank environment value must not shadow a real value from the env file
// either: "not set" means the next source decides.
func TestEmptyEnvironmentValueDoesNotShadowFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tasmofleet.env"), []byte("TASMOFLEET_LOG_LEVEL=debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load([]string{"TASMOFLEET_DATA_DIR=" + dir, "TASMOFLEET_LOG_LEVEL="})
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" || !c.Locked["LOG_LEVEL"] {
		t.Fatalf("file value must apply: level %q, locked %v", c.LogLevel, c.Locked["LOG_LEVEL"])
	}
}
