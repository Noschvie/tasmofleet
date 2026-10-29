package web

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

func openStore(t *testing.T, dir string) *store.SQLite {
	t.Helper()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Why (D20, N21): an admin pauses the automatic live check, e.g. while devices
// are being reflashed. The pause is global and must survive a restart (else
// the checks would quietly resume after an update), and every change must be
// in the audit log with who did it.
func TestPolicyLivePauseSurvivesRestartAndIsAudited(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	aud := &fakeAudit{}
	lp, err := sysconfig.NewLivePause(t.Context(), st, aud)
	if err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, func(d *Deps) { d.Policy = noPolicy{}; d.PolicyLivePause = lp; d.Recorder = aud })
	form := url.Values{"csrf": {"tok"}}

	r, body := e.do(t, "POST", "/policy/live/pause", "admin", form)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/tf/policy?ok=paused" {
		t.Fatalf("pause: %d %q\n%s", r.StatusCode, r.Header.Get("Location"), body)
	}
	if r, _ := e.do(t, "POST", "/policy/live/pause", "admin", form); r.StatusCode != 303 {
		t.Fatalf("pausing again: %d", r.StatusCode)
	}
	_ = st.Close() // "restart": a new service on the reopened database
	st = openStore(t, dir)
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	lp2, err := sysconfig.NewLivePause(t.Context(), st, aud)
	if err != nil {
		t.Fatal(err)
	}
	if s := lp2.State(); !s.Paused || s.By != "test:admin" || s.At.IsZero() {
		t.Fatalf("after restart: %+v", s)
	}
	e2 := setupWith(t, func(d *Deps) { d.Policy = noPolicy{}; d.PolicyLivePause = lp2; d.Recorder = aud })
	if r, _ := e2.do(t, "POST", "/policy/live/resume", "admin", form); r.StatusCode != 303 {
		t.Fatalf("resume: %d", r.StatusCode)
	}
	if lp2.Paused() {
		t.Fatal("still paused after resume")
	}

	var got []string
	aud.mu.Lock()
	for _, ev := range aud.events {
		if ev.Target == "policy:live" {
			got = append(got, ev.Actor+" "+ev.Action+" "+ev.Detail)
		}
	}
	aud.mu.Unlock()
	want := []string{`test:admin policy.live.pause {"paused":true}`, `test:admin policy.live.resume {"paused":false}`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("audit (one entry per change, the repeated pause is no change):\n got %q\nwant %q", got, want)
	}
}

// Why: pausing changes what the whole fleet is checked for; operators may run
// a check but not stop them. (CSRF is enforced for every POST by the guard in
// front of the handler, outside this package.)
func TestPolicyLivePauseNeedsAdmin(t *testing.T) {
	st := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	aud := &fakeAudit{}
	lp, err := sysconfig.NewLivePause(t.Context(), st, aud)
	if err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, func(d *Deps) { d.Policy = noPolicy{}; d.PolicyLivePause = lp })
	if r, _ := e.do(t, "POST", "/policy/live/pause", "operator", url.Values{"csrf": {"tok"}}); r.StatusCode != 403 {
		t.Errorf("operator: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/live/pause", "admin", url.Values{"csrf": {"tok"}, "extra": {"1"}}); r.StatusCode != 400 {
		t.Errorf("unknown field: %d", r.StatusCode)
	}
	if lp.Paused() {
		t.Fatal("a refused request paused the check")
	}
}

// pausePolicy has one rule, so the page draws its live-check line.
type pausePolicy struct{}

func (pausePolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	return PolicyReport{Rules: []policy.Rule{{Command: "SaveData", Want: "0"}}}, nil
}

// Why (N21): the status line is where people see that the automatic check is not running,
// who stopped it and since when; Resume is next to it. Run now stays: it is an on-demand
// check independent of the paused background reads. Viewers see the state but no forms.
func TestPolicyLiveLineShowsThePause(t *testing.T) {
	st := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	aud := &fakeAudit{}
	lp, err := sysconfig.NewLivePause(t.Context(), st, aud)
	if err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, func(d *Deps) { d.Policy = pausePolicy{}; d.PolicyLivePause = lp; d.Recorder = aud })
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, `action="/tf/policy/live/pause"`) || !strings.Contains(body, ">Run now<") || strings.Contains(body, "Resume") {
		t.Fatalf("running: Pause and Run now:\n%s", body)
	}
	r, _ := e.do(t, "POST", "/policy/live/pause", "admin", url.Values{"csrf": {"tok"}})
	_, body = e.do(t, "GET", r.Header.Get("Location")[len("/tf"):], "admin", nil)
	for _, want := range []string{"pol-line-paused", "Live check paused by test:admin at <time", `data-ts="time"`, `action="/tf/policy/live/resume"`, ">Resume<", "Live check paused</span>", ">Run now<"} {
		if !strings.Contains(body, want) {
			t.Errorf("paused page lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/tf/policy/live/pause"`) {
		t.Error("no Pause while paused")
	}
	_, body = e.do(t, "GET", "/policy", "operator", nil)
	if !strings.Contains(body, "Live check paused by") || strings.Contains(body, "/policy/live/resume") || strings.Contains(body, "/policy/live/pause") {
		t.Errorf("operator: the state without the forms:\n%s", body)
	}
	if !strings.Contains(body, `action="/tf/policy/live"`) {
		t.Error("operators may still run a check")
	}
	// without the pause service there are no Pause / Resume buttons at all
	e = setupWith(t, func(d *Deps) { d.Policy = pausePolicy{} })
	if _, body = e.do(t, "GET", "/policy", "admin", nil); strings.Contains(body, "Pause") || !strings.Contains(body, "Run now") {
		t.Error("no pause service: Run now only")
	}
}

// failingPolicy cannot build its report.
type failingPolicy struct{}

func (failingPolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	return PolicyReport{}, errors.New("the device list could not be read")
}

// Why (N3): a live check that fails must not take the page with it: the rules stay visible,
// the line says what failed in red and offers "Retry now", and no deviation count pretends
// to be current.
func TestPolicyLiveLineShowsAFailure(t *testing.T) {
	e, _, _ := rulesEnvDeps(t, func(d *Deps) { d.Policy = failingPolicy{} })
	saveRule(t, e, nil)
	r, body := e.do(t, "GET", "/policy", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, want := range []string{"pol-line-failed", "Live check failed: The device list could not be read.", ">Retry now<", "Flash wear"} {
		if !strings.Contains(body, want) {
			t.Errorf("failed page lacks %q", want)
		}
	}
	if strings.Contains(body, "dlg-fix") {
		t.Error("no Make compliant without a report")
	}
}
