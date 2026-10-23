package sysconfig

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type memStore struct {
	mu   sync.Mutex
	rows map[string]store.SystemSetting
	fail bool
}

// userRows are the stored settings without the import marker.
func (m *memStore) userRows() map[string]store.SystemSetting {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]store.SystemSetting{}
	for k, v := range m.rows {
		if k != importMarker {
			out[k] = v
		}
	}
	return out
}

func newMem() *memStore { return &memStore{rows: map[string]store.SystemSetting{}} }

func (m *memStore) LoadSystemSettings(context.Context) (map[string]store.SystemSetting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]store.SystemSetting{}
	for k, v := range m.rows {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) SaveSystemSettings(_ context.Context, actor string, ch []store.SystemSettingChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("disk full")
	}
	for _, c := range ch {
		if c.Delete {
			delete(m.rows, c.Key)
			continue
		}
		m.rows[c.Key] = store.SystemSetting{Key: c.Key, Value: c.Value, Secret: c.Secret, Target: c.Target, UpdatedBy: actor}
	}
	return nil
}

type memAudit struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (a *memAudit) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e)
}

func (a *memAudit) last() model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ev) == 0 {
		return model.AuditEvent{}
	}
	return a.ev[len(a.ev)-1]
}

func baseEnv(extra ...string) []string {
	return append([]string{
		"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null", // a fresh install: nothing else
	}, extra...)
}

// loadCfg loads the bootstrap configuration for a test; the device networks
// are set directly (they are an operational setting now, not an environment value).
func loadCfg(environ []string) (config.Config, error) {
	cfg, err := config.Load(environ)
	if err == nil && len(cfg.Legacy["DEVICE_NETWORKS"]) == 0 {
		cfg.DeviceNetworks = []netip.Prefix{netip.MustParsePrefix("172.31.13.0/24")}
	}
	return cfg, err
}

func newSvc(t *testing.T, st *memStore, env ...string) (*Service, *memAudit) {
	t.Helper()
	return newSvcEnv(t, st, baseEnv(env...))
}

func newSvcEnv(t *testing.T, st *memStore, environ []string) (*Service, *memAudit) {
	t.Helper()
	cfg, err := loadCfg(environ)
	if err != nil {
		t.Fatal(err)
	}
	aud := &memAudit{}
	s, err := New(context.Background(), Options{Base: cfg, Store: st, Audit: aud})
	if err != nil {
		t.Fatal(err)
	}
	return s, aud
}

var stepped = StepUp{Done: true, Possible: true}

// Why: precedence is stored > default. The environment is not a source for
// operational settings any more.
func TestStoredValueBeatsDefault(t *testing.T) {
	st := newMem()
	st.rows["BACKUP_KEEP"] = store.SystemSetting{Key: "BACKUP_KEEP", Value: "4"}
	s, _ := newSvc(t, st)
	if s.Snapshot().BackupKeep != 4 {
		t.Fatalf("keep=%d", s.Snapshot().BackupKeep)
	}
	sources := map[string]string{}
	for _, f := range s.State() {
		sources[f.Key] = f.Source
	}
	if sources["BACKUP_KEEP"] != SourceStored || sources["BACKUP_INTERVAL"] != SourceDefault {
		t.Fatalf("sources %v", sources)
	}
}

// Why: the live install still has operational values in its container
// configuration. They are imported once so that nothing is lost, then ignored.
func TestLegacyEnvironmentValuesAreImportedOnce(t *testing.T) {
	st := newMem()
	st.rows["BACKUP_KEEP"] = store.SystemSetting{Key: "BACKUP_KEEP", Value: "4"}
	env := []string{"TASMOFLEET_OTA_PARALLEL=5", "TASMOFLEET_BACKUP_KEEP=99", "TASMOFLEET_OTA_POWER_MAX_AGE=nonsense", "TASMOFLEET_MQTT_URL=tcp://bob:hunter2@broker.lan:1883"}
	s, aud := newSvc(t, st, env...)
	c := s.Snapshot()
	if c.OTAParallel != 5 {
		t.Fatalf("legacy value not imported: %d", c.OTAParallel)
	}
	if c.BackupKeep != 4 {
		t.Fatalf("an existing stored value must win over the environment: %d", c.BackupKeep)
	}
	if st.rows["OTA_PARALLEL"].Value != "5" || st.rows["OTA_PARALLEL"].UpdatedBy != "env-import" {
		t.Fatalf("not persisted: %+v", st.rows["OTA_PARALLEL"])
	}
	if _, ok := st.rows["OTA_POWER_MAX_AGE"]; ok || s.Skipped()["OTA_POWER_MAX_AGE"] == "" {
		t.Fatalf("an invalid value must be reported and not stored: skipped %v", s.Skipped())
	}
	// Credentials in a legacy URL move to the sealed password field.
	if c.MQTTURL != "tcp://broker.lan:1883" || c.MQTTUser != "bob" || c.MQTTPassword != "hunter2" {
		t.Fatalf("url %q user %q", c.MQTTURL, c.MQTTUser)
	}
	if r := st.rows["MQTT_PASSWORD"]; !r.Secret || r.Target != "tcp://broker.lan:1883" || strings.Contains(st.rows["MQTT_URL"].Value, "hunter2") {
		t.Fatalf("password row %+v url row %+v", r, st.rows["MQTT_URL"])
	}
	if e := aud.last(); e.Action != "sysconfig.env_import" || strings.Contains(e.Detail, "hunter2") || !strings.Contains(e.Detail, "OTA_PARALLEL") {
		t.Fatalf("audit %+v", e)
	}
	for _, f := range s.State() {
		if strings.Contains(f.Value, "hunter2") {
			t.Fatalf("%s leaks", f.Key)
		}
	}
	// After the import the environment is ignored, also after the admin removed the value.
	if _, err := s.Apply(context.Background(), Request{Actor: "a", Clear: []string{"OTA_PARALLEL"}}); err != nil {
		t.Fatal(err)
	}
	s2, _ := newSvc(t, st, "TASMOFLEET_OTA_PARALLEL=7")
	if s2.Snapshot().OTAParallel != 2 {
		t.Fatalf("a second start must not re-import: %d", s2.Snapshot().OTAParallel)
	}
}

// Why: a broker login given as its own environment variables (not embedded in the
// URL) was silently dropped on import, so MQTT connected without credentials. The
// sealed password must stay bound to the broker it was meant for.
func TestLegacyMQTTCredentialsAreImportedFromSeparateOrEmbeddedForms(t *testing.T) {
	cases := []struct {
		name string
		env  []string
	}{
		{"separate variables", []string{"TASMOFLEET_MQTT_URL=tcp://broker:1883", "TASMOFLEET_MQTT_USER=u", "TASMOFLEET_MQTT_PASSWORD=p"}},
		{"embedded in the URL", []string{"TASMOFLEET_MQTT_URL=tcp://u:p@broker:1883"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMem()
			s, _ := newSvc(t, st, tc.env...)
			c := s.Snapshot()
			if c.MQTTURL != "tcp://broker:1883" || c.MQTTUser != "u" || c.MQTTPassword != "p" {
				t.Fatalf("url %q user %q password set %v", c.MQTTURL, c.MQTTUser, c.MQTTPassword != "")
			}
			if st.rows["MQTT_USER"].Value != "u" {
				t.Fatalf("user row %+v", st.rows["MQTT_USER"])
			}
			if r := st.rows["MQTT_PASSWORD"]; !r.Secret || r.Target != "tcp://broker:1883" {
				t.Fatalf("password must be sealed and bound to the broker: %+v", r)
			}
		})
	}
}

func TestFreshInstallWithoutEnvironmentStartsAndWritesTheMarker(t *testing.T) {
	st := newMem()
	cfg, err := config.Load(baseEnv())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Options{Base: cfg, Store: st, Audit: &memAudit{}})
	if err != nil {
		t.Fatalf("no device networks must not stop the start: %v", err)
	}
	if len(s.Snapshot().DeviceNetworks) != 0 {
		t.Fatal("no networks yet")
	}
	if _, ok := st.rows[importMarker]; !ok {
		t.Fatal("the marker records that the environment was looked at")
	}
}

// Why: lesson from TasmoAdmin: a web session must never be able to weaken
// authentication or move listeners. Bootstrap keys are not editable even when
// the environment does not set them.
func TestBootstrapAndUnknownKeysAreRejected(t *testing.T) {
	for _, key := range []string{"TRUSTED_PROXIES", "PUBLIC_URL", "LISTEN", "ADMIN_LISTEN", "METRICS_TOKEN",
		"INITIAL_ADMIN_PASSWORD", "LOCAL_LOGIN", "DATA_DIR", "TLS_CERT_FILE", "NO_SUCH_KEY", "Legacy", "Locked", ""} {
		st := newMem()
		s, _ := newSvc(t, st)
		_, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped, Values: map[string]string{key: "x"}})
		if !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%q: err = %v", key, err)
		}
		if len(st.userRows()) != 0 {
			t.Errorf("%q: wrote %v", key, st.rows)
		}
	}
}

// Why: the UI uses the same validators as the environment (parity), and a
// rejected form writes nothing, not even its valid fields.
func TestInvalidValuesAreRejectedWithFieldErrors(t *testing.T) {
	st := newMem()
	s, _ := newSvc(t, st)
	_, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped,
		Values: map[string]string{"OTA_PARALLEL": "0", "BACKUP_KEEP": "5", "BACKUP_INTERVAL": "5s"}})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	if ve.Fields["OTA_PARALLEL"] == "" || ve.Fields["BACKUP_INTERVAL"] == "" || ve.Fields["BACKUP_KEEP"] != "" {
		t.Fatalf("fields = %v", ve.Fields)
	}
	if len(st.userRows()) != 0 {
		t.Fatalf("wrote %v", st.rows)
	}
}
func TestAuditRecordsOldAndNewButRedactsSecrets(t *testing.T) {
	st := newMem()
	s, aud := newSvc(t, st)
	ctx := context.Background()
	_, err := s.Apply(ctx, Request{Actor: "alice", StepUp: stepped, Via: "admin-listener", Values: map[string]string{
		"OTA_PARALLEL": "4", "MQTT_PASSWORD": "fake-secret-Zk3", "MQTT_USER": "tasmo",
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := aud.last()
	if e.Action != "sysconfig.update" || e.Actor != "alice" || e.Result != "ok" {
		t.Fatalf("event %+v", e)
	}
	if strings.Contains(e.Detail, "fake-secret-Zk3") {
		t.Fatalf("secret in audit detail: %s", e.Detail)
	}
	for _, want := range []string{`"key":"OTA_PARALLEL"`, `"old":"2"`, `"new":"4"`, `"key":"MQTT_PASSWORD"`, `"new":"***"`, `"via":"admin-listener"`} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail lacks %s: %s", want, e.Detail)
		}
	}
	if !st.rows["MQTT_PASSWORD"].Secret || st.rows["MQTT_PASSWORD"].Value != "fake-secret-Zk3" {
		t.Fatalf("the secret must be stored (sealed by the store) as a secret: %+v", st.rows["MQTT_PASSWORD"])
	}
	// A second save without a new password keeps the old one (write-only field).
	if _, err := s.Apply(ctx, Request{Actor: "alice", Values: map[string]string{"OTA_PARALLEL": "5"}}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().MQTTPassword != "fake-secret-Zk3" {
		t.Fatal("password lost")
	}
}

// Why: hot reload. A live setting reaches its subscriber in the same call,
// a restart setting only raises the restart flag.
func TestLiveChangeNotifiesSubscribersAndRestartChangesAreFlagged(t *testing.T) {
	s, _ := newSvc(t, newMem())
	var got []Event
	unsub := s.Subscribe(func(e Event) { got = append(got, e) })
	ctx := context.Background()
	res, err := s.Apply(ctx, Request{Actor: "a", Values: map[string]string{"BACKUP_INTERVAL": "6h", "FIRMWARE_REFRESH_INTERVAL": "12h"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].New.BackupSchedule != 6*time.Hour || got[0].Old.BackupSchedule != 0 {
		t.Fatalf("events %+v", got)
	}
	if !contains(got[0].Keys, "BACKUP_INTERVAL") {
		t.Fatalf("keys %v", got[0].Keys)
	}
	if !contains(res.Live, "BACKUP_INTERVAL") || !contains(res.RestartRequired, "FIRMWARE_REFRESH_INTERVAL") || contains(res.RestartRequired, "BACKUP_INTERVAL") {
		t.Fatalf("result %+v", res)
	}
	pending := map[string]bool{}
	for _, f := range s.State() {
		pending[f.Key] = f.PendingRestart
	}
	if !pending["FIRMWARE_REFRESH_INTERVAL"] || pending["BACKUP_INTERVAL"] {
		t.Fatalf("pending %v", pending)
	}
	// Setting it back to the value of the running process clears the flag.
	if _, err := s.Apply(ctx, Request{Actor: "a", Values: map[string]string{"FIRMWARE_REFRESH_INTERVAL": "6h"}}); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.State() {
		if f.Key == "FIRMWARE_REFRESH_INTERVAL" && f.PendingRestart {
			t.Fatal("restart no longer needed")
		}
	}
	unsub()
	if _, err := s.Apply(ctx, Request{Actor: "a", Values: map[string]string{"BACKUP_INTERVAL": "12h"}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 { // the OTA_PARALLEL round trip notified once more, then unsubscribed
		t.Fatalf("events after unsubscribe: %d", len(got))
	}
}

// Why: a failed save must not change the running configuration.
func TestFailedSaveKeepsRunningConfig(t *testing.T) {
	st := newMem()
	s, _ := newSvc(t, st)
	st.fail = true
	if _, err := s.Apply(context.Background(), Request{Actor: "a", Values: map[string]string{"BACKUP_KEEP": "3"}}); err == nil {
		t.Fatal("expected an error")
	}
	if s.Snapshot().BackupKeep != 10 {
		t.Fatal("snapshot changed although nothing was stored")
	}
}

func TestClearReturnsToDefault(t *testing.T) {
	st := newMem()
	st.rows["BACKUP_KEEP"] = store.SystemSetting{Key: "BACKUP_KEEP", Value: "4"}
	s, _ := newSvc(t, st)
	if _, err := s.Apply(context.Background(), Request{Actor: "a", Clear: []string{"BACKUP_KEEP"}}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().BackupKeep != 10 || len(st.userRows()) != 0 {
		t.Fatalf("keep=%d rows=%v", s.Snapshot().BackupKeep, st.rows)
	}
}

// Why: a stored value that no longer validates must not stop the start and
// must not vanish silently.
func TestStoredValueThatNoLongerValidatesIsSkippedAndReported(t *testing.T) {
	st := newMem()
	st.rows["DISCOVERY_SCAN_CIDRS"] = store.SystemSetting{Key: "DISCOVERY_SCAN_CIDRS", Value: "10.9.0.0/24"}
	st.rows["DISCOVERY_SCAN"] = store.SystemSetting{Key: "DISCOVERY_SCAN", Value: "true"}
	s, _ := newSvc(t, st)
	if s.Snapshot().DiscoveryScan && len(s.Snapshot().DiscoveryScanCIDRs) == 0 {
		t.Fatal("an invalid combination must not be applied")
	}
	if len(s.Skipped()) == 0 {
		t.Fatal("skipped values must be reported")
	}
}

func TestStateHidesSecretsAndListsBootstrapReadOnly(t *testing.T) {
	st := newMem()
	st.rows["MQTT_PASSWORD"] = store.SystemSetting{Key: "MQTT_PASSWORD", Value: "fake-secret-Zk3", Secret: true}
	s, _ := newSvc(t, st, "TASMOFLEET_METRICS_TOKEN=fake-token-77")
	for _, f := range s.State() {
		if f.Key == "MQTT_PASSWORD" && (f.Value != "" || !f.HasValue) {
			t.Fatalf("secret field must report only that a value exists: %+v", f)
		}
	}
	found := false
	for _, r := range s.ReadOnly() {
		if strings.Contains(r.Value, "fake-token-77") || strings.Contains(r.Value, "fake-secret-Zk3") {
			t.Fatalf("secret leaked: %+v", r)
		}
		if r.Key == "METRICS_TOKEN" && r.Value == "***" {
			found = true
		}
		if r.Key == "OTA_PARALLEL" {
			t.Fatal("editable keys do not belong in the read-only list")
		}
	}
	if !found {
		t.Fatal("METRICS_TOKEN must be listed, masked")
	}
}
func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Why: "test connection" sends the stored broker password to whatever host the
// form names, so it needs the same confirmation as changing the credentials,
// it must not reveal the password in its answer or the audit log, and it must
// not be usable as a port scanner at full speed.
func TestMQTTTestNeverLeaksThePassword(t *testing.T) {
	st := newMem()
	st.rows["MQTT_PASSWORD"] = store.SystemSetting{Key: "MQTT_PASSWORD", Value: "fake-secret-Zk3", Secret: true, Target: "tcp://broker:1883"}
	st.rows["MQTT_URL"] = store.SystemSetting{Key: "MQTT_URL", Value: "tcp://broker:1883"}
	var seen MQTTParams
	cfg, err := loadCfg(baseEnv())
	if err != nil {
		t.Fatal(err)
	}
	aud := &memAudit{}
	now := time.Unix(1000, 0)
	s, err := New(context.Background(), Options{Base: cfg, Store: st, Audit: aud, Now: func() time.Time { return now },
		TestMQTT: func(_ context.Context, p MQTTParams) error {
			seen = p
			return errors.New("dial failed for password fake-secret-Zk3")
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	msg, err := s.TestMQTT(ctx, TestRequest{Actor: "a", Values: map[string]string{"MQTT_URL": "tcp://broker:1883", "MQTT_USER": "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if seen.URL != "tcp://broker:1883" || seen.User != "u" || seen.Password != "fake-secret-Zk3" {
		t.Fatalf("the stored password is used for the unchanged broker when the form leaves it empty: %+v", seen)
	}
	if strings.Contains(msg, "fake-secret-Zk3") || !strings.Contains(msg, "failed") {
		t.Fatalf("msg = %q", msg)
	}
	if e := aud.last(); e.Action != "sysconfig.mqtt_test" || strings.Contains(e.Detail+e.Result, "fake-secret-Zk3") {
		t.Fatalf("audit %+v", e)
	}
	// Too soon after the last try.
	if _, err := s.TestMQTT(ctx, TestRequest{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://broker:1883"}}); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := s.TestMQTT(ctx, TestRequest{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "ftp://x"}}); err == nil {
		t.Fatal("an invalid URL must be refused before any connection")
	}
}

func mqttTestSvc(t *testing.T, stored map[string]string, env []string, seen *MQTTParams) *Service {
	t.Helper()
	st := newMem()
	for k, v := range stored {
		row := store.SystemSetting{Key: k, Value: v, Secret: k == "MQTT_PASSWORD"}
		if row.Secret {
			row.Target = hostPort(stored["MQTT_URL"]) // entered for the broker stored with it
		}
		st.rows[k] = row
	}
	cfg, err := loadCfg(append(baseEnv(), env...))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Options{Base: cfg, Store: st, Audit: &memAudit{}, Now: func() time.Time { return time.Unix(5000, 0) },
		TestMQTT: func(_ context.Context, p MQTTParams) error { *seen = p; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Why (security review): the stored broker password must never be sent to a
// host the admin typed into the test form. Only the configured broker, with
// the same host and port, may receive it; anywhere else the password has to be
// typed into the form.
func TestMQTTTestNeverSendsTheStoredPasswordToAnotherTarget(t *testing.T) {
	stored := map[string]string{"MQTT_URL": "tcp://broker.lan:1883", "MQTT_PASSWORD": "fake-secret-Zk3"}
	for name, url := range map[string]string{
		"other host": "tcp://evil.example:1883", "other port": "tcp://broker.lan:1884",
		"lookalike host": "tcp://broker.lan.evil.example:1883", "other scheme default port": "ssl://broker.lan:1883",
	} {
		var seen MQTTParams
		s := mqttTestSvc(t, stored, nil, &seen)
		if _, err := s.TestMQTT(context.Background(), TestRequest{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": url}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if seen.URL == "" || seen.Password != "" {
			t.Errorf("%s: stored password sent to %s: %+v", name, url, seen)
		}
	}
	// A password typed into the form is the admin's own choice and is used.
	var seen MQTTParams
	s := mqttTestSvc(t, stored, nil, &seen)
	if _, err := s.TestMQTT(context.Background(), TestRequest{Actor: "a", StepUp: stepped, Password: "typed-pw", Values: map[string]string{"MQTT_URL": "tcp://other:1883"}}); err != nil {
		t.Fatal(err)
	}
	if seen.Password != "typed-pw" {
		t.Fatalf("seen %+v", seen)
	}
	// The unchanged broker (also spelled with another case) may use the stored one.
	seen = MQTTParams{}
	s = mqttTestSvc(t, stored, nil, &seen)
	if _, err := s.TestMQTT(context.Background(), TestRequest{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://BROKER.lan:1883"}}); err != nil {
		t.Fatal(err)
	}
	if seen.Password != "fake-secret-Zk3" {
		t.Fatalf("seen %+v", seen)
	}
}

// Why: a CLI command run before the first serve (possibly with another
// environment) must not burn the one-time import marker, or the legacy
// settings of the container would be silently lost.
func TestCLIConstructionDoesNotConsumeTheLegacyImport(t *testing.T) {
	st := newMem()
	cliCfg, err := loadCfg(baseEnv()) // the CLI shell has no legacy values
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Options{Base: cliCfg, Store: st, Audit: &memAudit{}, SkipLegacyImport: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.rows[importMarker]; ok {
		t.Fatal("a CLI-style construction must not set the import marker")
	}
	s, _ := newSvc(t, st, "TASMOFLEET_OTA_PARALLEL=5")
	if s.Snapshot().OTAParallel != 5 || st.rows["OTA_PARALLEL"].Value != "5" {
		t.Fatalf("legacy value lost after a CLI run: %d", s.Snapshot().OTAParallel)
	}
}
