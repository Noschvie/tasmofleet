package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bare(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null"}, extra...)
}

// Why (#152): the container port of the UI is :8080 whatever the TLS mode; the
// old :8443 switch made the port mapping depend on the mode.
func TestUIPortIsFixedInEveryMode(t *testing.T) {
	for name, extra := range map[string][]string{
		"zero configuration (self-signed TLS)": nil,
		"trusted proxies (plain HTTP)":         {"TASMOFLEET_TRUSTED_PROXIES=10.0.0.1/32"},
		"own certificate":                      {"TASMOFLEET_TLS_CERT_FILE=/c.pem", "TASMOFLEET_TLS_KEY_FILE=/k.pem"},
		"insecure opt-in":                      {"TASMOFLEET_ALLOW_INSECURE_HTTP=true"},
	} {
		c, err := Load(bare(t, extra...))
		if err != nil || c.Listen != ":8080" || c.FWListen != ":80" {
			t.Errorf("%s: listen %q firmware %q err %v", name, c.Listen, c.FWListen, err)
		}
	}
}

// Why (#152): how the transport is chosen, from the issue's table.
func TestTLSFollowsTheModeNotAVariable(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		tls   bool // the listener serves TLS (certificate or self-signed)
		auto  bool
	}{
		{"nothing set", nil, true, true},
		{"trusted proxies", []string{"TASMOFLEET_TRUSTED_PROXIES=10.0.0.1/32"}, false, false},
		{"own certificate", []string{"TASMOFLEET_TLS_CERT_FILE=/c.pem", "TASMOFLEET_TLS_KEY_FILE=/k.pem"}, true, false},
		{"insecure opt-in", []string{"TASMOFLEET_ALLOW_INSECURE_HTTP=true"}, false, false},
	}
	for _, tc := range cases {
		c, err := Load(bare(t, tc.extra...))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := c.TLSCertFile != "" || c.TLSSelfSigned; got != tc.tls || c.AutoTLS != tc.auto {
			t.Errorf("%s: tls=%v auto=%v, want tls=%v auto=%v", tc.name, got, c.AutoTLS, tc.tls, tc.auto)
		}
		if len(c.Warnings) != 0 {
			t.Errorf("%s: unexpected warnings %v", tc.name, c.Warnings)
		}
	}
}

// Why (hard requirement for 0.17.0): Unraid keeps the user's saved template; a
// changed Community Applications template does not rewrite saved values. An
// install saved from a 0.16.x template must keep a reachable UI after pulling
// :latest without editing anything. These are the exact variable sets of the
// 0.16.x templates (empty fields are passed as "KEY=" by Unraid).
func TestSavedTemplatesFrom016KeepTheirListenerAndScheme(t *testing.T) {
	caDefault := []string{ // CA template defaults: port 8443 -> 8443, self-signed TLS
		"TASMOFLEET_LISTEN=:8443", "TASMOFLEET_TLS_SELF_SIGNED=true", "TASMOFLEET_FIRMWARE_LISTEN=:80",
		"TASMOFLEET_PUBLIC_URL=", "TASMOFLEET_TRUSTED_PROXIES=", "TASMOFLEET_INITIAL_ADMIN_PASSWORD=",
		"TASMOFLEET_METRICS_TOKEN=", "TASMOFLEET_LOG_LEVEL=",
	}
	caProxy := []string{ // CA template as its own description tells to switch it behind a proxy
		"TASMOFLEET_LISTEN=:8080", "TASMOFLEET_TLS_SELF_SIGNED=false", "TASMOFLEET_FIRMWARE_LISTEN=:80",
		"TASMOFLEET_PUBLIC_URL=https://tasmofleet.example.com", "TASMOFLEET_TRUSTED_PROXIES=192.168.1.10/32",
		"TASMOFLEET_INITIAL_ADMIN_PASSWORD=", "TASMOFLEET_METRICS_TOKEN=", "TASMOFLEET_LOG_LEVEL=",
	}
	repoExample := []string{ // deploy/unraid/tasmofleet.xml of 0.16.x (no TLS variable at all)
		"TASMOFLEET_PUBLIC_URL=https://tasmofleet.example.com", "TASMOFLEET_FIRMWARE_LISTEN=:80", "TASMOFLEET_LISTEN=:8080",
		"TASMOFLEET_TRUSTED_PROXIES=172.31.12.20/32", "TASMOFLEET_INITIAL_ADMIN_PASSWORD=", "TASMOFLEET_METRICS_TOKEN=", "TASMOFLEET_LOG_LEVEL=",
	}
	cases := []struct {
		name     string
		env      []string
		listen   string
		tls      bool
		warnings int // 0 or 1: a still-set TLS_SELF_SIGNED is reported once
	}{
		{"CA default, untouched", caDefault, ":8443", true, 1},
		{"CA template switched to a proxy", caProxy, ":8080", false, 1},
		{"repo example template", repoExample, ":8080", false, 0},
		// Before 0.17 an explicit false without proxies still ended in self-signed TLS on LISTEN.
		{"CA default with TLS flag false by hand", append(append([]string{}, caDefault[:1]...), "TASMOFLEET_TLS_SELF_SIGNED=false"), ":8443", true, 1},
	}
	for _, tc := range cases {
		c, err := Load(bare(t, tc.env...))
		if err != nil {
			t.Fatalf("%s: does not start: %v", tc.name, err)
		}
		if c.Listen != tc.listen || (c.TLSCertFile != "" || c.TLSSelfSigned) != tc.tls || c.FWListen != ":80" {
			t.Errorf("%s: listen %q tls %v firmware %q, want %q tls %v", tc.name, c.Listen, c.TLSSelfSigned, c.FWListen, tc.listen, tc.tls)
		}
		if len(c.Warnings) != tc.warnings {
			t.Errorf("%s: warnings %v, want %d", tc.name, c.Warnings, tc.warnings)
		}
	}
}

// Why: TLS_SELF_SIGNED is gone. The one saved combination whose scheme changes is
// "TLS_SELF_SIGNED=true together with trusted proxies" (the listener served TLS
// before, now the proxy mode means plain HTTP). The start warning must say so
// precisely, because a proxy that talks https to it needs one changed setting.
func TestRemovedTLSSelfSignedWarnsWhenTheSchemeChanges(t *testing.T) {
	c, err := Load(bare(t, "TASMOFLEET_TLS_SELF_SIGNED=true", "TASMOFLEET_TRUSTED_PROXIES=10.0.0.1/32", "TASMOFLEET_LISTEN=:8443"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSSelfSigned || c.TLSCertFile != "" {
		t.Fatalf("trusted proxies mean plain HTTP: %+v", c)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "TASMOFLEET_TLS_SELF_SIGNED") ||
		!strings.Contains(c.Warnings[0], "plain HTTP") || !strings.Contains(c.Warnings[0], "http://") {
		t.Fatalf("warnings: %v", c.Warnings)
	}
	// Unchanged behaviour (TLS before and after): the warning is only a clean-up hint.
	c, err = Load(bare(t, "TASMOFLEET_TLS_SELF_SIGNED=true"))
	if err != nil || len(c.Warnings) != 1 || strings.Contains(c.Warnings[0], "plain HTTP") {
		t.Fatalf("%v %v", c.Warnings, err)
	}
}

// Why (#148, WP5 review): an empty secret file is "not set", like an empty variable.
// It must not lock the field with an empty value or clash with the plain variable.
func TestEmptySecretFileCountsAsUnset(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	newline := filepath.Join(dir, "newline")
	for p, content := range map[string]string{empty: "", newline: "\n"} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{empty, newline} {
		c, err := Load(bare(t, "TASMOFLEET_OIDC_CLIENT_SECRET_FILE="+f))
		if err != nil || c.IsLocked("OIDC_CLIENT_SECRET") || c.OIDCClientSecret != "" {
			t.Errorf("%q: locked=%v secret=%q err=%v", f, c.IsLocked("OIDC_CLIENT_SECRET"), c.OIDCClientSecret, err)
		}
		c, err = Load(bare(t, "TASMOFLEET_INITIAL_ADMIN_PASSWORD_FILE="+f, "TASMOFLEET_INITIAL_ADMIN_PASSWORD=x"))
		if err != nil || c.InitialAdminPassword != "x" {
			t.Errorf("%q next to a value: %q %v", f, c.InitialAdminPassword, err)
		}
	}
	if _, err := Load(bare(t, "TASMOFLEET_OIDC_CLIENT_SECRET_FILE="+filepath.Join(dir, "missing"))); err == nil {
		t.Error("a _FILE that points nowhere is still an error")
	}
}
