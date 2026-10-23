package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: the release feed row tells whether update planning works from a fresh list. A failed
// check after an earlier success is a warning (the stale list still serves), a failure without
// any list is an error, and the fetch error text never reaches the page: it can contain a
// source URL with credentials.
func TestFeedComp(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	secret := "Get https://user:pw@mirror.example/index: timeout"
	cases := []struct {
		name  string
		st    firmware.IndexStatus
		tone  string
		state string
		has   string
	}{
		{"never tried", firmware.IndexStatus{}, "neutral", "Waiting", "Not checked yet"},
		{"healthy", firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-5 * time.Minute), LastAttempt: now.Add(-5 * time.Minute)}, "ok", "Healthy", "Checked 5m ago"},
		{"stale", firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-3 * time.Hour), LastAttempt: now.Add(-time.Minute), LastError: secret}, "warn", "Stale", "using the list from 3h ago"},
		{"down", firmware.IndexStatus{LastAttempt: now.Add(-time.Minute), LastError: secret}, "danger", "Failing", "no release list yet"},
	}
	for _, c := range cases {
		got := feedComp(c.st, now)
		if got.Tone != c.tone || got.State != c.state || !strings.Contains(got.Detail, c.has) {
			t.Errorf("%s: %+v, want tone %s, state %s, detail with %q", c.name, got, c.tone, c.state, c.has)
		}
		if strings.Contains(got.Detail, "pw@") || strings.Contains(got.Detail, "mirror.example") {
			t.Errorf("%s: fetch error leaked into the page: %q", c.name, got.Detail)
		}
	}
}

// Why: "running" and "queued" are different promises (a queued job waits for a slot), so they
// are counted apart; an unreadable job table is a failure, not "Idle".
func TestJobsComp(t *testing.T) {
	jobs := []model.Job{{State: model.JobQueued}, {State: model.JobVerify}, {State: model.JobQueued}, {State: model.JobApplying}, {State: model.JobQueued}}
	if got := jobsComp(jobs, nil); got.Detail != "2 jobs running · 3 jobs queued" || got.State != "Busy" {
		t.Errorf("busy: %+v", got)
	}
	if got := jobsComp([]model.Job{{State: model.JobQueued}}, nil); got.Detail != "1 job queued" {
		t.Errorf("queued only: %+v", got)
	}
	if got := jobsComp(nil, nil); got.Detail != "Idle" || got.Tone != "ok" {
		t.Errorf("idle: %+v", got)
	}
	if got := jobsComp(nil, errors.New("db locked")); got.Tone != "danger" || strings.Contains(got.Detail, "locked") {
		t.Errorf("error: %+v", got)
	}
}

func TestDBComp(t *testing.T) {
	if got := dbComp(store.DBInfo{SQLiteVersion: "3.50.4", Size: 3 << 20}, nil); got.Detail != "SQLite 3.50.4 · 3.0 MiB" || got.Tone != "ok" {
		t.Errorf("ok: %+v", got)
	}
	if got := dbComp(store.DBInfo{}, errors.New("disk I/O error")); got.Tone != "danger" || strings.Contains(got.Detail, "I/O") {
		t.Errorf("error: %+v", got)
	}
}

type fakeDBInfo struct{}

func (fakeDBInfo) DBInfo(context.Context) (store.DBInfo, error) {
	return store.DBInfo{SQLiteVersion: "3.99.0", Size: 2048}, nil
}

// Why: N13 rows reach the About page (admin, read-only health), after MQTT and the
// backup decoder, without a template change.
func TestAboutShowsComponentRows(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Database = fakeDBInfo{} })
	_, body := e.do(t, "GET", "/about", "admin", nil)
	last := 0
	for _, want := range []string{"MQTT broker", "Backup decoder", "Database", "SQLite 3.99.0 · 2.0 KiB"} {
		i := strings.Index(body, want)
		if i < 0 {
			t.Errorf("About lacks %q", want)
			continue
		}
		if i < last {
			t.Errorf("%q out of order", want)
		}
		last = i
	}
}
