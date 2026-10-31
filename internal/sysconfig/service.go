package sysconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Errors of Apply. A refused request changes nothing.
var (
	ErrLocked     = errors.New("sysconfig: setting is set by the environment")
	ErrUnknownKey = errors.New("sysconfig: setting is not editable here")
)

// ValidationError carries one message per field.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "invalid settings: " + strings.Join(parts, "; ")
}

// StepUpRequiredError says which changes wait for a confirmation. Possible is
// false when the account cannot confirm at all (header authentication): those
// changes are then only possible through the environment.
type StepUpRequiredError struct {
	Keys     []string
	Possible bool
}

func (e *StepUpRequiredError) Error() string {
	return "sysconfig: confirmation required for " + strings.Join(e.Keys, ", ")
}

// StepUp is what the web layer knows about the admin's confirmation.
type StepUp struct {
	Done     bool // the admin confirmed recently
	Possible bool // the account can confirm (local password or IdP)
}

// Sources of a value.
const (
	SourceEnv     = "environment"
	SourceStored  = "stored"
	SourceDefault = "default"
)

// importMarker is the stored row that records the one-time import of legacy
// environment values; once it exists the environment is never read for
// operational settings again.
const importMarker = "_env_import"

// Store is the part of store.SystemSettingsStore the service needs.
type Store interface {
	LoadSystemSettings(ctx context.Context) (map[string]store.SystemSetting, error)
	SaveSystemSettings(ctx context.Context, actor string, changes []store.SystemSettingChange) error
}

// Options configures New.
type Options struct {
	Base  config.Config // from config.Load; Base.Legacy is imported once
	Store Store
	Audit audit.Recorder
	Log   *slog.Logger
	// TestMQTT tries a broker connection ("test connection" button). It must
	// return errors whose text is safe to show. nil disables the button.
	TestMQTT func(ctx context.Context, p MQTTParams) error
	// TestSSO runs OIDC discovery against an issuer ("test SSO" button) and returns
	// a short result; its errors must be safe to show. nil disables the button.
	TestSSO func(ctx context.Context, issuer string) (string, error)
	Now     func() time.Time
	// SkipLegacyImport leaves the environment import (and its marker) alone.
	// Only the server imports; CLI commands run with whatever environment the
	// shell has and must not consume the one-time import.
	SkipLegacyImport bool
}

// Event is sent to subscribers after a change was stored.
type Event struct {
	Old, New config.Config
	Keys     []string // changed keys
}

// Service holds the effective configuration.
type Service struct {
	base    config.Config
	store   Store
	audit   audit.Recorder
	log     *slog.Logger
	test    func(context.Context, MQTTParams) error
	ssoTest func(context.Context, string) (string, error)
	now     func() time.Time

	notifyMu sync.Mutex // serialises Apply including the subscriber calls, so events arrive in commit order
	testMu   sync.Mutex // single flight for the connection test
	lastTest time.Time

	mu      sync.Mutex
	cur     config.Config
	start   config.Config // effective at start: what the running components were built from
	stored  map[string]store.SystemSetting
	skipped map[string]error
	// unreadable lists stored secrets that could not be decrypted: they are
	// reported, the features that need them stay off until they are entered again.
	unreadable map[string]bool
	// mismatch lists stored secrets whose bound target is not the target in use:
	// the secret is not used until it is entered again (key -> bound target).
	mismatch map[string]string
	// legacyPolicy is POLICY_RULES from the environment (inline JSON or a file
	// path) if the import has not run yet; the app imports it as managed rules.
	legacyPolicy string
	subs         map[int]func(Event)
	nextSub      int
}

// New loads the stored settings and applies them on top of Base.
func New(ctx context.Context, o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Audit == nil {
		return nil, errors.New("sysconfig: an audit recorder is required")
	}
	rows, err := o.Store.LoadSystemSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: load stored settings: %w", err)
	}
	vals := map[string]string{}
	unreadable := map[string]bool{}
	for k, r := range rows {
		if _, ok := Lookup(k); !ok {
			continue
		}
		if r.Unreadable {
			unreadable[k] = true
			continue
		}
		vals[k] = r.Value
	}
	cur, skipped := config.Overlay(o.Base, vals)
	legacyPolicy, err := importLegacy(ctx, &o, rows, vals, &cur, skipped)
	if err != nil {
		return nil, err
	}
	for k := range unreadable {
		skipped[k] = errors.New("the stored value cannot be decrypted; enter it again")
	}
	for k, why := range skipped {
		o.Log.Warn("stored setting ignored", "component", "sysconfig", "key", k, "reason", why.Error())
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	cur.Legacy = nil // consumed; never keep secrets around
	svc := &Service{ssoTest: o.TestSSO, legacyPolicy: legacyPolicy, mismatch: map[string]string{}, test: o.TestMQTT, now: o.Now, base: o.Base, store: o.Store, audit: o.Audit, log: o.Log, cur: cur, stored: rows, skipped: skipped, unreadable: unreadable, subs: map[int]func(Event){}}
	svc.cur = cur
	svc.cur = svc.bindSecrets(svc.cur)
	for k, bound := range svc.mismatch {
		if strings.HasPrefix(k, "NOTIFY_") {
			// The target of a notification secret is a webhook address in full (its path or query can
			// be the credential) or a mail login: never printed, only that it changed.
			skipped[k] = errors.New("the stored secret was entered for another target; enter it again")
			o.Log.Warn("stored secret not used: its target changed", "component", "sysconfig", "key", k)
			continue
		}
		now := secretTargets[k](svc.cur)
		skipped[k] = fmt.Errorf("the stored secret was entered for %s, the target is now %s; enter it again", orNone(bound), orNone(now))
		o.Log.Warn("stored secret not used: its target changed", "component", "sysconfig", "key", k, "bound_to", bound, "now", now)
	}
	svc.start = svc.effective(svc.cur)
	return svc, nil
}

func orNone(s string) string {
	if s == "" {
		return "no target"
	}
	return s
}

// secretTargets names, for each stored secret, the target it belongs to: the
// broker for the MQTT password, the identity provider for the OIDC client secret.
var secretTargets = map[string]func(config.Config) string{
	"MQTT_PASSWORD":      func(c config.Config) string { return hostPort(c.MQTTURL) },
	"OIDC_CLIENT_SECRET": func(c config.Config) string { return hostPort(c.OIDCIssuer) },
	// The notification secrets go only where they were entered (#249): to the webhook address as
	// stored (any change of scheme, host, port, path or query drops the secret), to the mail
	// server's host, port, security mode (a downgrade to none included) and user. Without this, an admin could point
	// the address at a host of their own and read the stored secret there.
	"NOTIFY_WEBHOOK_SECRET": func(c config.Config) string { return strings.TrimSpace(c.NotifyWebhookURL) }, // the whole address: scheme, host, port, path, query
	"NOTIFY_SMTP_PASSWORD": func(c config.Config) string {
		if c.NotifySMTPHost == "" {
			return ""
		}
		return fmt.Sprintf("%s:%d|%s|%s", strings.TrimSpace(c.NotifySMTPHost), c.NotifySMTPPort, c.NotifySMTPSecurity, strings.TrimSpace(c.NotifySMTPUser))
	},
}

// setSecret stores v in the field of c that belongs to key.
func setSecret(c *config.Config, key, v string) {
	switch key {
	case "MQTT_PASSWORD":
		c.MQTTPassword = v
	case "OIDC_CLIENT_SECRET":
		c.OIDCClientSecret = v
	case "NOTIFY_WEBHOOK_SECRET":
		c.NotifyWebhookSecret = v
	case "NOTIFY_SMTP_PASSWORD":
		c.NotifySMTPPassword = v
	}
}

// reusableSecrets may follow a changed target when the admin says so. The
// OIDC client secret is not among them: it proves this app to an identity
// provider, so a new issuer needs its own.
var reusableSecrets = map[string]bool{"MQTT_PASSWORD": true}

// bindSecrets applies each stored secret to c only if it was entered for the
// target c names now. Whatever changed the target since (an edited database
// row, another save) cannot make the secret follow: it stays unused until it is
// entered again. A secret the environment sets is not stored and left alone.
func (s *Service) bindSecrets(c config.Config) config.Config {
	for key, target := range secretTargets {
		delete(s.mismatch, key)
		if c.IsLocked(key) {
			continue
		}
		row, ok := s.stored[key]
		switch {
		case !ok || row.Unreadable:
			setSecret(&c, key, "")
		case row.Target != target(c):
			setSecret(&c, key, "")
			s.mismatch[key] = row.Target
		default:
			setSecret(&c, key, row.Value)
		}
	}
	return c
}

// effective is the configuration the running process uses: a feature whose
// stored secret cannot be decrypted stays off (MQTT without its password would
// connect anonymously or fail forever; single sign-on without its secret falls
// back to local sign-in, which always remains).
func (s *Service) effective(c config.Config) config.Config {
	if s.unreadable["MQTT_PASSWORD"] {
		c.MQTTURL = ""
	}
	if s.unreadable["OIDC_CLIENT_SECRET"] {
		c.AuthMode = "local"
	}
	return c
}

// Snapshot returns the effective configuration. It is a copy; consumers read
// it at the moment they need a value, or subscribe to changes.
func (s *Service) Snapshot() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective(s.cur)
}

// Skipped lists stored values that were ignored at start and why.
func (s *Service) Skipped() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, e := range s.skipped {
		out[k] = e.Error()
	}
	return out
}

// Subscribe registers f to be called synchronously after every stored change.
// The returned function removes it.
func (s *Service) Subscribe(f func(Event)) (unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = f
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// FieldState is one editable field as the page shows it.
type FieldState struct {
	Field
	Value          string // current value; empty for secrets
	HasValue       bool   // a secret has a value
	Source         string
	PendingRestart bool
	StoredBy       string
	Skipped        string // why a stored value was ignored
	NeedsReentry   bool   // a stored secret is not used until it is entered again
	Locked         bool   // set by the environment (sign-in settings only): read-only
}

// State describes every editable field.
func (s *Service) State() []FieldState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FieldState, 0, len(fields))
	for _, f := range fields {
		val, _ := config.Format(s.cur, f.Key)
		st := FieldState{Field: f, Value: redactURL(val), HasValue: val != "", Source: SourceDefault}
		if f.Key == "NOTIFY_WEBHOOK_URL" {
			st.Value = val // the admin edits the address as stored: a token in the query must survive a save (it holds no user info: validation refuses that)
		}
		if f.Secret {
			st.Value = ""
		}
		if s.unreadable[f.Key] {
			st.HasValue = false
		}
		if r, ok := s.stored[f.Key]; ok {
			st.Source, st.StoredBy = SourceStored, r.UpdatedBy
		}
		if s.cur.IsLocked(f.Key) {
			st.Locked, st.Source = true, SourceEnv
		}
		st.PendingRestart = f.Apply == Restart && !config.Equal(s.start, s.cur, f.Key)
		if why, ok := s.skipped[f.Key]; ok {
			st.Skipped = why.Error()
			st.NeedsReentry = f.Secret
		}
		out = append(out, st)
	}
	return out
}

// Row is a read-only line (bootstrap settings).
type Row struct {
	Key, Value string
	Secret     bool
}

// ReadOnly lists everything the page does not edit: the bootstrap and
// security settings. Secrets show "***" (or nothing when empty), URLs lose
// any user information.
func (s *Service) ReadOnly() []Row {
	cur := s.Snapshot()
	var out []Row
	for _, k := range config.Keys() {
		if _, ok := Lookup(k.Key); ok {
			continue
		}
		v, _ := config.Format(cur, k.Key)
		if k.Secret {
			if v != "" {
				v = "***"
			}
		} else {
			v = redactURL(v)
		}
		out = append(out, Row{Key: k.Key, Value: v, Secret: k.Secret})
	}
	return out
}

// Request is one submitted form.
type Request struct {
	Actor  string
	Via    string            // listener, for the audit record
	Values map[string]string // new values by key; a secret with an empty value is "unchanged" and must not be passed
	Clear  []string          // keys whose stored value is removed (back to the default)
	// Reuse lists secrets (only those in reusableSecrets) that the admin lets
	// follow a changed target instead of typing them again (audit row 16,
	// 2026-10-21). The stored value is bound to the new target and used there.
	Reuse  []string
	StepUp StepUp
	// Audit, if set, receives the audit records instead of the service's own
	// recorder (the web layer passes the recorder that tags the listener).
	Audit audit.Recorder

	reset bool // Reset: audit as sysconfig.reset and remove a cleared row even when its value is the default
}

// action is the audit action of the request.
func (r Request) action() string {
	if r.reset {
		return "sysconfig.reset"
	}
	return "sysconfig.update"
}

func (s *Service) rec(r audit.Recorder) audit.Recorder {
	if r != nil {
		return r
	}
	return s.audit
}

// Change is one applied change; secret values are never part of it.
type Change struct{ Key, Old, New string }

// Result of a successful Apply.
type Result struct {
	Changes         []Change
	Live            []string // applied now
	RestartRequired []string // applied after the next restart
}

// Apply validates and stores the changes. Everything is checked before
// anything is written: unknown or locked keys, the same validation as at
// start, the step-up. Then the values are stored, the snapshot is swapped,
// the change is audited and the subscribers are told.
func (s *Service) Apply(ctx context.Context, req Request) (Result, error) {
	s.notifyMu.Lock() // events leave in commit order, one save at a time
	defer s.notifyMu.Unlock()
	s.mu.Lock()
	res, ev, err := s.apply(ctx, req)
	subs := make([]func(Event), 0, len(s.subs))
	if err == nil && ev != nil {
		ids := make([]int, 0, len(s.subs))
		for id := range s.subs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			subs = append(subs, s.subs[id])
		}
	}
	s.mu.Unlock()
	if ev != nil {
		for _, f := range subs {
			f(*ev)
		}
	}
	return res, err
}

func (s *Service) deny(ctx context.Context, req Request, why string, keys []string) {
	d, _ := json.Marshal(withVia(map[string]any{"keys": keys}, req.Via))
	s.rec(req.Audit).Record(ctx, model.AuditEvent{Actor: req.Actor, Action: req.action(), Target: "settings:system", Detail: string(d), Result: "denied: " + why})
}

func (s *Service) apply(ctx context.Context, req Request) (Result, *Event, error) {
	keys := make([]string, 0, len(req.Values)+len(req.Clear))
	for k := range req.Values {
		keys = append(keys, k)
	}
	keys = append(keys, req.Clear...)
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 && keys[i-1] == k {
			return Result{}, nil, &ValidationError{Fields: map[string]string{k: "set and cleared (or given twice) in the same request"}}
		}
	}
	for _, k := range keys {
		if s.cur.IsLocked(k) {
			s.deny(ctx, req, "locked by environment", keys)
			return Result{}, nil, fmt.Errorf("%w: %s", ErrLocked, k)
		}
	}
	for _, k := range keys {
		if _, ok := Lookup(k); !ok {
			s.deny(ctx, req, "not editable", keys)
			return Result{}, nil, fmt.Errorf("%w: %q", ErrUnknownKey, k)
		}
	}

	cand := s.cur
	var changed []string
	for _, k := range keys {
		var want string
		if slices.Contains(req.Clear, k) {
			want = config.DefaultValue(k)
		} else {
			want = req.Values[k]
		}
		if err := config.Set(&cand, k, want); err != nil {
			return Result{}, nil, &ValidationError{Fields: map[string]string{k: config.KeyErrors(err)[k]}}
		}
		if !config.Equal(s.cur, cand, k) {
			changed = append(changed, k)
		}
	}
	// A reset also removes stored rows that hold the default value: afterwards
	// no field is "stored" any more.
	var unchanged []string
	if req.reset {
		for _, k := range req.Clear {
			if _, stored := s.stored[k]; stored && !slices.Contains(changed, k) {
				unchanged = append(unchanged, k)
			}
		}
	}
	if len(changed) == 0 && len(unchanged) == 0 {
		return Result{}, nil, nil
	}
	for _, k := range changed {
		if f, _ := Lookup(k); len(f.Enum) > 0 {
			if v, _ := config.Format(cand, k); !slices.Contains(f.Enum, v) {
				return Result{}, nil, &ValidationError{Fields: map[string]string{k: "must be one of " + strings.Join(f.Enum, ", ")}}
			}
		}
	}
	if err := config.Validate(cand); err != nil {
		fields := config.KeyErrors(err)
		if len(fields) == 0 {
			fields = map[string]string{changed[0]: err.Error()}
		}
		return Result{}, nil, &ValidationError{Fields: fields}
	}
	if fe := uiChecks(cand, changed); len(fe) > 0 {
		return Result{}, nil, &ValidationError{Fields: fe}
	}
	// A secret only goes to the target it was entered for: a new target needs
	// the secret typed again (or removed) in the same save.
	for key, target := range secretTargets {
		if target(s.cur) == target(cand) || secretValue(s.cur, key) == "" {
			continue
		}
		_, typed := req.Values[key]
		if slices.Contains(req.Reuse, key) {
			if !reusableSecrets[key] {
				return Result{}, nil, &ValidationError{Fields: map[string]string{key: "this secret cannot follow a changed target; enter it again"}}
			}
			if !typed && !slices.Contains(req.Clear, key) && !slices.Contains(changed, key) {
				changed = append(changed, key) // rewritten below with the new target, same value
			}
			continue
		}
		if !typed && !slices.Contains(req.Clear, key) {
			return Result{}, nil, &ValidationError{Fields: map[string]string{key: "enter the secret again, remove it, or keep it for the new target"}}
		}
	}

	if need := s.stepUpKeys(cand, changed); len(need) > 0 && !req.StepUp.Done {
		return Result{}, nil, &StepUpRequiredError{Keys: need, Possible: req.StepUp.Possible}
	}

	var writes []store.SystemSettingChange
	var res Result
	var audited []map[string]any
	for _, k := range changed {
		f, _ := Lookup(k)
		oldV, _ := config.Format(s.cur, k)
		newV, _ := config.Format(cand, k)
		if slices.Contains(req.Clear, k) {
			writes = append(writes, store.SystemSettingChange{Key: k, Delete: true})
		} else {
			w := store.SystemSettingChange{Key: k, Value: newV, Secret: f.Secret}
			if target, bound := secretTargets[k]; f.Secret && bound {
				w.Target = target(cand) // the secret belongs to the target it was entered for
			}
			writes = append(writes, w)
		}
		shownOld, shownNew := redactURL(oldV), redactURL(newV)
		if k == "NOTIFY_WEBHOOK_URL" {
			shownOld, shownNew = redactWebhookURL(oldV), redactWebhookURL(newV) // the path can be a secret (a Home Assistant webhook id)
		}
		if f.Secret {
			shownOld, shownNew = maskSecret(oldV), maskSecret(newV)
		} else if f.Key == "MQTT_USER" {
			shownOld, shownNew = oldV, newV
		}
		audited = append(audited, map[string]any{"key": k, "old": shownOld, "new": shownNew})
		res.Changes = append(res.Changes, Change{Key: k, Old: shownOld, New: shownNew})
		if f.Apply == Live {
			res.Live = append(res.Live, k)
		} else {
			res.RestartRequired = append(res.RestartRequired, k)
		}
	}
	for _, k := range unchanged {
		writes = append(writes, store.SystemSettingChange{Key: k, Delete: true})
	}
	if err := s.store.SaveSystemSettings(ctx, req.Actor, writes); err != nil {
		return Result{}, nil, fmt.Errorf("sysconfig: store settings: %w", err)
	}
	for _, w := range writes {
		if w.Delete {
			delete(s.stored, w.Key)
		} else {
			s.stored[w.Key] = store.SystemSetting{Key: w.Key, Value: w.Value, Secret: w.Secret, Target: w.Target, UpdatedBy: req.Actor}
		}
	}
	old := s.effective(s.cur)
	for _, key := range append(slices.Clone(changed), unchanged...) {
		if f, _ := Lookup(key); f.Secret {
			delete(s.unreadable, key) // entered again (or removed)
			delete(s.skipped, key)
		}
	}
	s.cur = s.bindSecrets(cand)
	for key := range secretTargets {
		if _, bound := s.mismatch[key]; !bound && !s.unreadable[key] {
			delete(s.skipped, key)
		}
	}
	d, _ := json.Marshal(withVia(map[string]any{"changes": audited}, req.Via))
	s.rec(req.Audit).Record(ctx, model.AuditEvent{Actor: req.Actor, Action: req.action(), Target: "settings:system", Detail: string(d), Result: "ok"})
	return res, &Event{Old: old, New: s.effective(s.cur), Keys: changed}, nil
}

// stepUpKeys returns the changed keys whose rule asks for a confirmation.
func (s *Service) stepUpKeys(cand config.Config, changed []string) []string {
	var need []string
	for _, k := range changed {
		f, _ := Lookup(k)
		if stepUpNeeded(f, s.cur, cand) {
			need = append(need, k)
		}
	}
	return need
}

func secretValue(c config.Config, key string) string {
	switch key {
	case "MQTT_PASSWORD":
		return c.MQTTPassword
	case "OIDC_CLIENT_SECRET":
		return c.OIDCClientSecret
	case "NOTIFY_WEBHOOK_SECRET":
		return c.NotifyWebhookSecret
	case "NOTIFY_SMTP_PASSWORD":
		return c.NotifySMTPPassword
	}
	return ""
}

// stepUpNeeded decides whether changing f needs a fresh confirmation. It fails
// closed: an unknown rule asks for the confirmation.
func stepUpNeeded(f Field, _, _ config.Config) bool {
	switch f.StepUp {
	case StepUpNever:
		return false
	case StepUpAlways:
		return true
	default:
		return true
	}
}

// uiChecks are rules only the UI enforces (stricter than the environment).
func uiChecks(cand config.Config, changed []string) map[string]string {
	out := map[string]string{}
	if slices.Contains(changed, "MQTT_URL") {
		if u, err := url.Parse(cand.MQTTURL); err == nil && u.User != nil {
			out["MQTT_URL"] = "must not contain a user name or password; use the User and Password fields"
		}
	}
	return out
}

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	return "***"
}

// redactURL drops the user information of a URL-looking value.
func redactURL(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	u.RawQuery = ""
	return u.String()
}

// MQTTParams are the broker settings a connection test uses.
type MQTTParams struct{ URL, User, Password, FullTopic string }

// TestRequest is one "test connection" click. Values holds the form's
// MQTT_URL, MQTT_USER and MQTT_FULLTOPIC; an empty or missing field uses the
// current value. The password is always the stored one: it is write-only.
type TestRequest struct {
	Actor  string
	Via    string
	Values map[string]string
	// Password is a password typed into the test form. Without it, the stored
	// password is used only for the unchanged, configured broker.
	Password string
	StepUp   StepUp
	Audit    audit.Recorder
}

// Errors of TestMQTT.
var (
	ErrNoTester = errors.New("sysconfig: connection test is not available")
	ErrTooSoon  = errors.New("sysconfig: wait a few seconds between connection tests")
)

const testGap = 5 * time.Second

// TestMQTT tries to connect with the form's broker settings and the stored
// password. It returns a short result text ("connected" or "failed: ...").
// The stored password goes only to the broker it was entered for.
func (s *Service) TestMQTT(ctx context.Context, req TestRequest) (string, error) {
	if s.test == nil {
		return "", ErrNoTester
	}
	cur := s.Snapshot()
	cand := cur
	for _, k := range []string{"MQTT_URL", "MQTT_USER", "MQTT_FULLTOPIC"} {
		v := req.Values[k]
		if v == "" {
			continue
		}
		if err := config.Set(&cand, k, v); err != nil {
			return "", &ValidationError{Fields: map[string]string{k: config.KeyErrors(err)[k]}}
		}
	}
	if err := config.Validate(cand); err != nil {
		return "", &ValidationError{Fields: config.KeyErrors(err)}
	}
	if fe := uiChecks(cand, []string{"MQTT_URL"}); len(fe) > 0 {
		return "", &ValidationError{Fields: fe}
	}
	if cand.MQTTURL == "" {
		return "", &ValidationError{Fields: map[string]string{"MQTT_URL": "enter a broker URL to test"}}
	}
	// The stored password goes only to the broker that is configured now,
	// same scheme, host and port. Any other target needs a typed password.
	password := req.Password
	if password == "" && sameBroker(cur.MQTTURL, cand.MQTTURL) {
		password = cur.MQTTPassword
	}
	if !s.testMu.TryLock() {
		return "", ErrTooSoon
	}
	defer s.testMu.Unlock()
	if now := s.now(); now.Sub(s.lastTest) < testGap {
		return "", ErrTooSoon
	} else {
		s.lastTest = now
	}
	p := MQTTParams{URL: cand.MQTTURL, User: cand.MQTTUser, Password: password, FullTopic: cand.MQTTFullTopic}
	err := s.test(ctx, p)
	msg, result := "connected", "ok"
	if err != nil {
		text := err.Error()
		if p.Password != "" {
			text = strings.ReplaceAll(text, p.Password, "***")
		}
		msg, result = "failed: "+text, "failed"
	}
	d, _ := json.Marshal(withVia(map[string]any{"url": redactURL(p.URL), "host": hostPort(p.URL), "user": p.User, "stored_password": password != "" && req.Password == ""}, req.Via))
	s.rec(req.Audit).Record(ctx, model.AuditEvent{Actor: req.Actor, Action: "sysconfig.mqtt_test", Target: "settings:system", Detail: string(d), Result: result})
	return msg, nil
}

// withVia adds the listener only when known (the app's recorder tags it itself).
func withVia(m map[string]any, via string) map[string]any {
	if via != "" {
		m["via"] = via
	}
	return m
}

var defaultPorts = map[string]string{"tcp": "1883", "mqtt": "1883", "ssl": "8883", "tls": "8883", "mqtts": "8883", "ws": "80", "wss": "443", "http": "80", "https": "443"}

// hostPort returns scheme://host:port in lower case, with the scheme's
// default port filled in; "" if the URL does not parse.
func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPorts[u.Scheme]
	}
	return strings.ToLower(u.Scheme + "://" + u.Hostname() + ":" + port)
}

// sameBroker reports whether two broker URLs name the same scheme, host and port.
func sameBroker(a, b string) bool {
	x := hostPort(a)
	return x != "" && x == hostPort(b)
}

// importLegacy moves operational settings that are still set in the environment
// or the env file into the database, once. The environment is not a source for
// them any more: a value is imported only if the database has none, and only on
// the first start (the marker row), so removing a value in the UI cannot bring the
// old environment value back. Invalid values are reported and not imported.
// It returns POLICY_RULES (inline JSON or a path) for the app to import as managed
// rules, if the import has not run yet.
func importLegacy(ctx context.Context, o *Options, rows map[string]store.SystemSetting, vals map[string]string, cur *config.Config, skipped map[string]error) (string, error) {
	if o.SkipLegacyImport {
		return "", nil
	}
	legacy := o.Base.Legacy
	keys := make([]string, 0, len(legacy))
	for k := range legacy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if _, done := rows[importMarker]; done {
		for _, k := range keys {
			o.Log.Warn("ignoring an environment value: settings are stored in the database now; remove it from the container configuration",
				"component", "sysconfig", "key", config.Prefix+k)
		}
		return "", nil
	}
	pending := map[string]string{}
	for _, k := range keys {
		if k == "POLICY_RULES" {
			continue
		}
		if _, stored := rows[k]; stored {
			o.Log.Warn("ignoring an environment value: a stored setting exists", "component", "sysconfig", "key", config.Prefix+k)
			continue
		}
		pending[k] = legacy[k]
	}
	splitURLCredentials(pending, rows)
	next, rejected := config.Overlay(*cur, pending)
	var changes []store.SystemSettingChange
	var imported []string
	order := keys
	for _, k := range []string{"MQTT_USER", "MQTT_PASSWORD"} {
		if _, listed := legacy[k]; !listed {
			order = append(order, k) // split out of MQTT_URL
		}
	}
	for _, k := range order {
		v, ok := pending[k]
		if !ok {
			continue
		}
		if why := rejected[k]; why != nil {
			skipped[k] = fmt.Errorf("the environment value was not imported: %w", why)
			o.Log.Error("environment value not imported: invalid", "component", "sysconfig", "key", config.Prefix+k, "reason", why.Error())
			continue
		}
		f, _ := Lookup(k)
		ch := store.SystemSettingChange{Key: k, Value: v, Secret: f.Secret}
		if t, bound := secretTargets[k]; f.Secret && bound {
			ch.Target = t(next)
		} else if f.Secret {
			ch.Target = hostPort(next.MQTTURL)
		} else if formatted, err := config.Format(next, k); err == nil {
			ch.Value = formatted
		}
		changes = append(changes, ch)
		rows[k] = store.SystemSetting{Key: k, Value: ch.Value, Secret: ch.Secret, Target: ch.Target, UpdatedBy: "env-import"}
		imported = append(imported, k)
		vals[k] = ch.Value
		o.Log.Warn("imported a setting from the environment; remove it from the container configuration",
			"component", "sysconfig", "key", config.Prefix+k)
	}
	changes = append(changes, store.SystemSettingChange{Key: importMarker, Value: "1"})
	if err := o.Store.SaveSystemSettings(ctx, "env-import", changes); err != nil {
		return "", fmt.Errorf("sysconfig: import environment settings: %w", err)
	}
	if len(imported) > 0 {
		d, _ := json.Marshal(map[string]any{"keys": imported})
		o.Audit.Record(ctx, model.AuditEvent{Actor: "system", Action: "sysconfig.env_import", Target: "settings:system", Detail: string(d), Result: "ok"})
	}
	rows[importMarker] = store.SystemSetting{Key: importMarker, Value: "1", UpdatedBy: "env-import"}
	*cur = next
	return legacy["POLICY_RULES"], nil
}

// LegacyPolicyRules returns POLICY_RULES from the environment (inline JSON or a
// file path) when it has not been imported yet; the empty string otherwise.
func (s *Service) LegacyPolicyRules() string { return s.legacyPolicy }

// splitURLCredentials moves user information out of a legacy MQTT_URL
// (tcp://user:password@host:1883) into MQTT_USER and MQTT_PASSWORD, unless those
// are set already: the password belongs in the sealed field, not in a plain row.
func splitURLCredentials(pending map[string]string, rows map[string]store.SystemSetting) {
	raw, ok := pending["MQTT_URL"]
	if !ok {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return
	}
	user, pw := u.User.Username(), ""
	if p, set := u.User.Password(); set {
		pw = p
	}
	u.User = nil
	pending["MQTT_URL"] = u.String()
	if _, has := pending["MQTT_USER"]; !has && rows["MQTT_USER"].Key == "" && user != "" {
		pending["MQTT_USER"] = user
	}
	if _, has := pending["MQTT_PASSWORD"]; !has && rows["MQTT_PASSWORD"].Key == "" && pw != "" {
		pending["MQTT_PASSWORD"] = pw
	}
}

// TestSSO checks the identity provider named by the form (or the current issuer
// when the form leaves it empty) with OIDC discovery. It sends no secret. It
// shares the pause between tests with TestMQTT.
func (s *Service) TestSSO(ctx context.Context, req TestRequest) (string, error) {
	if s.ssoTest == nil {
		return "", ErrNoTester
	}
	issuer := req.Values["OIDC_ISSUER"]
	if issuer == "" {
		issuer = s.Snapshot().OIDCIssuer
	}
	if issuer == "" {
		return "", &ValidationError{Fields: map[string]string{"OIDC_ISSUER": "enter an issuer URL to test"}}
	}
	if !s.testMu.TryLock() {
		return "", ErrTooSoon
	}
	defer s.testMu.Unlock()
	if now := s.now(); now.Sub(s.lastTest) < testGap {
		return "", ErrTooSoon
	} else {
		s.lastTest = now
	}
	msg, err := s.ssoTest(ctx, issuer)
	result := "ok"
	if err != nil {
		msg, result = "failed: "+err.Error(), "failed"
	}
	d, _ := json.Marshal(withVia(map[string]any{"issuer": redactURL(issuer)}, req.Via))
	s.rec(req.Audit).Record(ctx, model.AuditEvent{Actor: req.Actor, Action: "sysconfig.sso_test", Target: "settings:system", Detail: string(d), Result: result})
	return msg, nil
}

// setupMarker is the stored row that records that the first-run setup was
// finished or skipped.
const setupMarker = "_setup_done"

// SetupDone reports whether the first-run setup was finished or skipped.
func (s *Service) SetupDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.stored[setupMarker]
	return ok
}

// MarkSetupDone records that the first-run setup is finished (or skipped).
func (s *Service) MarkSetupDone(ctx context.Context, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stored[setupMarker]; ok {
		return nil
	}
	if err := s.store.SaveSystemSettings(ctx, actor, []store.SystemSettingChange{{Key: setupMarker, Value: "1"}}); err != nil {
		return fmt.Errorf("sysconfig: record the setup: %w", err)
	}
	s.stored[setupMarker] = store.SystemSetting{Key: setupMarker, Value: "1", UpdatedBy: actor}
	return nil
}

// Preview is the effective configuration with the given form values on top, validated but not
// stored: the base of the notification test buttons. A key that is missing (an empty secret field
// is not posted) keeps its current value, so a stored secret is used by the test without ever
// being shown, but only for the target it was entered for: when the form names another target
// the stored secret is dropped from the preview and its key is returned, so the test cannot be
// used to send a write-only secret to a host of the admin's choice.
func (s *Service) Preview(values map[string]string) (config.Config, []string, error) {
	cur := s.Snapshot()
	cand := cur
	for k, v := range values {
		if _, ok := Lookup(k); !ok {
			return config.Config{}, nil, &ValidationError{Fields: map[string]string{k: "not an editable setting"}}
		}
		if err := config.Set(&cand, k, v); err != nil {
			return config.Config{}, nil, &ValidationError{Fields: map[string]string{k: config.KeyErrors(err)[k]}}
		}
	}
	if err := config.Validate(cand); err != nil {
		return config.Config{}, nil, &ValidationError{Fields: config.KeyErrors(err)}
	}
	var dropped []string
	for key, target := range secretTargets {
		if _, typed := values[key]; typed || secretValue(cand, key) == "" || target(cand) == target(cur) {
			continue
		}
		setSecret(&cand, key, "")
		dropped = append(dropped, key)
	}
	return cand, dropped, nil
}

// redactWebhookURL keeps scheme, host and port of a webhook address for the audit log and hides
// the path and query, which can be the secret itself (a Home Assistant webhook id, an ntfy topic).
func redactWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		out += "/***"
	}
	return out
}
