package ota

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeRunner records what the engine sent and as whom.
type fakeRunner struct {
	mu    sync.Mutex
	sent  []string
	roles []model.Role
	err   error
	reply string
}

func (f *fakeRunner) Run(_ context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p.Subject+"|"+string(rune('0'+id))+"|"+cmd)
	f.roles = append(f.roles, p.Role)
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.reply), nil
}

// fakePrincipals is the account directory: role by subject, missing = gone,
// disabled = refused.
type fakePrincipals struct {
	mu       sync.Mutex
	roles    map[string]model.Role
	disabled map[string]bool
}

func (f *fakePrincipals) Current(_ context.Context, method, subject string) (model.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method != "local" {
		return model.Principal{}, errors.New("unknown method")
	}
	if f.disabled[subject] {
		return model.Principal{}, errors.New("the account is disabled")
	}
	r, ok := f.roles[subject]
	if !ok {
		return model.Principal{}, errors.New("the account is gone")
	}
	return model.Principal{Subject: subject, Role: r, Method: method}, nil
}

// as submits like the web layer does: with the creator's sign-in method.
func as(method string) context.Context { return WithActorKind(context.Background(), method) }

func commandHarness(t *testing.T, n int) (*harness, *fakeRunner, *fakePrincipals, *auditRec) {
	t.Helper()
	h := newHarness(t, 2, failingDevices(n)...)
	run := &fakeRunner{reply: `{"Power":"ON"}`}
	pr := &fakePrincipals{roles: map[string]model.Role{"alice": model.RoleAdmin, "bob": model.RoleOperator}, disabled: map[string]bool{}}
	aud := &auditRec{}
	h.e.opts.Commands, h.e.opts.Principals, h.e.audit = run, pr, aud
	return h, run, pr, aud
}

// Why: a command job sends the stored command once, as the current principal of
// its creator, and keeps the (compact) reply as the note of the step.
func TestCommandJobSendsOnceAndKeepsTheReply(t *testing.T) {
	h, run, _, aud := commandHarness(t, 2)
	id, skipped, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"Power1 ON"}}, {DeviceID: 2, Commands: []string{"Power1 ON"}}})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", err, skipped)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 2 || !jobs[0].IsCommand() || jobs[0].State != model.JobSucceeded || jobs[1].State != model.JobSucceeded {
		t.Fatalf("jobs %+v", jobs)
	}
	if len(run.sent) != 2 || run.roles[0] != model.RoleAdmin {
		t.Fatalf("sent %v roles %v", run.sent, run.roles)
	}
	var note string
	h.st.mu.Lock()
	for _, ev := range h.st.events {
		if ev.To == model.JobSucceeded && ev.JobID == jobs[0].ID {
			note = ev.Detail
		}
	}
	h.st.mu.Unlock()
	if note != `{"Power":"ON"}` {
		t.Fatalf("reply note %q", note)
	}
	var started bool
	for _, e := range aud.evs {
		started = started || e.Action == "command.bulk.start"
	}
	if !started {
		t.Fatalf("the start must be audited: %+v", aud.evs)
	}
}

// Why (security review): the role is decided when the step is sent, not when
// the job was created. A creator who was demoted or disabled in between must
// not get an admin command out, and the refusal is audited.
func TestCommandStepFailsWhenTheCreatorIsDemotedDisabledOrGone(t *testing.T) {
	for name, change := range map[string]func(*fakePrincipals){
		"demoted":  func(p *fakePrincipals) { p.roles["alice"] = model.RoleOperator },
		"disabled": func(p *fakePrincipals) { p.disabled["alice"] = true },
		"gone":     func(p *fakePrincipals) { delete(p.roles, "alice") },
	} {
		t.Run(name, func(t *testing.T) {
			h, run, pr, aud := commandHarness(t, 1)
			id, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Reset 1"}}})
			if err != nil {
				t.Fatal(err)
			}
			change(pr) // between create and send
			h.run(t, h.e)
			jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
			if len(jobs) != 1 || jobs[0].State != model.JobFailed || !strings.Contains(jobs[0].LastError, "admin") {
				t.Fatalf("job %+v", jobs)
			}
			if len(run.sent) != 0 {
				t.Fatalf("nothing may be sent: %v", run.sent)
			}
			var refused bool
			for _, e := range aud.evs {
				refused = refused || (e.Action == "command.bulk.refused" && e.Actor == "alice" && strings.HasPrefix(e.Result, "refused"))
			}
			if !refused {
				t.Fatalf("the refusal must be audited: %+v", aud.evs)
			}
		})
	}
}

// Why: without a way to check the creator the engine must not guess an admin.
func TestCommandStepWithoutAPrincipalResolverFails(t *testing.T) {
	h, run, _, _ := commandHarness(t, 1)
	h.e.opts.Principals = nil
	h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Power1 ON"}}}) //nolint:errcheck // checked through the job state
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || len(run.sent) != 0 {
		t.Fatalf("%+v sent %v", j, run.sent)
	}
}

// Why: Power TOGGLE or Restart would act twice if the engine re-sent after a
// restart, so a job found mid-send fails with a note and is retried on purpose.
func TestCommandJobFoundSendingIsNotSentAgain(t *testing.T) {
	h, run, _, _ := commandHarness(t, 1)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindCommand, Commands: "Power1 TOGGLE", State: model.JobApplying, CreatedBy: "alice", CreatedKind: "local"})
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || !strings.Contains(j.LastError, "restart") || len(run.sent) != 0 {
		t.Fatalf("%+v sent %v", j, run.sent)
	}
}

// Why: a failing device is reported on its own job; the others are unaffected.
func TestCommandJobFailureKeepsTheDeviceError(t *testing.T) {
	h, run, _, _ := commandHarness(t, 1)
	run.err = errors.New("the device is unreachable")
	h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Status 2"}}}) //nolint:errcheck // checked through the job state
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || !strings.Contains(j.LastError, "unreachable") {
		t.Fatalf("%+v", j)
	}
}

// Why (security review): a retry runs as the retrying user. An operator is
// refused, an admin retries the same command as themselves, and a command the
// policy does not allow for that role is refused instead of sent.
func TestCommandRetryRunsAsTheRetryingAdminOnly(t *testing.T) {
	h, run, _, _ := commandHarness(t, 2)
	run.err = errors.New("boom")
	orig, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Reset 1"}}, {DeviceID: 2, Commands: []string{"Power1 ON"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	run.err = nil
	if _, err := h.e.SubmitRetry(as("local"), "bob", orig, nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("an operator's retry must be refused: %v", err)
	}
	if _, err := h.e.SubmitRetry(as("local"), "nobody", orig, nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("an unknown user's retry must be refused: %v", err)
	}
	sentBefore := len(run.sent)
	res, err := h.e.SubmitRetry(as("local"), "alice", orig, nil)
	if err != nil || len(res.Devices) != 2 {
		t.Fatalf("admin retry: %+v %v", res, err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 2 || jobs[0].CreatedBy != "alice" || !jobs[0].IsCommand() {
		t.Fatalf("retry jobs %+v", jobs)
	}
	got := strings.Join(run.sent[sentBefore:], ",")
	if len(run.sent) != sentBefore+2 || !strings.Contains(got, "alice|1|Reset 1") || !strings.Contains(got, "alice|2|Power1 ON") {
		t.Fatalf("retry sent %v", run.sent[sentBefore:])
	}
}

// Why: a stored command that the policy refuses for the retrying principal (a
// line that became malformed, say) is refused at retry, not sent.
func TestCommandRetryChecksEveryStoredCommandAgainstThePolicy(t *testing.T) {
	h, run, _, _ := commandHarness(t, 1)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindCommand, Commands: "Power1 ON;Reset 1", State: model.JobFailed, CreatedBy: "alice", CreatedKind: "local", BatchID: 7})
	_, err := h.e.SubmitRetry(as("local"), "alice", 7, nil)
	var refused *RefusedError
	if !errors.As(err, &refused) || len(run.sent) != 0 {
		t.Fatalf("err %v sent %v", err, run.sent)
	}
}

// Why: a command job without a stored sign-in method (every job from before the
// method was kept) or with one nobody knows never resolves to a principal.
func TestCommandStepWithoutASignInMethodFails(t *testing.T) {
	h, run, _, _ := commandHarness(t, 1)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindCommand, Commands: "Power1 ON", State: model.JobQueued, CreatedBy: "alice"})
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || len(run.sent) != 0 {
		t.Fatalf("%+v sent %v", j, run.sent)
	}
}

// Why: the method of the submitting request is stored with the job.
func TestCommandJobStoresTheCreatorsSignInMethod(t *testing.T) {
	h, _, _, _ := commandHarness(t, 1)
	id, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{{DeviceID: 1, Commands: []string{"Power1 ON"}}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 1 || jobs[0].CreatedKind != "local" {
		t.Fatalf("%+v", jobs)
	}
}

// Why: bulk "Restart" is the single-device restart ("Restart 1") sent through
// the command runner (console service: command policy, device lock, audit) as
// the creating admin. A retry sends the very same line, never a stronger one.
func TestBulkRestartGoesThroughTheCommandRunnerAsTheAdmin(t *testing.T) {
	h, run, _, aud := commandHarness(t, 2)
	id, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{
		{DeviceID: 1, Commands: []string{"Restart 1"}}, {DeviceID: 2, Commands: []string{"Restart 1"}}})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	got := strings.Join(run.sent, ",")
	if len(run.sent) != 2 || !strings.Contains(got, "alice|1|Restart 1") || !strings.Contains(got, "alice|2|Restart 1") || run.roles[0] != model.RoleAdmin {
		t.Fatalf("sent %v roles %v", run.sent, run.roles)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 2 || jobs[0].State != model.JobSucceeded {
		t.Fatalf("%+v", jobs)
	}
	if len(aud.evs) == 0 || aud.evs[0].Action != "command.bulk.start" {
		t.Fatalf("audit %+v", aud.evs)
	}
}
