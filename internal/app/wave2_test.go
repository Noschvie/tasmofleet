package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why (N19): the /metrics master switch must take effect on the running
// server without a restart, and "off" must look like no endpoint at all (404),
// even to a scraper that would otherwise be allowed (loopback, no token).
func TestMetricsSwitchAppliesLive(t *testing.T) {
	svc := importServices(t)
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log}
	h := a.metricsHandler()
	get := func() int {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("default on: %d", c)
	}
	set := func(v string) {
		if _, err := svc.Sys.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: map[string]string{"METRICS_ENABLED": v}}); err != nil {
			t.Fatal(err)
		}
	}
	set("false")
	if c := get(); c != http.StatusNotFound {
		t.Fatalf("switched off: %d, want 404", c)
	}
	set("true")
	if c := get(); c != http.StatusOK {
		t.Fatalf("switched on again: %d", c)
	}
}

// Why (N17): the log level chosen on the settings page must change what the
// running process logs at once, through the one LevelVar the logger shares.
func TestLogLevelFollowsTheSettingLive(t *testing.T) {
	svc := importServices(t)
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	FollowLogLevel(svc.Sys, level, log)
	log.Debug("hidden-at-info")
	if strings.Contains(buf.String(), "hidden-at-info") {
		t.Fatal("default info level must hide debug lines")
	}
	if _, err := svc.Sys.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "debug"}}); err != nil {
		t.Fatal(err)
	}
	log.Debug("shown-at-debug")
	if !strings.Contains(buf.String(), "shown-at-debug") {
		t.Fatal("debug must show right after the change, without a restart")
	}
	if _, err := svc.Sys.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "error"}}); err != nil {
		t.Fatal(err)
	}
	log.Warn("hidden-at-error")
	if strings.Contains(buf.String(), "hidden-at-error") {
		t.Fatal("error level must hide warnings")
	}
}

// Why (N17): the logger is built from the environment before the database is
// open; a level stored on the settings page in an earlier run must take over
// as soon as the settings are loaded. An unknown value means info.
func TestStoredLogLevelAppliesAtStart(t *testing.T) {
	svc := importServices(t)
	if _, err := svc.Sys.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "warn"}}); err != nil {
		t.Fatal(err)
	}
	log, level := NewLogger(svc.Cfg) // svc.Cfg was taken before the change: info
	if level.Level() != slog.LevelInfo {
		t.Fatalf("level %v", level.Level())
	}
	FollowLogLevel(svc.Sys, level, log)
	if level.Level() != slog.LevelWarn {
		t.Fatalf("stored level not applied at start: %v", level.Level())
	}
	SetLogLevel(level, "nonsense")
	if level.Level() != slog.LevelInfo {
		t.Fatal("an unknown level must fall back to info")
	}
}

// Why: nanosecond timestamps (.15597172) are hard to read; the log carries milliseconds in a fixed
// format. It goes through NewLogger, so a logger built without the ReplaceAttr fails here.
func TestLogTimestampHasMilliseconds(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w // the JSON logger writes to stdout
	l, _ := NewLogger(config.Config{LogLevel: "info", LogFormat: "json"})
	os.Stdout = old
	l.Info("x", slog.Time("other", time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)))
	_ = w.Close() // end of the pipe: nothing more to read
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(out, &rec); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}(Z|[+-]\d\d:\d\d)$`).MatchString(rec["time"].(string)) {
		t.Fatalf("time = %v", rec["time"])
	}
	if rec["other"] != "2026-01-02T03:04:05.123456789Z" {
		t.Fatalf("only the record time may be shortened, got %v", rec["other"])
	}
}
