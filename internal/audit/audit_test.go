package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
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
