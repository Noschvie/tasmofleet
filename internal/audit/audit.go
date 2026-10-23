// Package audit records every state-changing action. Record never fails the
// caller's operation silently: an audit write error is logged at error level
// and counted in tasmofleet_audit_errors_total. Secrets (passwords, tokens)
// must never appear in Detail; Record redacts known keys defensively.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Recorder is used by web handlers, the CLI and the engine.
type Recorder interface {
	Record(ctx context.Context, e model.AuditEvent)
}

// RedactKeys are JSON keys whose values Record replaces with "***".
var RedactKeys = []string{"password", "http_password", "token", "secret", "client_secret"}

const redacted = "***"

// Store is the part of store.AuditStore the recorder needs.
type Store interface {
	AppendAudit(ctx context.Context, e model.AuditEvent) error
}

var _ Store = store.AuditStore(nil)

// StoreRecorder writes audit events to a Store with redaction.
type StoreRecorder struct {
	store   Store
	log     *slog.Logger
	onError func()
	now     func() time.Time
}

var _ Recorder = (*StoreRecorder)(nil)

// New returns a Recorder. onError (may be nil) is called for every failed
// write and feeds tasmofleet_audit_errors_total; log may be nil.
func New(s Store, log *slog.Logger, onError func()) *StoreRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &StoreRecorder{store: s, log: log, onError: onError, now: time.Now}
}

// Record redacts the detail, fills At and Result when empty, stores the event
// and logs it at info level. Failures are logged and counted, never returned.
func (r *StoreRecorder) Record(ctx context.Context, e model.AuditEvent) {
	if e.At.IsZero() {
		e.At = r.now().UTC()
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	e.Detail = Redact(e.Detail)
	if err := r.store.AppendAudit(ctx, e); err != nil {
		r.log.Error("audit write failed", "component", "audit", "action", e.Action, "target", e.Target, "err", err)
		if r.onError != nil {
			r.onError()
		}
		return
	}
	r.log.Info("audit", "component", "audit", "actor", e.Actor, "action", e.Action, "target", e.Target, "result", e.Result)
}

// Redact replaces the value of every key matching RedactKeys with "***",
// at any depth. Keys match case-insensitively, ignoring "_" and "-", and as a
// substring, so "WebPassword" and "client-secret" are covered. Detail that is
// not valid JSON cannot be proven secret-free and is replaced by a marker.
// Empty detail stays empty.
func Redact(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(detail))
	dec.UseNumber() // keep large numbers exact
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return `{"detail":"` + "dropped: not valid JSON" + `"}`
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactValue(v)); err != nil {
		return `{"detail":"dropped: not encodable"}`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKey(k) {
				t[k] = redacted
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
		return t
	}
	return v
}

func normalise(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("_", "", "-", "").Replace(s)
}

func secretKey(k string) bool {
	nk := normalise(k)
	for _, rk := range RedactKeys {
		if strings.Contains(nk, normalise(rk)) {
			return true
		}
	}
	return false
}
