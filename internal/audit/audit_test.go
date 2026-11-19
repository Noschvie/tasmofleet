package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

type fakeStore struct {
	events []model.AuditEvent
	err    error
}

func (f *fakeStore) AppendAudit(_ context.Context, e model.AuditEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}
func (f *fakeStore) ListAudit(context.Context, store.AuditFilter) ([]model.AuditEvent, error) {
	return f.events, nil
}
func (f *fakeStore) PruneAudit(context.Context, time.Time) (int64, error) { return 0, nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The audit table is readable by every admin and kept for a year, so a secret
// that reaches it is a long-lived leak. Redaction must work at any depth, in
// arrays, and for the key spellings devices and APIs actually use.
func TestRecordRedactsSecretsAtAnyDepth(t *testing.T) {
	const secret = "hunter2-do-not-store"
	cases := map[string]string{
		"flat":           `{"http_password":"` + secret + `","name":"plug"}`,
		"nested":         `{"device":{"auth":{"password":"` + secret + `"}}}`,
		"array of objs":  `{"devices":[{"name":"a","token":"` + secret + `"},{"name":"b"}]}`,
		"case differs":   `{"Password":"` + secret + `"}`,
		"tasmota style":  `{"WebPassword":"` + secret + `"}`,
		"dash spelling":  `{"client-secret":"` + secret + `"}`,
		"non-string":     `{"secret":["` + secret + `",1]}`,
		"deep array mix": `{"a":[[{"b":{"client_secret":"` + secret + `"}}]]}`,
	}
	for name, detail := range cases {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{}
			r := New(fs, quiet(), nil)
			r.Record(context.Background(), model.AuditEvent{Actor: "a", Action: "x", Detail: detail})
			if len(fs.events) != 1 {
				t.Fatal("no event stored")
			}
			if strings.Contains(fs.events[0].Detail, secret) {
				t.Fatalf("secret stored: %s", fs.events[0].Detail)
			}
			if !json.Valid([]byte(fs.events[0].Detail)) {
				t.Fatalf("detail no longer JSON: %s", fs.events[0].Detail)
			}
		})
	}
}

func TestRecordKeepsHarmlessValuesAndNumbers(t *testing.T) {
	fs := &fakeStore{}
	r := New(fs, quiet(), nil)
	r.Record(context.Background(), model.AuditEvent{Action: "x", Detail: `{"name":"plug","count":12345678901234567890,"password":"p"}`})
	var m map[string]any
	if err := json.Unmarshal([]byte(fs.events[0].Detail), &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "plug" || m["password"] != "***" {
		t.Errorf("got %v", m)
	}
	if !strings.Contains(fs.events[0].Detail, "12345678901234567890") {
		t.Errorf("large numbers must not be rounded through float64: %s", fs.events[0].Detail)
	}
}

// If the detail cannot be parsed it cannot be proven free of secrets.
func TestRecordDropsUnparsableDetail(t *testing.T) {
	fs := &fakeStore{}
	r := New(fs, quiet(), nil)
	r.Record(context.Background(), model.AuditEvent{Action: "x", Detail: `password=hunter2 {broken`})
	if strings.Contains(fs.events[0].Detail, "hunter2") {
		t.Fatalf("unparsable detail stored: %s", fs.events[0].Detail)
	}
}

func TestRecordFillsDefaults(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{}
	r := New(fs, quiet(), nil)
	r.now = func() time.Time { return now }
	r.Record(context.Background(), model.AuditEvent{Action: "device.create"})
	e := fs.events[0]
	if !e.At.Equal(now) || e.Result != "ok" || e.Detail != "" {
		t.Errorf("defaults: %+v", e)
	}
}

// A failing audit write must never fail (or panic in) the caller, but must be
// visible: logged at error level and counted.
func TestRecordStoreFailureIsCountedNotFatal(t *testing.T) {
	var logbuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logbuf, nil))
	n := 0
	r := New(&fakeStore{err: errors.New("disk full")}, log, func() { n++ })
	r.Record(context.Background(), model.AuditEvent{Action: "x", Detail: `{"password":"p"}`})
	if n != 1 {
		t.Errorf("error counter = %d", n)
	}
	if !strings.Contains(logbuf.String(), "level=ERROR") || strings.Contains(logbuf.String(), "disk full") == false {
		t.Errorf("log: %s", logbuf.String())
	}
}

func TestRedactKeysContract(t *testing.T) {
	for _, k := range []string{"password", "http_password", "token", "secret", "client_secret"} {
		found := false
		for _, rk := range RedactKeys {
			if rk == k {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from RedactKeys", k)
		}
	}
}

// Why (#505): the audit log is the record of who changed what, and the action has already
// happened when Record is called. A web handler passes the request context, which net/http
// cancels when the client goes away (a closed tab, a proxy timeout right after the submit); the
// insert then failed with "context canceled" and the change had no audit row. With the real
// database, a cancelled caller context must still leave the row.
func TestRecordStoresTheRowWhenTheCallerIsGone(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // temp DB
	failed := 0
	r := New(db, quiet(), func() { failed++ })

	gone, cancel := context.WithCancel(context.Background())
	cancel() // the client disconnected after the action committed
	r.Record(gone, model.AuditEvent{Actor: "alice", Action: "device.delete", Target: "device:7"})

	rows, err := db.ListAudit(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || len(rows) != 1 || rows[0].Action != "device.delete" || rows[0].Actor != "alice" {
		t.Fatalf("%d failed writes, rows %+v: the action happened and must have its audit row", failed, rows)
	}
}

type ctxKey struct{}

// ctxStore records what the context the recorder writes with looks like at the time of the insert
// (the recorder releases it right after).
type ctxStore struct {
	fakeStore
	called   bool
	err      error
	value    any
	deadline time.Time
	hasDL    bool
}

func (s *ctxStore) AppendAudit(ctx context.Context, e model.AuditEvent) error {
	s.called, s.err, s.value = true, ctx.Err(), ctx.Value(ctxKey{})
	s.deadline, s.hasDL = ctx.Deadline()
	return s.fakeStore.AppendAudit(ctx, e)
}

// Why (#505): the write must outlive the request but not hang for ever: it keeps the request's
// values (the audit insert may read them) and gets a deadline of its own, so a stuck database
// cannot hold a handler or the engine's worker indefinitely.
func TestRecordWritesWithItsOwnDeadlineAndTheCallersValues(t *testing.T) {
	st := &ctxStore{}
	r := New(st, quiet(), nil)
	caller, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-1"))
	cancel()
	r.Record(caller, model.AuditEvent{Action: "x"})
	if !st.called {
		t.Fatal("nothing written")
	}
	if st.err != nil {
		t.Errorf("the write context is already done: %v", st.err)
	}
	if st.value != "req-1" {
		t.Error("the request's values were dropped")
	}
	if left := time.Until(st.deadline); !st.hasDL || left > writeTimeout || left <= 0 {
		t.Errorf("deadline in %v (set: %v), want one within %v", left, st.hasDL, writeTimeout)
	}
}

// Why (#1004): an operator tracing one device greps the log for `"device":45`, the key every
// other log line uses. The audit line used to carry the device only inside the string target
// ("device:45"), so that filter missed it. The key must be a JSON number like the other lines
// (not a string), and it must never match a transaction, a backup or a setting that merely
// has the same number, or the filter would name the wrong device.
func TestTheAuditLogLineNamesItsDeviceByNumber(t *testing.T) {
	cases := []struct {
		name, target string
		wantDevice   string // "" = no device key
	}{
		{"a device target", "device:45", "45"},
		{"a large id stays exact", "device:9007199254740993", "9007199254740993"},
		{"a transaction with the same number", "transaction:45", ""},
		{"a backup", "backup:45", ""},
		{"a proposal", "proposal:45", ""},
		{"a setting", "settings:system", ""},
		{"no target", "", ""},
		{"nothing after the prefix", "device:", ""},
		{"not a number", "device:abc", ""},
		{"id zero is no device", "device:0", ""},
		{"a negative id", "device:-3", ""},
		{"another prefix that ends in device", "devices:45", ""},
		{"a sign is not the canonical spelling", "device:+45", ""},
		{"leading zeros are not the canonical spelling", "device:045", ""},
		{"trailing text", "device:45x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := New(&fakeStore{}, slog.New(slog.NewJSONHandler(&buf, nil)), nil)
			r.Record(context.Background(), model.AuditEvent{Actor: "a", Action: "x", Target: tc.target})
			m := decodeLogLine(t, buf.Bytes())
			if m["target"] != tc.target {
				t.Errorf("target = %v, want %q unchanged", m["target"], tc.target)
			}
			assertDeviceKey(t, m, tc.wantDevice)
		})
	}
}

// Why (#1004): a failed audit write is the line an operator needs most when tracing a device,
// so the one filter must find it too.
func TestTheFailedAuditWriteLineNamesItsDeviceToo(t *testing.T) {
	var buf bytes.Buffer
	r := New(&fakeStore{err: errors.New("disk full")}, slog.New(slog.NewJSONHandler(&buf, nil)), nil)
	r.Record(context.Background(), model.AuditEvent{Actor: "a", Action: "x", Target: "device:45"})
	m := decodeLogLine(t, buf.Bytes())
	if m["msg"] != "audit write failed" {
		t.Fatalf("msg = %v", m["msg"])
	}
	assertDeviceKey(t, m, "45")
	if m["target"] != "device:45" {
		t.Errorf("target = %v", m["target"])
	}
}

func decodeLogLine(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("log line %q: %v", b, err)
	}
	return m
}

// assertDeviceKey checks that the line has no device key (want "") or has it as a JSON number.
func assertDeviceKey(t *testing.T, m map[string]any, want string) {
	t.Helper()
	got, has := m["device"]
	if want == "" {
		if has {
			t.Errorf("device = %v, want no device key", got)
		}
		return
	}
	n, ok := got.(json.Number)
	if !ok {
		t.Fatalf("device = %#v (%T), want a JSON number", got, got)
	}
	if n.String() != want {
		t.Errorf("device = %s, want %s", n, want)
	}
}
