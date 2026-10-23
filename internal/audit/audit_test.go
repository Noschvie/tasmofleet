package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
