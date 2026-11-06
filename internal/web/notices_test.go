package web

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
)

// noticeStream opens an event stream (a path below /tf) as role; the caller closes the body.
func noticeStream(t *testing.T, e *env, path, role string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf"+path, nil)
	req.Header.Set("X-Test-Role", role)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readOn reads the stream until marker shows in what arrives after got, and returns got plus that.
func readOn(t *testing.T, resp *http.Response, got, marker string) string {
	t.Helper()
	from := len(got)
	buf := make([]byte, 4096)
	for !strings.Contains(got[from:], marker) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended before %q: %v (%q)", marker, err, got)
		}
	}
	return got
}

// publishing repeats evs, in order, every 20 ms until stop (a stream subscribes only after it has
// answered, so a single publish could come too early).
func publishing(t *testing.T, e *env, evs ...events.Event) (stop func()) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			for _, ev := range evs {
				e.hub.Publish(ev)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func noticeEvent(id string) events.Event {
	return events.Event{Topic: events.TopicNotice, Payload: events.Notice{ID: id, Name: "snapshot.failed", Result: "failed",
		Title: "Database snapshot failed", Text: "not a directory", Path: "/settings/system/snapshots"}}
}

func jobEvent(id model.JobID) events.Event {
	return events.Event{Topic: events.TopicJob, Payload: model.Job{ID: id, DeviceID: 1}}
}

// Why (#540): only admins can turn browser notifications on (the card is on their Settings page),
// and a failed database snapshot is admin business (its page and the audit log are admin-only). So
// an operator's stream carries no notice, an admin's page without live data gets the notices alone
// (and nothing else of the stream), and a demoted admin stops hearing them at the next re-check, as
// the admin-only streams do (GHSA-wq89-cqcx-3h3h). Each event published after another arrives after
// it, so a later marker proves the earlier one was left out.
func TestNoticesReachAdminsOnly(t *testing.T) {
	t.Run("an operator's stream carries no notice", func(t *testing.T) {
		e := setup(t)
		resp := noticeStream(t, e, "/events", "operator")
		defer func() { _ = resp.Body.Close() }()
		publishing(t, e, noticeEvent("1-1"), jobEvent(7))
		if got := readOn(t, resp, "", "event: job-7"); strings.Contains(got, "event: notice") {
			t.Errorf("an operator got a notice: %q", got)
		}
	})
	t.Run("an operator cannot open the notice stream", func(t *testing.T) {
		e := setup(t)
		resp := noticeStream(t, e, "/events?only=notice", "operator")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403", resp.StatusCode)
		}
	})
	t.Run("an admin's notice stream carries the notices alone", func(t *testing.T) {
		e := setup(t)
		resp := noticeStream(t, e, "/events?only=notice", "admin")
		defer func() { _ = resp.Body.Close() }()
		publishing(t, e, jobEvent(7), noticeEvent("1-1"))
		got := readOn(t, resp, "", `"id":"1-1"`)
		if strings.Contains(got, "event: job") {
			t.Errorf("the notice stream carried a job event: %q", got)
		}
	})
	t.Run("a demoted admin hears no more notices", func(t *testing.T) {
		demoted := &atomic.Bool{}
		e := setupWith(t, func(d *Deps) { d.SSERecheck, d.Guard = 30*time.Millisecond, demotableGuard{demoted: demoted} })
		resp := noticeStream(t, e, "/events", "admin")
		defer func() { _ = resp.Body.Close() }()
		stop := publishing(t, e, noticeEvent("1-1"))
		got := readOn(t, resp, "", `"id":"1-1"`)
		stop()
		demoted.Store(true)
		time.Sleep(300 * time.Millisecond) // several re-checks
		publishing(t, e, noticeEvent("2-2"), jobEvent(9))
		from := len(got)
		got = readOn(t, resp, got, "event: job-9")
		if strings.Contains(got[from:], `"id":"2-2"`) {
			t.Errorf("a demoted admin still got a notice: %q", got[from:])
		}
	})
}
