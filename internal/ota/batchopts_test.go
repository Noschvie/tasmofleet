package ota

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// peakRunner measures how many commands are in flight at once and the time
// each send was given. A send holds its slot until `want` sends are in flight
// (or a short real-time grace ends), so a scheduler that would allow more is
// caught by the peak.
type peakRunner struct {
	mu       sync.Mutex
	inflight int
	peak     int
	want     int
	budgets  []time.Duration
}

func (p *peakRunner) Run(ctx context.Context, _ model.Principal, _ model.DeviceID, _ string) (json.RawMessage, error) {
	p.mu.Lock()
	p.inflight++
	p.peak = max(p.peak, p.inflight)
	if dl, ok := ctx.Deadline(); ok {
		p.budgets = append(p.budgets, time.Until(dl))
	}
	p.mu.Unlock()
	end := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(end) {
		p.mu.Lock()
		reached := p.inflight >= p.want
		p.mu.Unlock()
		if reached {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
	return json.RawMessage(`{"Restart":"Restarting"}`), nil
}

func optsHarness(t *testing.T, n int, want int) (*harness, *peakRunner) {
	t.Helper()
	h, _, _, _ := commandHarness(t, n) // fleet-wide MaxParallel 2
	run := &peakRunner{want: want}
	h.e.opts.Commands = run
	return h, run
}

func reqs(n int, cmd string) []PolicyRequest {
	out := make([]PolicyRequest, n)
	for i := range out {
		out[i] = PolicyRequest{DeviceID: model.DeviceID(i + 1), Commands: []string{cmd}}
	}
	return out
}

// Why (design v2, N15): "One device at a time" must never let two devices of
// the transaction restart together, also while the fleet limit would allow it.
func TestBatchParallelOneRunsDevicesOneByOne(t *testing.T) {
	h, run := optsHarness(t, 4, 2)
	ctx := WithBatchOptions(as("local"), BatchOptions{Parallel: 1})
	if _, _, err := h.e.SubmitCommand(ctx, "alice", reqs(4, "Restart 1")); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	if run.peak != 1 {
		t.Fatalf("peak %d devices at once, want 1", run.peak)
	}
}

// Why: "Devices at a time: 3" restarts three at once although the fleet-wide
// limit (2 here) is lower: that limit guards firmware downloads, the bulk cap
// is the admin's explicit choice and is bounded by MaxBatchParallel.
func TestBatchParallelThreeOverridesTheFleetLimit(t *testing.T) {
	h, run := optsHarness(t, 6, 3)
	ctx := WithBatchOptions(as("local"), BatchOptions{Parallel: 3})
	if _, _, err := h.e.SubmitCommand(ctx, "alice", reqs(6, "Restart 1")); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	if run.peak != 3 {
		t.Fatalf("peak %d devices at once, want 3", run.peak)
	}
}

// Why: without options nothing changes: the fleet-wide limit still applies.
func TestNoBatchOptionsKeepsTheFleetLimit(t *testing.T) {
	h, run := optsHarness(t, 4, 4)
	if _, _, err := h.e.SubmitCommand(as("local"), "alice", reqs(4, "Restart 1")); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	if run.peak != 2 {
		t.Fatalf("peak %d devices at once, want the fleet limit 2", run.peak)
	}
}

// Why: "Wait up to 5 s for each response" bounds each device's send; without
// it the default (several command timeouts) applies. A cap above
// MaxBatchParallel is clamped, never taken as "unlimited".
func TestReplyTimeoutAndClamp(t *testing.T) {
	h, run := optsHarness(t, 2, 1)
	ctx := WithBatchOptions(as("local"), BatchOptions{Parallel: 99, ReplyTimeout: 5 * time.Second})
	id, _, err := h.e.SubmitCommand(ctx, "alice", reqs(1, "Status 5"))
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if jobs[0].BatchParallel != MaxBatchParallel || jobs[0].ReplyTimeout != 5*time.Second {
		t.Fatalf("stored options: parallel %d, timeout %v", jobs[0].BatchParallel, jobs[0].ReplyTimeout)
	}
	if _, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 2, Commands: []string{"Status 5"}}}); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	if len(run.budgets) != 2 || run.budgets[0] > 5*time.Second || run.budgets[1] <= 5*time.Second {
		t.Fatalf("send budgets %v, want <= 5s with the option and the longer default without", run.budgets)
	}
}

// Why: "Retry failed" repeats the transaction as it was asked for; a retry
// that dropped "One device at a time" would restart the failed devices together.
func TestRetryKeepsBatchOptions(t *testing.T) {
	h, run, _, _ := commandHarness(t, 2)
	run.err = context.DeadlineExceeded
	ctx := WithBatchOptions(as("local"), BatchOptions{Parallel: 1, ReplyTimeout: 5 * time.Second})
	orig, _, err := h.e.SubmitCommand(ctx, "alice", reqs(2, "Restart 1"))
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	run.err = nil
	res, err := h.e.SubmitRetry(as("local"), "alice", orig, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	for _, j := range jobs {
		if j.BatchParallel != 1 || j.ReplyTimeout != 5*time.Second {
			t.Fatalf("retry job options: parallel %d, timeout %v", j.BatchParallel, j.ReplyTimeout)
		}
	}
	h.run(t, h.e)
}
