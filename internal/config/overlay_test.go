package config

import (
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// Why: operational settings belong to the system settings page. A value that is
// still in the environment must not configure anything any more (the database is
// the single source); it is only kept so it can be imported once.
func TestOperationalEnvironmentValuesAreLegacyNotConfiguration(t *testing.T) {
	dir := t.TempDir()
	sec := dir + "/mqtt"
	if err := os.WriteFile(sec, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(base(dir, "TASMOFLEET_OTA_PARALLEL=9", "TASMOFLEET_MQTT_URL=tcp://b:1883", "TASMOFLEET_MQTT_PASSWORD_FILE="+sec,
		"TASMOFLEET_POLICY_RULES=[]", "TASMOFLEET_FIRMWARE_KEEP_RELEASES=3"))
	if err != nil {
		t.Fatal(err)
	}
	if c.OTAParallel != 2 || c.MQTTURL != "" || c.MQTTPassword != "" {
		t.Fatalf("applied from the environment: parallel=%d url=%q", c.OTAParallel, c.MQTTURL)
	}
	want := map[string]string{
		"OTA_PARALLEL": "9", "MQTT_URL": "tcp://b:1883", "MQTT_PASSWORD": "from-file", "POLICY_RULES": "[]", "FIRMWARE_KEEP_RELEASES": "3",
		"DEVICE_NETWORKS": "172.31.13.0/24, 172.31.23.0/24", "OPERATOR_GROUPS": "",
	}
	for k, v := range want {
		if k == "OPERATOR_GROUPS" {
			if _, ok := c.Legacy[k]; ok {
				t.Errorf("bootstrap key %s must not be legacy", k)
			}
			continue
		}
		if strings.TrimSpace(c.Legacy[k]) != strings.TrimSpace(v) {
			t.Errorf("Legacy[%s] = %q, want %q", k, c.Legacy[k], v)
		}
	}
	// A garbage operational value does not stop the start; the import reports it.
	if _, err := Load(base(t.TempDir(), "TASMOFLEET_OTA_PARALLEL=lots")); err != nil {
		t.Fatalf("legacy values are checked at import, not at load: %v", err)
	}
}

func TestEveryBootstrapKeyStillComesFromTheEnvironment(t *testing.T) {
	c, err := Load(base(t.TempDir(), "TASMOFLEET_LISTEN=:1234", "TASMOFLEET_FIRMWARE_LISTEN=:81", "TASMOFLEET_METRICS_TOKEN=t"))
	if err != nil || c.Listen != ":1234" || c.FWListen != ":81" || c.MetricsToken != "t" {
		t.Fatalf("%+v %v", c, err)
	}
}

// Why: precedence is now stored > default. Stored values are applied through
// the same parsers and validators; bootstrap keys can never be overlaid.
func TestOverlayAppliesOnlyOperationalKeys(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	got, skipped := Overlay(c, map[string]string{"BACKUP_KEEP": "4", "AUTH_MODE": "local", "LISTEN": ":1", "NO_SUCH_KEY": "x"})
	if got.BackupKeep != 4 || got.AuthMode != "header" || got.Listen != ":8080" {
		t.Fatalf("keep=%d auth=%s listen=%s", got.BackupKeep, got.AuthMode, got.Listen)
	}
	for _, k := range []string{"AUTH_MODE", "LISTEN", "NO_SUCH_KEY"} {
		if skipped[k] == nil {
			t.Errorf("%s must be skipped", k)
		}
	}
}

// Why: a stored value that no longer validates must not stop the start; it is
// skipped and reported.
func TestOverlaySkipsValuesThatNoLongerValidate(t *testing.T) {
	c, _ := Load(base(t.TempDir()))
	got, skipped := Overlay(c, map[string]string{"BACKUP_KEEP": "0", "OTA_PARALLEL": "4"})
	if got.BackupKeep != 10 || got.OTAParallel != 4 {
		t.Errorf("good values apply, bad ones do not: keep=%d parallel=%d", got.BackupKeep, got.OTAParallel)
	}
	if skipped["BACKUP_KEEP"] == nil {
		t.Errorf("skipped = %v", skipped)
	}
}

// Why: values that only validate together (a scan needs its networks) must
// survive a restart whatever their alphabetical order.
func TestOverlayAppliesValuesThatDependOnEachOther(t *testing.T) {
	c, _ := Load(base(t.TempDir()))
	got, skipped := Overlay(c, map[string]string{
		"DISCOVERY_SCAN": "true", "DISCOVERY_SCAN_CIDRS": "172.31.13.0/25", "DEVICE_NETWORKS": "172.31.13.0/24",
	})
	if !got.DiscoveryScan || len(got.DiscoveryScanCIDRs) != 1 || len(skipped) != 0 {
		t.Fatalf("scan=%v cidrs=%v skipped=%v", got.DiscoveryScan, got.DiscoveryScanCIDRs, skipped)
	}
}

// Why: the UI and a legacy import use the same parsers and validators.
func TestValidationRejectsBadOperationalValues(t *testing.T) {
	cases := []struct{ key, val string }{
		{"OTA_PARALLEL", "0"}, {"OTA_PARALLEL", "x"}, {"BACKUP_KEEP", "0"},
		{"DEVICE_NETWORKS", "not-a-cidr"}, {"FIRMWARE_REFRESH_INTERVAL", "-1h"}, {"FIRMWARE_KEEP_RELEASES", "-1"}, {"FIRMWARE_SOURCES", "evil.example"},
		{"FIRMWARE_BASE_URL", "https://fw.example"},
		{"MQTT_URL", "ftp://broker"}, {"MQTT_URL", "ws://broker:80"}, {"MQTT_URL", "wss://broker:443"}, {"MQTT_URL", "tcp://"}, {"POLL_INTERVAL", "500ms"},
		{"DISCOVERY_SCAN_INTERVAL", "10s"}, {"BACKUP_INTERVAL", "5s"},
		{"SNAPSHOT_INTERVAL", "30m"}, {"SNAPSHOT_KEEP", "0"}, // a snapshot every few minutes would fill the disk; keeping none is no snapshot
		{"CR_WARN_PERCENT", "49"}, {"CR_WARN_PERCENT", "101"}, {"CR_WARN_PERCENT", "x"},
	}
	for _, tc := range cases {
		c, _ := Load(base(t.TempDir()))
		err := Set(&c, tc.key, tc.val)
		if err == nil {
			err = Validate(c)
		}
		if KeyErrors(err)[tc.key] == "" {
			t.Errorf("%s=%q: err = %v", tc.key, tc.val, err)
		}
	}
}

func TestFormatRoundTripsEveryKey(t *testing.T) {
	c, _ := Load(base(t.TempDir(), "TASMOFLEET_TRUSTED_PROXIES=172.31.11.0/24"))
	if err := Set(&c, "FIRMWARE_SOURCES", "github"); err != nil {
		t.Fatal(err)
	}
	if err := Set(&c, "DISCOVERY_SCAN_CIDRS", "172.31.13.0/25"); err != nil {
		t.Fatal(err)
	}
	for _, k := range Keys() {
		s, err := Format(c, k.Key)
		if err != nil {
			t.Fatalf("%s: %v", k.Key, err)
		}
		var d Config
		if err := Set(&d, k.Key, s); err != nil {
			t.Fatalf("%s: re-parsing %q: %v", k.Key, s, err)
		}
		if again, _ := Format(d, k.Key); again != s {
			t.Errorf("%s: %q -> %q", k.Key, s, again)
		}
	}
}

func TestKeysMarkSecretsAndOperationalSettings(t *testing.T) {
	info := map[string]KeyInfo{}
	for _, k := range Keys() {
		info[k.Key] = k
	}
	for _, k := range []string{"MQTT_PASSWORD", "OIDC_CLIENT_SECRET", "METRICS_TOKEN", "INITIAL_ADMIN_PASSWORD"} {
		if !info[k].Secret {
			t.Errorf("%s must be marked secret", k)
		}
	}
	// The coordinator's bootstrap list: these stay environment-only.
	for _, k := range []string{"DATA_DIR", "LISTEN", "PUBLIC_URL", "BASE_PATH", "TRUSTED_PROXIES", "LOCAL_LOGIN",
		"TLS_CERT_FILE", "INITIAL_ADMIN_PASSWORD", "FIRMWARE_LISTEN"} {
		if info[k].UI {
			t.Errorf("%s is bootstrap and must not be an operational setting", k)
		}
	}
	for _, k := range []string{"AUTH_MODE", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_DISPLAY_NAME", "ADMIN_GROUPS", "OPERATOR_GROUPS", "METRICS_TOKEN"} {
		if !info[k].UI || !info[k].Lock {
			t.Errorf("%s must be an editable sign-in setting (locked by the environment)", k)
		}
	}
	for _, k := range []string{"DEVICE_NETWORKS", "FIRMWARE_BASE_URL", "FIRMWARE_SOURCES", "MQTT_URL", "MQTT_PASSWORD", "OTA_PARALLEL", "BACKUP_KEEP",
		"POLL_INTERVAL", "DISCOVERY_SCAN", "METRICS_DEVICE_LABELS", "AUDIT_RETENTION"} {
		if !info[k].UI {
			t.Errorf("%s must be an operational setting", k)
		}
	}
}

func TestDiscoveryDefaultsKeepTodaysBehaviour(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if !c.DiscoveryMQTT || c.DiscoveryScan || c.DiscoveryScanInterval != 24*time.Hour {
		t.Fatalf("MQTT discovery stays on, the scheduled scan off: %+v", c)
	}
}

// Why: a scheduled scan may only ever probe addresses inside DEVICE_NETWORKS.
func TestScheduledScanCIDRsMustLieInsideDeviceNetworks(t *testing.T) {
	c, _ := Load(base(t.TempDir()))
	_ = Set(&c, "DEVICE_NETWORKS", "172.31.13.0/24")
	_ = Set(&c, "DISCOVERY_SCAN", "true")
	_ = Set(&c, "DISCOVERY_SCAN_CIDRS", "10.0.0.0/24")
	if KeyErrors(Validate(c))["DISCOVERY_SCAN_CIDRS"] == "" {
		t.Fatal("outside the device networks must be refused")
	}
	_ = Set(&c, "DISCOVERY_SCAN_CIDRS", "172.31.13.0/24")
	if err := Validate(c); err != nil || c.DiscoveryScanCIDRs[0] != netip.MustParsePrefix("172.31.13.0/24") {
		t.Fatalf("%v", err)
	}
	_ = Set(&c, "DISCOVERY_SCAN_CIDRS", "")
	if KeyErrors(Validate(c))["DISCOVERY_SCAN_CIDRS"] == "" {
		t.Fatal("an enabled scan without CIDRs must be refused")
	}
}

func TestKeyErrorsUnwrapsJoinedErrors(t *testing.T) {
	err := errors.Join(&KeyError{Key: "A", Err: errors.New("bad a")}, &KeyError{Key: "B", Err: errors.New("bad b")})
	m := KeyErrors(err)
	if m["A"] != "bad a" || m["B"] != "bad b" || len(m) != 2 {
		t.Fatalf("m = %v", m)
	}
	if !strings.Contains(err.Error(), "TASMOFLEET_A: bad a") {
		t.Fatalf("text must keep the env spelling: %v", err)
	}
}

// Why: "6h0m0s" in the settings is noise; the short form must parse back to the same duration.
func TestShortDuration(t *testing.T) {
	for _, d := range []time.Duration{6 * time.Hour, 90 * time.Minute, 10 * time.Second, 300 * time.Second, 720 * time.Hour, time.Hour + time.Second, 0} {
		s := shortDuration(d)
		back, err := time.ParseDuration(s)
		if err != nil || back != d {
			t.Errorf("%v -> %q -> %v (%v)", d, s, back, err)
		}
	}
	if got := shortDuration(6 * time.Hour); got != "6h" {
		t.Errorf("6h is shown as %q", got)
	}
	if got := shortDuration(10 * time.Second); got != "10s" {
		t.Errorf("10s is shown as %q", got)
	}
}

// Why (#289): the Identity card warns when the settings text area is fuller than this; below
// 50 % it would warn on healthy devices, above 100 % never, so the range is enforced and a
// device page that reads the default sees 90.
func TestCRWarnPercentDefaultAndRange(t *testing.T) {
	c, err := Load(base(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if c.CRWarnPercent != 90 {
		t.Fatalf("default = %d, want 90", c.CRWarnPercent)
	}
	for _, v := range []string{"50", "100"} {
		c, _ := Load(base(t.TempDir()))
		if err := Set(&c, "CR_WARN_PERCENT", v); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if err := Validate(c); err != nil {
			t.Fatalf("%s must be valid: %v", v, err)
		}
	}
}
