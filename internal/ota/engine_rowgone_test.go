package ota

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why (#466): a job whose row vanished (the device was deleted) is not a fault of the engine. The
// engine must stop quietly (WARN), while a genuine conflict (the row is in another state) stays an
// ERROR, because then two writers disagree and somebody should read about it.

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// failingSave is a job store whose SaveTransition always fails with err.
type failingSave struct {
	*fakeStore
	err error
}

func (f failingSave) SaveTransition(context.Context, model.Job, model.JobEvent) error { return f.err }

func rowGoneEngine(t *testing.T, err error) (*Engine, *lockedBuf) {
	t.Helper()
	h := newHarness(t, 1, plugDevice())
	buf := &lockedBuf{}
	h.e.log = slog.New(slog.NewTextHandler(buf, nil))
	h.e.jobs = failingSave{h.st, err}
	return h.e, buf
}

func queuedJob() model.Job {
	return model.Job{ID: 7, DeviceID: 1, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
}

// persistSites runs each of the three places that persist a transition against a failing store and
// returns what they logged.
func persistSites(t *testing.T, err error) map[string]string {
	t.Helper()
	out := map[string]string{}
	run := func(name string, f func(e *Engine) bool) {
		e, buf := rowGoneEngine(t, err)
		if f(e) {
			t.Errorf("%s: reported success although the transition was not stored", name)
		}
		out[name] = buf.String()
	}
	ctx := context.Background()
	run("apply", func(e *Engine) bool { _, _, ok := e.apply(ctx, queuedJob(), Event{Kind: EvStart}); return ok })
	run("policy", func(e *Engine) bool {
		_, ok := e.policyMove(ctx, queuedJob(), model.JobApplying, "start", "")
		return ok
	})
	run("template", func(e *Engine) bool { return e.templateEvent(ctx, queuedJob(), "baseline", "x") })
	return out
}

func TestVanishedJobRowStopsQuietly(t *testing.T) {
	for site, logged := range persistSites(t, store.ErrNotFound) {
		if strings.Contains(logged, "level=ERROR") {
			t.Errorf("%s: a job row that is gone (device deleted) must not log an ERROR:\n%s", site, logged)
		}
		if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "job=7") {
			t.Errorf("%s: want one WARN naming the job:\n%s", site, logged)
		}
	}
}

func TestConflictingJobRowStaysAnError(t *testing.T) {
	conflict := store.ErrConflict
	for site, logged := range persistSites(t, conflict) {
		if !strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "level=WARN") {
			t.Errorf("%s: a conflict is two writers disagreeing and must stay an ERROR:\n%s", site, logged)
		}
	}
}
