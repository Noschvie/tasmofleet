package ota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// "Send command" runs as a transaction too: one job per device, shown and
// retried like an update. The job is not an OTA job: queued, applying (the
// command is out), then succeeded or failed with the device's reply as the
// note. The command went through the command policy when the job was created
// and goes through it again when it is sent (CommandRunner).

// CommandRunner sends one command to a device on behalf of a principal and
// returns the scrubbed reply (devicecmd.Service).
type CommandRunner interface {
	Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error)
}

// PrincipalResolver says who a stored subject is now: the current role and
// whether the account is still active. It fails for an account that is gone or
// disabled. Local accounts are read from the account store; for single
// sign-on and header users it is the role of their last sign-in.
//
// Subjects are not unique across sign-in methods, so the identity is the pair
// (method, subject); an unknown or ambiguous pair is an error, never a guess.
type PrincipalResolver interface {
	Current(ctx context.Context, method, subject string) (model.Principal, error)
}

type actorKindKey struct{}

// WithActorKind tells the engine which sign-in method the actor of a
// submission used ("local", "oidc", "header"). Command jobs store it.
func WithActorKind(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, actorKindKey{}, method)
}

func actorKind(ctx context.Context) string {
	k, _ := ctx.Value(actorKindKey{}).(string)
	return k
}

// ErrNotAllowed: the user is not (or no longer) an active admin, so a command
// transaction is neither sent nor retried for them.
var ErrNotAllowed = errors.New("ota: only an active admin can send or retry commands")

// currentAdmin resolves subject and requires an active admin. Nothing is
// assumed: no resolver, an unknown, disabled or demoted account all refuse.
func (e *Engine) currentAdmin(ctx context.Context, method, subject string) (model.Principal, error) {
	if e.opts.Principals == nil {
		return model.Principal{}, fmt.Errorf("%w: this server cannot check who started the job", ErrNotAllowed)
	}
	p, err := e.opts.Principals.Current(ctx, method, subject)
	switch {
	case err != nil:
		return model.Principal{}, fmt.Errorf("%w: %w", ErrNotAllowed, err)
	case p.Role < model.RoleAdmin:
		return model.Principal{}, fmt.Errorf("%w: the account is not an admin any more", ErrNotAllowed)
	}
	return p, nil
}

const maxReplyNote = 300

// SubmitCommand creates a transaction that sends one command to each device.
// Devices that already have an active job are returned with the reason and the
// others still start; ErrNothingToFix when none is left.
func (e *Engine) SubmitCommand(ctx context.Context, actor string, reqs []PolicyRequest) (model.BatchID, []Refusal, error) {
	id, skipped, err := e.submitPolicy(ctx, actor, model.KindCommand, reqs, 0)
	if e.audit != nil && err == nil {
		skip := map[model.DeviceID]bool{}
		for _, s := range skipped {
			skip[s.DeviceID] = true
		}
		var devs []model.DeviceID
		for _, r := range reqs {
			if !skip[r.DeviceID] {
				devs = append(devs, r.DeviceID)
			}
		}
		detail, _ := json.Marshal(map[string]any{"devices": devs, "skipped": len(skipped)}) // a plain map always encodes
		e.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "command.bulk.start", Target: "transaction:" + strconv.FormatInt(int64(id), 10), Detail: string(detail), Result: "ok"})
	}
	return id, skipped, err
}

// workCommand drives one command job to its end. A command is not repeated
// after a restart: Power TOGGLE or Restart would act twice, so a job found
// sending is failed with a note and can be retried on purpose.
func (e *Engine) workCommand(ctx context.Context, j model.Job) {
	if j.State == model.JobApplying {
		e.finishCommand(ctx, j, "", "interrupted by a restart; the result is unknown, check the device and retry")
		return
	}
	if e.opts.Commands == nil {
		e.finishCommand(ctx, j, "", "this server cannot send commands")
		return
	}
	var ok bool
	if j, ok = e.policyMove(ctx, j, model.JobApplying, "send", ""); !ok {
		return
	}
	// Who sends it is decided now, not when the job was created: the account
	// may have been demoted or disabled since. The command policy then checks
	// that role again for this very command.
	p, err := e.currentAdmin(ctx, j.CreatedKind, j.CreatedBy)
	if err != nil {
		e.auditCommandRefusal(ctx, j, err)
		e.finishCommand(ctx, j, "", err.Error())
		return
	}
	wait := e.machine().CommandTimeout * policyApplyFactor
	if j.ReplyTimeout > 0 {
		wait = j.ReplyTimeout // bulk "Wait up to 5 s for each response"
	}
	sctx, cancel := context.WithTimeout(ctx, wait)
	raw, err := e.opts.Commands.Run(sctx, p, j.DeviceID, strings.TrimSpace(j.Commands))
	cancel()
	if ctx.Err() != nil {
		return // shutting down: the job resumes as "interrupted"
	}
	if err != nil {
		e.finishCommand(ctx, j, "", err.Error())
		return
	}
	e.finishCommand(ctx, j, compactReply(raw), "")
}

// auditCommandRefusal records that a step was not sent because its creator may
// not send commands any more. The command text is not part of the entry.
func (e *Engine) auditCommandRefusal(ctx context.Context, j model.Job, reason error) {
	if e.audit == nil {
		return
	}
	detail, _ := json.Marshal(map[string]any{"transaction": j.BatchID, "job": j.ID, "reason": reason.Error()}) // a plain map always encodes
	e.audit.Record(ctx, model.AuditEvent{Actor: j.CreatedBy, Action: "command.bulk.refused", Target: "device:" + strconv.FormatInt(int64(j.DeviceID), 10), Detail: string(detail), Result: "refused: " + reason.Error()})
}

func (e *Engine) finishCommand(ctx context.Context, j model.Job, reply, failure string) {
	to, event, detail := model.JobSucceeded, "done", reply
	if failure != "" {
		to, event, detail = model.JobFailed, "failed", failure
	}
	e.policyMove(ctx, j, to, event, detail)
}

// compactReply is the reply on one line, cut to a note's length.
func compactReply(raw json.RawMessage) string {
	var b bytes.Buffer
	s := ""
	if err := json.Compact(&b, raw); err == nil {
		s = b.String()
	} else {
		s = strings.Join(strings.Fields(string(raw)), " ")
	}
	if len(s) > maxReplyNote {
		s = strings.ToValidUTF8(s[:maxReplyNote], "") + "…"
	}
	return s
}
